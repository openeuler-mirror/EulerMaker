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
    useI18n: () => ({ t: key => key }), ApiError: class extends Error {},
    errorTranslationKey: (_, fallback) => fallback,
    list: async () => ({ items: [], next: "" }),
    request: async (path, options) => {
      calls.push({ path, ...options, body: JSON.parse(options.body) });
      return transport.request();
    },
  };
  const state = new Function(...Object.keys(deps), script + `
    return { t, accounts, loading, loadError, dialogError, success, page, nextToken, createOpen, name,
      accountType, typeOptions, ttl, saving, secret, createdName, deleting, confirmName, openCreate,
      closeCreate, closeSecret, openDelete, closeDelete, loadAccounts, reload, changePage,
      createAccount, deleteAccount, formatDate };
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
    for (const name of ["EmptyState", "ArrowLeft", "ArrowRight", "CircleCheckFilled", "Refresh", "WarningFilled"]) {
      app.component(name, { render: () => null });
    }
    return renderToString(app);
  };
  return { state, calls, transport, html };
}

test("machine account form defaults to Runner and resets when reopened", async () => {
  const { state, html } = setup();
  state.openCreate();
  assert.equal(state.accountType.value, "runner");
  assert.deepEqual(state.typeOptions.value.map(option => option.value), ["runner", "service"]);
  assert.match(await html(), /value="runner" selected/);
  state.accountType.value = "service";
  assert.match(await html(), /admin.serviceAccountHint/);
  state.closeCreate();
  state.openCreate();
  assert.equal(state.accountType.value, "runner");
  assert.doesNotMatch(await html(), /admin.serviceAccountHint/);
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
  transport.request = async () => { throw new Error("unavailable"); };
  await state.createAccount();
  assert.equal(state.accountType.value, "service");
  assert.equal(state.createOpen.value, true);
  assert.equal(state.secret.value, "");
  assert.equal(state.dialogError.value, "admin.createMachineFailed");
  assert.equal(state.saving.value, false);
});

test("account list displays Runner and Service types", async () => {
  const { state, html } = setup();
  state.loading.value = false;
  state.accounts.value = [
    { metadata: { name: "runner-account" }, spec: { type: "runner" } },
    { metadata: { name: "service-account" }, spec: { type: "service" } },
  ];
  const result = await html();
  assert.match(result, /<th>admin.machineType<\/th>/);
  assert.match(result, /<td>runner<\/td>/);
  assert.match(result, /<td>service<\/td>/);
});
