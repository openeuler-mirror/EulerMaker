import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import { renderToString } from "@vue/server-renderer";
import * as Vue from "vue";
import ts from "typescript";

const source = readFileSync(new URL("../src/views/RunnerView.vue", import.meta.url), "utf8");
const script = source.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1].replace(/^import .*;\n/gm, "");
const transpile = code => ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
const resources = await import("data:text/javascript;base64," + Buffer.from(transpile(readFileSync(new URL("../src/utils/runnerResources.ts", import.meta.url), "utf8"))).toString("base64"));
const translate = key => key === "common.emptyValue" ? "—" : key;
const tick = async () => { await Vue.nextTick(); await Promise.resolve(); };

function setup() {
  const route = Vue.reactive({ params: { name: "runner-a" } });
  const calls = [];
  let dispose;
  const request = (path, options) => new Promise((resolve, reject) => calls.push({ path, options, resolve, reject }));
  const dependencies = {
    computed: Vue.computed, ref: Vue.ref, watch: Vue.watch,
    onBeforeUnmount: callback => { dispose = callback; }, useRoute: () => route,
    useI18n: () => ({ t: translate, locale: Vue.ref("en-US") }), request,
    errorTranslationKey: error => error.key || "operations.loadRunnersFailed",
    formatRunnerMemory: resources.formatRunnerMemory,
  };
  const scope = Vue.effectScope();
  const state = scope.run(() => new Function(...Object.keys(dependencies), transpile(script)
    + "\nreturn { name, runner, loading, errorKey, fields, resourceKeys, metadataGroups, formatDate, resourceLabel, resourceValue, loadRunner, t };")(...Object.values(dependencies)));
  return { state, calls, route, dispose: () => { dispose(); scope.stop(); } };
}

test("Runner details load a named object and discard late responses after navigation or unmount", async () => {
  const view = setup();
  try {
    assert.equal(view.calls[0].path, "/apis/ebs/v1/runners/runner-a");
    view.route.params.name = "runner/b";
    await tick();
    assert.equal(view.calls[0].options.signal.aborted, true);
    assert.equal(view.calls[1].path, "/apis/ebs/v1/runners/runner%2Fb");
    view.calls[1].resolve({ metadata: { name: "runner/b" } });
    await tick();
    view.calls[0].resolve({ metadata: { name: "runner-a" } });
    await tick();
    assert.equal(view.state.runner.value.metadata.name, "runner/b");
    const refresh = view.state.loadRunner();
    view.dispose();
    assert.equal(view.calls[2].options.signal.aborted, true);
    view.calls[2].resolve({ metadata: { name: "removed" } });
    await refresh;
    assert.equal(view.state.runner.value, null);
  } finally { view.dispose(); }
});

test("Runner details display request errors and allow explicit retry", async () => {
  const view = setup();
  try {
    for (const key of ["errors.notFound", "errors.forbidden", "errors.unavailable"]) {
      view.calls.at(-1).reject({ key });
      await tick();
      assert.equal(view.state.errorKey.value, key);
      assert.equal(view.state.loading.value, false);
      assert.equal(view.state.runner.value, null);
      void view.state.loadRunner();
    }
    view.calls.at(-1).resolve({ metadata: { name: "runner-a" }, status: { phase: "Online" } });
    await tick();
    assert.equal(view.state.errorKey.value, "");
    assert.equal(view.state.runner.value.status.phase, "Online");
  } finally { view.dispose(); }
});

const template = source.slice(source.indexOf("<template>") + 10, source.indexOf("\n</template>"));
const render = new Function("Vue", compile(template, { mode: "function", prefixIdentifiers: true }).code)(Vue);
async function renderView(state) {
  const app = Vue.createSSRApp({ setup: () => state, render });
  app.component("RouterLink", { props: ["to"], setup: (_, { slots }) => () => Vue.h("a", slots.default?.()) });
  app.component("StatusBadge", { props: ["value"], setup: props => () => Vue.h("span", props.value) });
  app.component("RunnerLabels", { props: ["runner"], setup: props => () => Vue.h("div", JSON.stringify(props.runner.metadata?.labels)) });
  app.component("RunnerTaints", { props: ["runner"], setup: props => () => Vue.h("div", JSON.stringify(props.runner.spec?.taints)) });
  for (const name of ["ArrowLeft", "Refresh", "WarningFilled"]) app.component(name, { render: () => Vue.h("i") });
  return renderToString(app);
}

test("Runner details display resources, metadata, taints, addresses and escaped diagnostics", async () => {
  const view = setup();
  try {
    view.calls[0].resolve({
      metadata: { name: "runner-a", uid: "uid-a", labels: { "ebs.io/pool": "gating" }, annotations: { note: "<script>bad</script>" } },
      spec: { instanceID: "instance-a", type: "ct", arch: "aarch64", taints: [{ key: "ebs.io/pool", value: "gating", effect: "NoSchedule" }] },
      status: { phase: "Online", activeJobs: 0, capacity: { cpu: "64", memory: "128Gi", "ephemeral-storage": "98Gi" }, allocatable: { cpu: "60", memory: "120Gi", "ephemeral-storage": "80Gi" },
        info: { os: "openEuler", kernelVersion: "6.6", runtimeVersion: "containerd", agentVersion: "a".repeat(40) },
        addresses: [{ type: "InternalIP", address: "192.0.2.10" }],
        conditions: [{ type: "Ready", status: "False", reason: "CheckFailed", message: "<img onerror=bad>" }],
      },
    });
    await tick();
    const html = await renderView(view.state);
    assert.equal((html.match(/class="resource-card"/g) || []).length, 3);
    assert.ok(html.includes("runnerDetail.ephemeralStorage"));
    assert.ok(html.includes("98 GiB"));
    assert.ok(html.includes("80 GiB"));
    for (const value of ["64", "60", "128 GiB", "120 GiB", "instance-a", "gating", "NoSchedule", "192.0.2.10", "CheckFailed", "a".repeat(40)]) assert.ok(html.includes(value), value);
    assert.ok(html.includes("&lt;script&gt;bad&lt;/script&gt;"));
    assert.ok(html.includes("&lt;img onerror=bad&gt;"));
    assert.equal(view.state.formatDate("invalid"), "—");
    view.state.runner.value = { metadata: { deletionTimestamp: "2026-11-21T00:00:00Z" } };
    const empty = await renderView(view.state);
    assert.ok(empty.includes("Deleting"));
    assert.ok(empty.includes("runnerDetail.none"));
    assert.ok(empty.includes("—"));
  } finally { view.dispose(); }
});

test("Runner navigation is restricted to Ops/Admin and returns to the Runner tab", () => {
  const routerSource = readFileSync(new URL("../src/router.ts", import.meta.url), "utf8").replace(/^import .*;\n/gm, "").replace("export const router", "const router");
  let guard;
  const session = { authenticated: false, role: "" };
  const router = new Function("createRouter", "createWebHistory", "useSessionStore", transpile(routerSource) + "\nreturn router;")(
    options => ({ ...options, beforeEach: callback => { guard = callback; } }), () => ({}), () => session,
  );
  const route = router.routes.find(item => item.name === "runner");
  assert.equal(route.path, "/operations/runners/:name");
  const target = { meta: route.meta, fullPath: "/operations/runners/runner-a" };
  assert.equal(guard(target).name, "login");
  session.authenticated = true;
  session.role = "user";
  assert.equal(guard(target).name, "projects");
  for (const role of ["ops", "admin"]) { session.role = role; assert.equal(guard(target), true); }
  const operations = readFileSync(new URL("../src/views/OperationsView.vue", import.meta.url), "utf8");
  assert.match(operations, /name: 'runner', params: \{ name: runner\.metadata\.name \}/);
  assert.match(source, /name: 'operations', query: \{ section: 'runners' \}/);
});
