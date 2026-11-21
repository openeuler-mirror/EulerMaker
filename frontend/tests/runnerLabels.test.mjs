import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import { renderToString } from "@vue/server-renderer";
import * as Vue from "vue";
import ts from "typescript";

const source = readFileSync(new URL("../src/components/RunnerLabels.vue", import.meta.url), "utf8");
const transpile = code => ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
const helpers = await import("data:text/javascript;base64," + Buffer.from(transpile(readFileSync(new URL("../src/utils/runnerLabels.ts", import.meta.url), "utf8"))).toString("base64"));
const script = source.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1].replace(/^import .*;\n/gm, "");
const render = new Function("Vue", compile(source.slice(10, source.indexOf("\n</template>")), { mode: "function", prefixIdentifiers: true }).code)(Vue);

function setup(role = "ops") {
  const runner = Vue.reactive({ metadata: { name: "runner-a", uid: "uid-a", resourceVersion: "1", labels: {
    pool: "old", "ebs.io/runner-arch": "aarch64", "ebs.io/runner-capability.kvm": "true",
  } } });
  const current = JSON.parse(JSON.stringify(runner));
  current.metadata.resourceVersion = "2";
  current.metadata.labels.zone = "concurrent";
  const calls = [], events = [];
  let dispose;
  const dependencies = {
    computed: Vue.computed, ref: Vue.ref, defineProps: () => ({ runner }), defineEmits: () => event => events.push(event),
    onBeforeUnmount: callback => { dispose = callback; }, useI18n: () => ({ t: key => key }), useSessionStore: () => ({ role }),
    ...helpers, errorTranslationKey: error => error.key || "operations.runnerLabelsFailed",
    request: async (path, options) => {
      calls.push({ path, options });
      return options?.method === "PATCH" ? {} : current;
    },
  };
  const state = new Function(...Object.keys(dependencies), transpile(script) + "\nreturn { t, canEdit, entries, editing, saving, errorKey, rows, lockedRows, startEditing, save };")(...Object.values(dependencies));
  const html = () => renderToString(Vue.createSSRApp({ setup: () => state, render }));
  return { runner, current, state, calls, events, html, dispose: () => dispose() };
}

test("Runner label details display key/value on the same row and keep managed labels read-only", async () => {
  const view = setup();
  const html = await view.html();
  assert.match(html, /class="label-line"><dt>pool<\/dt><dd>old<\/dd>/);
  view.state.startEditing();
  assert.equal(view.state.lockedRows.value.size, 2);
  const form = await view.html();
  assert.equal((form.match(/ disabled/g) || []).length, 6);
  assert.ok(form.includes("operations.runnerLabelsHint"));
});

test("Runner label details patch only edits with current identity and version, then refresh", async () => {
  for (const role of ["ops", "admin"]) {
    const view = setup(role);
    view.state.startEditing();
    view.state.rows.value.find(row => row.key === "pool").value = "gating";
    view.state.rows.value.push({ key: "new", value: "" });
    await view.state.save();
    assert.equal(view.calls[1].path, "/apis/ebs/v1/runners/runner-a");
    assert.deepEqual(JSON.parse(view.calls[1].options.body), { metadata: {
      uid: "uid-a", resourceVersion: "2", labels: { pool: "gating", new: "" },
    } });
    assert.equal(view.runner.metadata.labels.pool, "old");
    assert.deepEqual(view.events, ["saved"]);
    assert.equal(view.state.editing.value, false);
  }
});

test("Runner label details retain drafts on concurrent changes, replaced or deleting objects", async () => {
  for (const change of [metadata => { metadata.labels.pool = "other"; }, metadata => { metadata.uid = "replaced"; }, metadata => { metadata.deletionTimestamp = "2026-11-21T00:00:00Z"; }]) {
    const view = setup();
    view.state.startEditing();
    view.state.rows.value.find(row => row.key === "pool").value = "gating";
    change(view.current.metadata);
    await view.state.save();
    assert.equal(view.state.errorKey.value, "errors.conflict");
    assert.equal(view.state.editing.value, true);
    assert.equal(view.calls.length, 1);
    assert.deepEqual(view.events, []);
  }
});

test("Runner label details guard edit permissions and skip patches for unchanged labels", async () => {
  const user = setup("user");
  user.state.startEditing();
  assert.equal(user.state.editing.value, false);
  assert.ok(!(await user.html()).includes("common.edit"));
  const view = setup();
  view.state.startEditing();
  await view.state.save();
  assert.equal(view.calls.length, 1);
  assert.deepEqual(view.events, ["saved"]);
});
