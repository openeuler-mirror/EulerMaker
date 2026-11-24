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
      filtered, editing, deleting, page, nextToken, reload, loadUsers, changePage, roleLabel, openEdit, openDelete };
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

test("user role and status filters combine with current-page search", () => {
  const state = setup();
  const names = () => state.filtered.value.map(user => user.metadata.name);
  assert.deepEqual(names(), ["alice", "bob", "carol", "dave"]);
  for (const [role, status, expected] of [
    ["ebs:user", "", ["alice", "bob"]], ["ebs:ops", "", ["carol", "dave"]],
    ["", "true", ["alice", "carol"]], ["", "false", ["bob", "dave"]],
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
  const renderPage = () => {
    const app = Vue.createSSRApp({ setup: () => state, render });
    app.component("ColumnMultiFilter", {
      props: ["options", "modelValue", "label", "allLabel"],
      setup: props => () => Vue.h("button", { "aria-label": props.label }, props.allLabel),
    });
    for (const name of ["AppSelect", "EmptyState", "ModalDialog", "ArrowLeft", "ArrowRight", "CircleCheckFilled", "Refresh", "Search", "WarningFilled"]) {
      app.component(name, { render: () => null });
    }
    return renderToString(app);
  };
  const html = await renderPage();
  assert.match(html, /aria-label="admin.role"/);
  assert.match(html, /aria-label="admin.enabled"/);
  assert.match(html, /admin.allRoles/);
  assert.match(html, /admin.allStatuses/);
  const toolbar = html.match(/<div class="list-toolbar">([\s\S]*?)<\/div>/)[1];
  assert.doesNotMatch(toolbar, /admin.role|admin.enabled/);
  assert.match(html, /<th><button aria-label="admin.role"/);
  assert.match(html, /<th><button aria-label="admin.enabled"/);
  state.search.value = "no-matching-user";
  const empty = await renderPage();
  assert.match(empty, /<th><button aria-label="admin.role"/);
  assert.match(empty, /<td colspan="6">/);
  for (const locale of ["zh-CN", "en-US"]) {
    const messages = readFileSync(new URL(`../src/i18n/messages/${locale}.ts`, import.meta.url), "utf8");
    for (const key of ["allRoles", "allStatuses"]) assert.match(messages, new RegExp(`${key}: "[^"]+"`));
  }
});
