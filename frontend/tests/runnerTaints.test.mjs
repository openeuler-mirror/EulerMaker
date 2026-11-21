import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import { renderToString } from "@vue/server-renderer";
import * as Vue from "vue";
import ts from "typescript";

const source = readFileSync(new URL("../src/components/RunnerTaints.vue", import.meta.url), "utf8");
const transpile = code => ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
const helpers = await import("data:text/javascript;base64," + Buffer.from(transpile(readFileSync(new URL("../src/utils/runnerLabels.ts", import.meta.url), "utf8"))).toString("base64"));
const script = source.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1].replace(/^import .*;\n/gm, "");
const render = new Function("Vue", compile(source.slice(10, source.indexOf("\n</template>")), { mode: "function", prefixIdentifiers: true }).code)(Vue);

function setup(role = "ops") {
  const runner = Vue.reactive({ metadata: { name: "runner-a", uid: "uid-a", resourceVersion: "1" },
    spec: { taints: [{ key: "pool", value: "gating", effect: "NoSchedule" }] } });
  const current = JSON.parse(JSON.stringify(runner));
  current.metadata.resourceVersion = "2";
  const calls = [], events = [];
  let dispose;
  const transport = { request: async (_, options) => options?.method === "PATCH" ? {} : current };
  const dependencies = {
    computed: Vue.computed, ref: Vue.ref, defineProps: () => ({ runner }), defineEmits: () => event => events.push(event),
    onBeforeUnmount: callback => { dispose = callback; }, useI18n: () => ({ t: key => key }), useSessionStore: () => ({ role }),
    ...helpers, errorTranslationKey: error => error.key || "runnerDetail.saveTaintsFailed",
    request: async (path, options) => { calls.push({ path, options }); return transport.request(path, options); },
  };
  const state = new Function(...Object.keys(dependencies), transpile(script)
    + "\nreturn { props, t, canEdit, editing, saving, errorKey, rows, effectOptions, startEditing, save };")(...Object.values(dependencies));
  const html = () => {
    const app = Vue.createSSRApp({ setup: () => state, render });
    app.component("AppSelect", { props: ["options"], setup: props => () => Vue.h("select", props.options.map(option => Vue.h("option", option.label))) });
    return renderToString(app);
  };
  return { runner, current, state, calls, events, transport, html, dispose: () => dispose() };
}

test("Runner taints display values and offer all supported effects to Ops/Admin", async () => {
  for (const role of ["ops", "admin"]) {
    const view = setup(role);
    assert.match(await view.html(), /<td>pool<\/td><td>gating<\/td><td>NoSchedule<\/td>/);
    view.state.startEditing();
    const html = await view.html();
    for (const effect of ["NoSchedule", "NoExecute", "PreferNoSchedule"]) assert.ok(html.includes(`<option>${effect}</option>`));
    assert.ok(html.includes("runnerDetail.taintsHint"));
  }
  const view = setup("user");
  view.state.startEditing();
  assert.equal(view.state.editing.value, false);
  assert.ok(!(await view.html()).includes("common.edit"));
});

test("Runner taints add, edit and remove with a versioned spec-only patch", async () => {
  for (const taints of [
    [{ key: "pool", value: "gating", effect: "NoExecute" }, { key: "pool", value: "", effect: "PreferNoSchedule" }], [],
  ]) {
    const view = setup();
    view.state.startEditing();
    view.state.rows.value = taints;
    await view.state.save();
    assert.equal(view.calls[1].path, "/apis/ebs/v1/runners/runner-a");
    assert.equal(view.calls[1].options.headers["Content-Type"], "application/merge-patch+json");
    assert.deepEqual(JSON.parse(view.calls[1].options.body), { metadata: { uid: "uid-a", resourceVersion: "2" }, spec: { taints } });
    assert.equal(view.runner.spec.taints[0].effect, "NoSchedule");
    assert.deepEqual(view.events, ["saved"]);
  }
});

test("Runner taint validation rejects invalid keys, values, effects and duplicate key/effect pairs before requests", async () => {
  for (const taints of [
    [{ key: "", effect: "NoSchedule" }], [{ key: "pool", value: "bad value", effect: "NoSchedule" }],
    [{ key: "pool", effect: "unknown" }], [{ key: "pool", effect: "NoSchedule" }, { key: "pool", effect: "NoSchedule" }],
  ]) {
    const view = setup();
    view.state.startEditing();
    view.state.rows.value = taints;
    await view.state.save();
    assert.equal(view.state.errorKey.value, "runnerDetail.invalidTaints");
    assert.equal(view.calls.length, 0);
  }
});

test("Runner taints preserve drafts on concurrent changes, replacement, deletion and failed saves", async () => {
  for (const mutate of [
    view => { view.current.spec.taints.push({ key: "other", effect: "NoSchedule" }); },
    view => { view.current.metadata.uid = "replaced"; },
    view => { view.current.metadata.deletionTimestamp = "2026-11-21T00:00:00Z"; },
    view => { view.transport.request = async (_, options) => { if (options?.method === "PATCH") throw { key: "errors.conflict" }; return view.current; }; },
  ]) {
    const view = setup();
    view.state.startEditing();
    view.state.rows.value = [];
    mutate(view);
    await view.state.save();
    assert.equal(view.state.errorKey.value, "errors.conflict");
    assert.equal(view.state.editing.value, true);
    assert.deepEqual(view.state.rows.value, []);
    assert.deepEqual(view.events, []);
  }
});

test("Runner taints skip unchanged or already-applied writes and cancel requests on unmount", async () => {
  for (const alreadyApplied of [false, true]) {
    const view = setup();
    view.state.startEditing();
    if (alreadyApplied) { view.state.rows.value = []; view.current.spec.taints = []; }
    await view.state.save();
    assert.equal(view.calls.length, 1);
    assert.deepEqual(view.events, ["saved"]);
  }
  const view = setup();
  view.state.startEditing();
  view.state.rows.value = [];
  let resolve;
  view.transport.request = () => new Promise(done => { resolve = done; });
  const saving = view.state.save();
  view.dispose();
  assert.equal(view.calls[0].options.signal.aborted, true);
  resolve(view.current);
  await saving;
  assert.equal(view.calls.length, 1);
  assert.deepEqual(view.events, []);
});
