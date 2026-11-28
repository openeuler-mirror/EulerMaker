import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import { parse } from "@vue/compiler-sfc";
import { renderToString } from "@vue/server-renderer";
import * as Vue from "vue";
import ts from "typescript";

const source = readFileSync(new URL("../src/views/MachineAccountsView.vue", import.meta.url), "utf8");
const { descriptor } = parse(source);
const transpile = code => ts.transpileModule(code, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
}).outputText;
const script = transpile(descriptor.scriptSetup.content.replace(/^import .*;\n/gm, ""));
const render = new Function("Vue", transpile(compile(descriptor.template.content, {
  mode: "function", prefixIdentifiers: true, expressionPlugins: ["typescript"],
}).code))(Vue);

function setup() {
  const calls = [];
  const transport = { request: async () => ({ name: "worker" }) };
  const deps = {
    computed: Vue.computed, ref: Vue.ref, onMounted: () => {}, onBeforeUnmount: () => {},
    useI18n: () => ({ t: key => key }), ApiError: class extends Error { constructor(status) { super("request failed"); this.status = status; } },
    errorTranslationKey: (_, fallback) => fallback,
    list: async () => ({ items: [], next: "" }),
    request: async (path, options) => {
      calls.push({ path, ...options, body: JSON.parse(options.body) });
      return transport.request();
    },
  };
  const state = new Function(...Object.keys(deps), script + `
    return { t, accounts, typeFilter, filteredAccounts, loading, loadError, dialogError, success, page, nextToken, createOpen, name,
      accountType, typeOptions, projectNames, permissions, permissionOptions, ttl, saving, secret, createdName, deleting, confirmName, openCreate,
      closeCreate, closeSecret, openDelete, closeDelete, loadAccounts, reload, changePage,
      createAccount, deleteAccount, formatDate, editing, openEdit, updateAccount };
  `)(...Object.values(deps));
  const html = async () => {
    const app = Vue.createSSRApp({ setup: () => state, render });
    app.component("ModalDialog", {
      props: ["titleId", "title", "closeLabel"], emits: ["close"],
      setup: (_, { slots }) => () => slots.default?.(),
    });
    app.component("AppSelect", {
      props: ["options", "modelValue"],
      setup: props => () => Vue.h("select", props.options.map(option =>
        Vue.h("option", { value: option.value, selected: props.modelValue === option.value }, option.label))),
    });
    app.component("ColumnMultiFilter", {
      props: ["options", "modelValue", "label", "allLabel"],
      setup: props => () => Vue.h("button", { "aria-label": props.label }, props.allLabel),
    });
    for (const name of ["EmptyState", "ArrowLeft", "ArrowRight", "CircleCheckFilled", "Refresh", "WarningFilled"]) {
      app.component(name, { render: () => null });
    }
    return renderToString(app);
  };
  return { state, calls, transport, html, ApiError: deps.ApiError };
}

test("machine account type header filters current-page accounts and stays visible for empty results", async () => {
  const { state, html, calls } = setup();
  state.loading.value = false;
  state.accounts.value = [
    { metadata: { name: "worker" }, spec: { type: "runner" } },
    { metadata: { name: "sync" }, spec: { type: "service" } },
    { metadata: { name: "legacy" }, spec: {} },
  ];
  const names = () => state.filteredAccounts.value.map(account => account.metadata.name);
  assert.deepEqual(names(), ["worker", "sync", "legacy"]);
  state.typeFilter.value = ["runner"];
  assert.deepEqual(names(), ["worker", "legacy"]);
  state.typeFilter.value = ["service"];
  assert.deepEqual(names(), ["sync"]);
  const filtered = await html();
  assert.match(filtered, /<th><button aria-label="admin.machineType"/);
  assert.match(filtered, /admin.allMachineTypes/);
  assert.doesNotMatch(filtered, /<strong>worker<\/strong>/);
  state.typeFilter.value = ["runner", "service"];
  assert.equal(names().length, 3);
  state.accounts.value = [];
  const empty = await html();
  assert.match(empty, /<th><button aria-label="admin.machineType"/);
  assert.match(empty, /<td colspan="5">/);
  state.typeFilter.value = [];
  assert.deepEqual(names(), []);
  assert.equal(calls.length, 0);
  for (const locale of ["zh-CN", "en-US"]) {
    const messages = readFileSync(new URL(`../src/i18n/messages/${locale}.ts`, import.meta.url), "utf8");
    assert.match(messages, /allMachineTypes: "[^"]+"/);
  }
});

test("machine account form defaults to Runner and resets when reopened", async () => {
  const { state, html } = setup();
  state.openCreate();
  assert.equal(state.accountType.value, "runner");
  assert.deepEqual(state.typeOptions.value.map(option => option.value), ["runner", "service"]);
  assert.match(await html(), /value="runner" selected/);
  state.accountType.value = "service";
  state.projectNames.value = "demo";
  state.permissions.value = ["builds:abort"];
  const serviceForm = await html();
  assert.match(serviceForm, /admin.serviceAccountHint/);
  assert.match(serviceForm, /admin.serviceProjects/);
  assert.match(serviceForm, /admin.servicePermissions/);
  for (const permission of ["projects:update", "builds:create", "builds:abort"]) {
    assert.match(serviceForm, new RegExp(`type="checkbox" value="${permission}"`));
  }
  state.closeCreate();
  state.openCreate();
  assert.equal(state.accountType.value, "runner");
  assert.equal(state.projectNames.value, "");
  assert.deepEqual(state.permissions.value, []);
  assert.doesNotMatch(await html(), /admin.serviceAccountHint/);
  assert.doesNotMatch(await html(), /admin.serviceProjects|admin.servicePermissions/);
});

test("creation sends the selected account type with a one-time client secret", async () => {
  for (const type of ["runner", "service"]) {
    const { state, calls } = setup();
    state.openCreate();
    state.name.value = "worker";
    state.accountType.value = type;
    await state.createAccount();
    assert.equal(calls.length, 1);
    assert.equal(calls[0].path, "/auth/machineaccounts");
    assert.equal(calls[0].method, "POST");
    assert.deepEqual(calls[0].body, {
      name: "worker", type, tokenTTLSeconds: 3600, clientSecret: state.secret.value,
      ...(type === "service" ? { projects: [], permissions: [] } : {}),
    });
    assert.equal(Buffer.from(state.secret.value, "base64url").length, 32);
    assert.equal(state.createdName.value, "worker");
    assert.equal(state.createOpen.value, false);
    state.closeSecret();
    assert.equal(state.secret.value, "");
  }
});

test("failed creation preserves the selected type for retry", async () => {
  const { state, transport } = setup();
  state.openCreate();
  state.name.value = "service-account";
  state.accountType.value = "service";
  state.projectNames.value = "demo, future-project";
  state.permissions.value = ["projects:update"];
  transport.request = async () => { throw new Error("unavailable"); };
  await state.createAccount();
  assert.equal(state.accountType.value, "service");
  assert.equal(state.createOpen.value, true);
  assert.equal(state.secret.value, "");
  assert.equal(state.dialogError.value, "admin.createMachineFailed");
  assert.equal(state.saving.value, false);
  assert.equal(state.projectNames.value, "demo, future-project");
  assert.deepEqual(state.permissions.value, ["projects:update"]);
});

test("Service creation submits exact projects and independent permissions", async () => {
  for (const permissions of [["projects:update"], ["builds:create"], ["builds:abort"], ["projects:update", "builds:create", "builds:abort"]]) {
    const { state, calls } = setup();
    state.openCreate();
    state.name.value = "automation";
    state.accountType.value = "service";
    state.projectNames.value = "  demo,\n future-project，other-project  ";
    state.permissions.value = permissions;
    await state.createAccount();
    assert.equal(calls.length, 1);
    assert.deepEqual(calls[0].body.projects, ["demo", "future-project", "other-project"]);
    assert.deepEqual(calls[0].body.permissions, permissions);
  }
});

test("empty project or permission list is allowed without implicit grants", async () => {
  for (const [projects, permissions] of [["", ["builds:create"]], ["demo", []]]) {
    const { state, calls } = setup();
    state.openCreate();
    state.name.value = "automation";
    state.accountType.value = "service";
    state.projectNames.value = projects;
    state.permissions.value = permissions;
    await state.createAccount();
    assert.equal(calls.length, 1);
    assert.deepEqual(calls[0].body.projects, projects ? [projects] : []);
    assert.deepEqual(calls[0].body.permissions, permissions);
  }
});

test("Service accounts require standalone * scope on creation and update", async () => {
  for (const projects of ["*", "*, demo", "demo, *"]) {
    for (const edit of [false, true]) {
      const { state, calls } = setup();
      if (edit) {
        state.openEdit(serviceAccount());
      } else {
        state.openCreate();
        state.name.value = "automation";
        state.accountType.value = "service";
      }
      state.projectNames.value = projects;
      state.permissions.value = ["builds:create"];
      await (edit ? state.updateAccount() : state.createAccount());
      if (projects !== "*") {
        assert.equal(calls.length, 0);
        assert.equal(state.dialogError.value, "admin.serviceProjectsWildcardExclusive");
        continue;
      }
      assert.equal(calls.length, 1);
      const grants = edit ? calls[0].body.spec : calls[0].body;
      assert.deepEqual(grants.projects, projects.split(", "));
      assert.deepEqual(grants.permissions, ["builds:create"]);
    }
  }
});

test("invalid or duplicate grants are rejected before registration", async () => {
  const cases = [
    ["demo-*", [], "admin.invalidServiceProjects"],
    ["*, *", [], "admin.serviceProjectsWildcardExclusive"],
    ["Demo", [], "admin.invalidServiceProjects"],
    ["ALL", [], "admin.invalidServiceProjects"],
    ["demo/distro", [], "admin.invalidServiceProjects"],
    ["a".repeat(64), [], "admin.invalidServiceProjects"],
    ["demo, demo", [], "admin.invalidServiceProjects"],
    ["demo", ["builds:delete"], "admin.invalidServicePermissions"],
    ["demo", ["builds:abort", "builds:abort"], "admin.invalidServicePermissions"],
  ];
  for (const [projects, permissions, error] of cases) {
    const { state, calls } = setup();
    state.openCreate();
    state.name.value = "automation";
    state.accountType.value = "service";
    state.projectNames.value = projects;
    state.permissions.value = permissions;
    await state.createAccount();
    assert.equal(calls.length, 0);
    assert.equal(state.dialogError.value, error);
    assert.equal(state.secret.value, "");
  }
});

test("switching to Runner omits Service grants from the request", async () => {
  const { state, calls } = setup();
  state.openCreate();
  state.name.value = "worker";
  state.accountType.value = "service";
  state.projectNames.value = "demo";
  state.permissions.value = ["builds:abort"];
  state.accountType.value = "runner";
  await state.createAccount();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].body.type, "runner");
  assert.equal(Object.hasOwn(calls[0].body, "projects"), false);
  assert.equal(Object.hasOwn(calls[0].body, "permissions"), false);
});

test("Service grant labels and hints are available in Chinese and English", () => {
  for (const locale of ["zh-CN", "en-US"]) {
    const source = readFileSync(new URL(`../src/i18n/messages/${locale}.ts`, import.meta.url), "utf8");
    const admin = new Function(`return ({${source.match(/  admin: \{([\s\S]*?)\n  \},/)[1]}});`)();
    for (const key of ["serviceAccountHint", "serviceProjects", "serviceProjectsPlaceholder", "serviceProjectsHint", "servicePermissions", "servicePermissionsHint", "permissionProjectsUpdate", "permissionBuildsCreate", "permissionBuildsAbort", "invalidServiceProjects", "invalidServicePermissions", "editMachineGrants", "machineGrantsEditHint", "savingMachineGrants", "machineGrantsSaved", "saveMachineGrantsFailed", "machineGrantsConflict", "tokenTTLUpdateHint", "invalidTokenTTL"]) {
      assert.equal(typeof admin[key], "string", `${locale}: ${key}`);
      assert.ok(admin[key].length > 0, `${locale}: ${key}`);
    }
  }
});

test("account list displays Runner and Service types", async () => {
  const { state, html } = setup();
  state.loading.value = false;
  state.accounts.value = [
    { metadata: { name: "runner-account" }, spec: { type: "runner" } },
    { metadata: { name: "service-account" }, spec: { type: "service" } },
  ];
  const result = await html();
  assert.match(result, /<th><button aria-label="admin.machineType"/);
  assert.match(result, /<td>runner<\/td>/);
  assert.match(result, /<td>service<\/td>/);
  assert.equal((result.match(/admin.editMachineGrants/g) || []).length, 2);
});

const serviceAccount = () => ({
  metadata: { name: "automation", uid: "original-id", resourceVersion: "v1:3:1" },
  spec: { type: "service", tokenTTLSeconds: 3600, projects: ["demo"], permissions: ["projects:update"] },
});

test("editing Service settings submits lifetime, grants and concurrency guards without rotating credentials", async () => {
  const { state, calls, html } = setup();
  const account = serviceAccount();
  state.openEdit(account);
  assert.equal(state.projectNames.value, "demo");
  assert.deepEqual(state.permissions.value, ["projects:update"]);
  assert.match(await html(), /admin.machineGrantsEditHint/);
  state.projectNames.value = "other-project";
  state.permissions.value = ["builds:abort"];
  state.ttl.value = 7200;
  await state.updateAccount();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/apis/iam.ebs/v1/machineaccounts/automation");
  assert.equal(calls[0].method, "PATCH");
  assert.equal(calls[0].headers["Content-Type"], "application/merge-patch+json");
  assert.deepEqual(calls[0].body, {
    metadata: { uid: "original-id", resourceVersion: "v1:3:1" },
    spec: { tokenTTLSeconds: 7200, projects: ["other-project"], permissions: ["builds:abort"] },
  });
  assert.deepEqual(account, serviceAccount());
  assert.equal(state.createOpen.value, false);
  assert.equal(state.secret.value, "");
  assert.equal(state.success.value, "admin.machineGrantsSaved");
});

test("grant update failures retain the draft and do not retry with a newer version", async () => {
  for (const status of [409, 404, 503]) {
    const { state, calls, transport, ApiError } = setup();
    state.openEdit(serviceAccount());
    state.permissions.value = [];
    transport.request = async () => { throw new ApiError(status); };
    await state.updateAccount();
    assert.equal(calls.length, 1);
    assert.equal(state.createOpen.value, true);
    assert.deepEqual(state.permissions.value, []);
    assert.equal(state.editing.value.metadata.resourceVersion, "v1:3:1");
    assert.equal(state.dialogError.value, status === 503 ? "admin.saveMachineGrantsFailed" : "admin.machineGrantsConflict");
    assert.equal(state.saving.value, false);
  }
});

test("edit validation and cancel preserve the list; create resets edit state", async () => {
  const { state, calls } = setup();
  const account = serviceAccount();
  state.openEdit(account);
  state.projectNames.value = "demo-*";
  await state.updateAccount();
  assert.equal(calls.length, 0);
  assert.equal(state.dialogError.value, "admin.invalidServiceProjects");
  state.closeCreate();
  assert.deepEqual(account, serviceAccount());
  state.openCreate();
  assert.equal(state.editing.value, null);
  assert.equal(state.projectNames.value, "");
  assert.deepEqual(state.permissions.value, []);
});

test("Runner editing updates only lifetime and keeps service fields hidden", async () => {
  const { state, calls, html } = setup();
  state.openEdit({ metadata: { name: "worker", uid: "id", resourceVersion: "v1:1:1" }, spec: { type: "runner", tokenTTLSeconds: 3600 } });
  assert.equal(state.accountType.value, "runner");
  const form = await html();
  assert.doesNotMatch(form, /admin.serviceProjects|admin.servicePermissions/);
  assert.match(form, /admin.tokenTTLUpdateHint/);
  assert.doesNotMatch(form, /type="number"[^>]*readonly/);
  state.ttl.value = 300;
  await state.updateAccount();
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].body, { metadata: { uid: "id", resourceVersion: "v1:1:1" }, spec: { tokenTTLSeconds: 300 } });
});

test("lifetime updates validate integer bounds for Runner and Service accounts", async () => {
  for (const type of ["runner", "service"]) {
    for (const ttl of [0, 299, 300, 7200, 86400, 86401, 300.5, "300"]) {
      const { state, calls } = setup();
      state.openEdit({ metadata: serviceAccount().metadata, spec: { type, tokenTTLSeconds: 3600 } });
      state.ttl.value = ttl;
      await state.updateAccount();
      const valid = Number.isInteger(ttl) && ttl >= 300 && ttl <= 86400;
      assert.equal(calls.length, valid ? 1 : 0, `${type}: ${ttl}`);
      if (!valid) {
        assert.equal(state.dialogError.value, "admin.invalidTokenTTL");
        assert.equal(state.createOpen.value, true);
      }
    }
  }
});
