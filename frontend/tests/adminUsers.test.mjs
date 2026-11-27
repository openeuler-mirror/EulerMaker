import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import { parse } from "@vue/compiler-sfc";
import { renderToString } from "@vue/server-renderer";
import * as Vue from "vue";
import ts from "typescript";

const source = readFileSync(new URL("../src/views/AdminUsersView.vue", import.meta.url), "utf8");
const { descriptor } = parse(source);
const transpile = code => ts.transpileModule(code, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
}).outputText;
const script = transpile(descriptor.scriptSetup.content.replace(/^import .*;\n/gm, ""));
const render = new Function("Vue", transpile(compile(descriptor.template.content, {
  mode: "function", prefixIdentifiers: true, expressionPlugins: ["typescript"],
}).code))(Vue);

function setup() {
  const deps = {
    computed: Vue.computed, ref: Vue.ref, reactive: Vue.reactive, onMounted: () => {},
    useI18n: () => ({ t: key => key }),
  };
  const state = new Function(...Object.keys(deps), script + `
    return { t, users, loading, loadError, success, search, roleFilter, statusFilter, roleOptions, statusOptions,
      filtered, editing, deleting, page, nextToken, reload, loadUsers, changePage, userRole, roleLabel, openEdit, openDelete };
  `)(...Object.values(deps));
  state.loading.value = false;
  state.users.value = [
    { metadata: { name: "alice" }, spec: { scopes: ["ebs:user"], enabled: true, displayName: "Alice", email: "a@example.org" } },
    { metadata: { name: "bob" }, spec: { scopes: ["ebs:user"], enabled: false } },
    { metadata: { name: "carol" }, spec: { scopes: ["ebs:ops"], enabled: true } },
    { metadata: { name: "dave" }, spec: { scopes: ["ebs:ops"], enabled: false } },
  ];
  return state;
}

function renderPage(state) {
  const app = Vue.createSSRApp({ setup: () => state, render });
  app.component("ColumnMultiFilter", {
    props: ["options", "modelValue", "label", "allLabel"],
    setup: props => () => Vue.h("button", { "aria-label": props.label }, props.allLabel),
  });
  for (const name of ["AppSelect", "EmptyState", "ModalDialog", "ArrowLeft", "ArrowRight", "CircleCheckFilled", "Refresh", "Search", "WarningFilled"]) {
    app.component(name, { render: () => null });
  }
  return renderToString(app);
}

test("roles display and filter by admin, ops, user precedence rather than username", () => {
  const state = setup();
  state.users.value = [
    { metadata: { name: "administrator" }, spec: { scopes: ["ebs:user", "ebs:ops", "ebs:admin"] } },
    { metadata: { name: "operator" }, spec: { scopes: ["ebs:user", "ebs:ops"] } },
    { metadata: { name: "admin" }, spec: { scopes: ["ebs:user"] } },
  ];
  assert.deepEqual(state.roleOptions.value.map(option => option.value), ["ebs:admin", "ebs:ops", "ebs:user"]);
  assert.deepEqual(state.users.value.map(state.roleLabel), ["admin.adminUser", "admin.opsUser", "admin.regularUser"]);
  for (const [role, name] of [["ebs:admin", "administrator"], ["ebs:ops", "operator"], ["ebs:user", "admin"]]) {
    state.roleFilter.value = [role];
    assert.deepEqual(state.filtered.value.map(user => user.metadata.name), [name]);
  }
});

test("administrator actions are disabled while other users remain editable", async () => {
  const state = setup();
  const admin = { metadata: { name: "root" }, spec: { scopes: ["ebs:admin"] } };
  state.users.value.push(admin);
  const html = await renderPage(state);
  const rows = [...html.matchAll(/<tr>.*?<td class="admin-row-actions">(.*?)<\/td><\/tr>/gs)].map(match => match[1]);
  assert.equal(rows.length, 5);
  assert.equal((rows[0].match(/ disabled/g) || []).length, 2);
  for (const row of rows.slice(1)) assert.doesNotMatch(row, / disabled/);
  await state.openEdit(admin);
  state.openDelete(admin);
  assert.equal(state.editing.value, null);
  assert.equal(state.deleting.value, null);
  state.openDelete(state.users.value[0]);
  assert.equal(state.deleting.value.metadata.name, "alice");
});

test("current-page rows sort admin before ops before users without mutating API order", () => {
  const state = setup();
  state.users.value.push({ metadata: { name: "root" }, spec: { scopes: ["ebs:user", "ebs:admin"] } });
  assert.deepEqual(state.filtered.value.map(user => user.metadata.name), ["root", "carol", "dave", "alice", "bob"]);
  assert.deepEqual(state.users.value.map(user => user.metadata.name), ["alice", "bob", "carol", "dave", "root"]);
});

test("user role and status filters combine with current-page search", () => {
  const state = setup();
  const names = () => state.filtered.value.map(user => user.metadata.name);
  assert.deepEqual(names(), ["carol", "dave", "alice", "bob"]);
  for (const [role, status, expected] of [
    ["ebs:user", "", ["alice", "bob"]], ["ebs:ops", "", ["carol", "dave"]],
    ["", "true", ["carol", "alice"]], ["", "false", ["dave", "bob"]],
    ["ebs:ops", "false", ["dave"]], ["ebs:user", "true", ["alice"]],
  ]) {
    state.roleFilter.value = role ? [role] : [];
    state.statusFilter.value = status ? [status] : [];
    assert.deepEqual(names(), expected);
  }
  state.search.value = "EXAMPLE.ORG";
  assert.deepEqual(names(), ["alice"]);
  state.statusFilter.value = ["false"];
  assert.deepEqual(names(), []);
  state.search.value = "";
  state.roleFilter.value = [];
  state.statusFilter.value = [];
  assert.equal(names().length, 4);
  state.roleFilter.value = ["ebs:user", "ebs:ops"];
  state.statusFilter.value = ["true", "false"];
  assert.equal(names().length, 4);
});

test("status filtering uses the same enabled default as the table", () => {
  const state = setup();
  state.users.value = [{ metadata: { name: "legacy" }, spec: { scopes: ["ebs:user"] } }];
  state.statusFilter.value = ["true"];
  assert.equal(state.filtered.value.length, 1);
  state.statusFilter.value = ["false"];
  assert.equal(state.filtered.value.length, 0);
});

test("user list keeps filters in table headers even when no rows match", async () => {
  const state = setup();
  const html = await renderPage(state);
  assert.match(html, /aria-label="admin.role"/);
  assert.match(html, /aria-label="admin.enabled"/);
  assert.match(html, /admin.allRoles/);
  assert.match(html, /admin.allStatuses/);
  const toolbar = html.match(/<div class="list-toolbar">([\s\S]*?)<\/div>/)[1];
  assert.doesNotMatch(toolbar, /admin.role|admin.enabled/);
  assert.match(html, /<th><button aria-label="admin.role"/);
  assert.match(html, /<th><button aria-label="admin.enabled"/);
  state.search.value = "no-matching-user";
  const empty = await renderPage(state);
  assert.match(empty, /<th><button aria-label="admin.role"/);
  assert.match(empty, /<td colspan="6">/);
  for (const locale of ["zh-CN", "en-US"]) {
    const messages = readFileSync(new URL(`../src/i18n/messages/${locale}.ts`, import.meta.url), "utf8");
    for (const key of ["allRoles", "allStatuses", "adminUser"]) assert.match(messages, new RegExp(`${key}: "[^"]+"`));
  }
});
