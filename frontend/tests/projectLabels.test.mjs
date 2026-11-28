import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import { renderToString } from "@vue/server-renderer";
import * as Vue from "vue";
import ts from "typescript";

const read = path => readFileSync(new URL(path, import.meta.url), "utf8");
const transpile = code => ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
const moduleURL = code => "data:text/javascript;base64," + Buffer.from(transpile(code)).toString("base64");
const typeURL = moduleURL(read("../src/utils/projectType.ts"));
const helpers = await import(moduleURL(read("../src/utils/projectLabels.ts").replace('"./projectType"', JSON.stringify(typeURL))));
const { PROJECT_TYPE_LABEL } = await import(typeURL);
const { PROJECT_OWNER_LABEL, PROJECT_PURPOSE_LABEL, projectLabelPatch } = helpers;
const source = read("../src/components/ProjectLabels.vue");
const script = source.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1].replace(/^import .*;\n/gm, "");
const render = new Function("Vue", compile(source.slice(10, source.indexOf("\n</template>")), { mode: "function", prefixIdentifiers: true }).code)(Vue);

function setup(role = "ops", username = "owner") {
  const project = Vue.reactive({ metadata: { name: "demo", uid: "uid-a", resourceVersion: "1", labels: {
    [PROJECT_OWNER_LABEL]: "owner", [PROJECT_TYPE_LABEL]: "personal", pool: "old", remove: "me",
  } } });
  const current = JSON.parse(JSON.stringify(project));
  current.metadata.resourceVersion = "2";
  current.metadata.labels.zone = "concurrent";
  const calls = [], events = [];
  let dispose;
  const dependencies = {
    computed: Vue.computed, ref: Vue.ref, defineProps: () => ({ project }), defineEmits: () => (...event) => events.push(event),
    onBeforeUnmount: callback => { dispose = callback; }, useI18n: () => ({ t: key => key }),
    useSessionStore: () => ({ role, session: { identity: { name: username } } }),
    ...helpers, errorTranslationKey: error => error.key || "project.labelsFailed",
    request: async (path, options) => {
      calls.push({ path, options });
      if (dependencies.failure && options?.method === "PATCH") throw dependencies.failure;
      return current;
    },
  };
  const state = new Function(...Object.keys(dependencies), transpile(script) + "\nreturn { t, canEdit, entries, visibleEntries, editing, saving, errorKey, rows, lockedRows, startEditing, save };")(...Object.values(dependencies));
  const html = () => renderToString(Vue.createSSRApp({ setup: () => state, render }));
  const edit = () => { state.startEditing(); state.rows.value.find(row => row.key === "pool").value = "gating"; };
  return { project, current, state, calls, events, html, edit, dependencies, dispose: () => dispose() };
}

test("Project label editor shows key/value rows and restricts editing to owners", async () => {
  const view = setup();
  assert.match(await view.html(), /class="label-line"><dt>pool<\/dt><dd>old<\/dd>/);
  view.state.startEditing();
  assert.equal(view.state.lockedRows.value.size, 1);
  assert.match(await view.html(), /project.labelsHint/);
  for (const [role, name] of [["ops", "other"], ["admin", "other"], ["user", "member"], ["", ""]]) {
    const other = setup(role, name);
    other.state.startEditing();
    assert.equal(other.state.editing.value, false);
    assert.ok(!(await other.html()).includes("common.edit"));
  }
  const user = setup("user");
  user.state.startEditing();
  assert.equal(user.state.lockedRows.value.size, 2);
});

test("Project labels patch adds, edits and removes only changed labels using the current version", async () => {
  const view = setup();
  view.project.metadata.labels["ebs.io/member-user.member"] = "true";
  view.current.metadata.labels["ebs.io/member-user.member"] = "true";
  view.edit();
  view.state.rows.value = view.state.rows.value.filter(row => row.key !== "remove");
  view.state.rows.value.push({ key: "new", value: "" });
  await view.state.save();
  assert.equal(view.calls[1].path, "/apis/ebs/v1/projects/demo");
  assert.equal(view.calls[1].options.headers["Content-Type"], "application/merge-patch+json");
  assert.deepEqual(JSON.parse(view.calls[1].options.body), { metadata: {
    uid: "uid-a", resourceVersion: "2", labels: { pool: "gating", remove: null, new: "" },
  } });
  assert.equal(view.project.metadata.labels.pool, "old");
  assert.deepEqual(view.events, [["saved", view.current]]);
  assert.match(read("../src/views/ProjectView.vue"), /<ProjectLabels[^>]*@saved="project = \$event"/);
  assert.equal(view.state.editing.value, false);
});

test("Project label display hides ownership labels without removing them from editing", async () => {
  const view = setup();
  view.project.metadata.labels["ebs.io/member-user.member"] = "true";
  const html = await view.html();
  assert.doesNotMatch(html, /ebs.io\/owner-user|ebs.io\/member-user\./);
  assert.match(html, /<dt>pool<\/dt>/);
  view.state.startEditing();
  assert.ok(view.state.rows.value.some(row => row.key === PROJECT_OWNER_LABEL));
  assert.ok(view.state.rows.value.some(row => row.key === "ebs.io/member-user.member"));
  await view.state.save();
  assert.equal(view.calls.length, 0);

  view.project.metadata.labels = { [PROJECT_OWNER_LABEL]: "owner", "ebs.io/member-user.member": "true" };
  assert.match(await view.html(), /project.noLabels/);
  assert.doesNotMatch(await view.html(), /class="label-list"/);
});

test("Reserved Project label edits follow user, Ops and Admin permissions", () => {
  const patch = (role, key, old, value) => projectLabelPatch(old, value === undefined ? [] : [{ key, value }], old, role);
  for (const role of ["user", "ops", "admin"]) {
    assert.equal(patch(role, PROJECT_OWNER_LABEL, { [PROJECT_OWNER_LABEL]: "owner" }, undefined).error, "project.labelsReadOnly");
  }
  assert.equal(patch("user", PROJECT_TYPE_LABEL, {}, "personal").error, undefined);
  assert.equal(patch("user", PROJECT_TYPE_LABEL, {}, "community").error, "project.labelsReadOnly");
  assert.equal(patch("user", PROJECT_TYPE_LABEL, { [PROJECT_TYPE_LABEL]: "personal" }, undefined).error, "project.labelsReadOnly");
  assert.equal(patch("ops", PROJECT_TYPE_LABEL, {}, "community").error, undefined);
  assert.equal(patch("ops", PROJECT_TYPE_LABEL, { [PROJECT_TYPE_LABEL]: "community" }, undefined).error, undefined);
  assert.equal(patch("admin", PROJECT_TYPE_LABEL, {}, "community").error, "project.labelsReadOnly");
  for (const role of ["ops", "admin"]) assert.equal(patch(role, PROJECT_PURPOSE_LABEL, {}, "gating").error, undefined);
  assert.equal(patch("user", PROJECT_PURPOSE_LABEL, {}, "gating").error, "project.labelsReadOnly");
  assert.equal(patch("admin", "pool", {}, "gating").error, "project.labelsReadOnly");
});

test("Label validation rejects invalid and duplicate keys and invalid reserved values", () => {
  for (const rows of [
    [{ key: "bad/key/extra", value: "ok" }], [{ key: "pool", value: "bad value" }],
    [{ key: "pool", value: "a" }, { key: "pool", value: "b" }],
  ]) assert.equal(projectLabelPatch({}, rows, {}, "ops").error, "project.invalidLabels");
  for (const key of [PROJECT_TYPE_LABEL, PROJECT_PURPOSE_LABEL, "ebs.io/member-user.member"]) {
    assert.equal(projectLabelPatch({}, [{ key, value: "wrong" }], {}, "ops").error, "project.invalidLabelValue");
  }
});

test("Project label drafts survive conflicts, replacement, deletion and request failure", async () => {
  for (const change of [
    metadata => { metadata.labels.pool = "other"; }, metadata => { metadata.uid = "replaced"; },
    metadata => { metadata.deletionTimestamp = "2026-11-24T00:00:00Z"; },
  ]) {
    const view = setup();
    view.edit();
    change(view.current.metadata);
    await view.state.save();
    assert.equal(view.state.errorKey.value, "errors.conflict");
    assert.equal(view.state.editing.value, true);
    assert.equal(view.calls.length, 1);
  }
  const view = setup();
  view.edit();
  view.dependencies.failure = new Error("offline");
  await view.state.save();
  assert.equal(view.state.errorKey.value, "project.labelsFailed");
  assert.equal(view.state.editing.value, true);
  assert.deepEqual(view.events, []);
});

test("Unchanged labels do not send requests and unmount cancels pending saves", async () => {
  const unchanged = setup();
  unchanged.state.startEditing();
  await unchanged.state.save();
  assert.equal(unchanged.calls.length, 0);
  const view = setup();
  view.edit();
  const saving = view.state.save();
  view.dispose();
  await saving;
  assert.equal(view.calls[0].options.signal.aborted, true);
  assert.equal(view.calls.length, 1);
  assert.deepEqual(view.events, []);
});
