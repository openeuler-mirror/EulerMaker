import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import * as Vue from "vue";
import { createMemoryHistory, createRouter } from "vue-router";
import { renderToString } from "@vue/server-renderer";
import ts from "typescript";

const resourcesSource = readFileSync(new URL("../src/utils/runnerResources.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(resourcesSource, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { formatRunnerMemory } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

const source = readFileSync(new URL("../src/views/OperationsView.vue", import.meta.url), "utf8");
const labelsSource = readFileSync(new URL("../src/utils/runnerLabels.ts", import.meta.url), "utf8");
const labelsJS = ts.transpileModule(labelsSource, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
const labelHelpers = await import("data:text/javascript;base64," + Buffer.from(labelsJS).toString("base64"));
const { runnerLabelRows, runnerLabelPatch, isSystemRunnerLabel } = labelHelpers;
const searchSource = readFileSync(new URL("../src/utils/runnerSearch.ts", import.meta.url), "utf8");
const searchJS = ts.transpileModule(searchSource, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText;
const searchHelpers = new Function("validRunnerLabelKey", searchJS.replace(/^import .*;\n/gm, "").replace(/^export /gm, "") +
  "\nreturn { runnerSearchOptions, runnerSearchSuggestions, compileRunnerLabelSelector };")(labelHelpers.validRunnerLabelKey);
const template = source.match(/<code class="runner-agent-version"[\s\S]*?<\/code>/)?.[0];
assert.ok(template);
const { code } = compile(template, { mode: "function", prefixIdentifiers: true });
const render = new Function("Vue", code)(Vue);

test("Runner label edits patch only changed keys and retain concurrent unrelated labels", () => {
  const original = { "ebs.io/runner-type": "ct", pool: "old", obsolete: "true" };
  const rows = runnerLabelRows({ "ebs.io/runner-type": "ct", pool: "new", empty: "" });
  const result = runnerLabelPatch(original, rows, { ...original, zone: "new-zone" });
  assert.equal(result.error, undefined);
  assert.deepEqual({ ...result.labels }, { pool: "new", obsolete: null, empty: "" });
  assert.equal(runnerLabelPatch(original, rows, { ...original, pool: "another" }).error, "errors.conflict");
  assert.equal(runnerLabelPatch(original, rows, { ...original, pool: "new" }).error, undefined);
});

test("Runner label validation enforces syntax, uniqueness and read-only system labels", () => {
  for (const labels of [{ "example.com/pool": "community", empty: "" }, { pool: "community" }]) {
    assert.equal(runnerLabelPatch({}, runnerLabelRows(labels), {}).error, undefined);
  }
  for (const rows of [
    [{ key: "", value: "v" }], [{ key: "bad/key/name", value: "v" }],
    [{ key: "Upper.example/key", value: "v" }], [{ key: "a".repeat(64), value: "v" }],
    [{ key: "valid", value: "a".repeat(64) }], [{ key: "valid", value: "two words" }],
    [{ key: "a", value: "1" }, { key: "a", value: "2" }],
  ]) assert.equal(runnerLabelPatch({}, rows, {}).error, "operations.invalidRunnerLabels");
  for (const key of ["ebs.io/runner-type", "ebs.io/runner-arch", "ebs.io/runner-capability.kvm"]) {
    assert.equal(runnerLabelPatch({ [key]: "old" }, [], {}).error, "operations.systemRunnerLabelsReadOnly");
    assert.equal(runnerLabelPatch({}, [{ key, value: "new" }], {}).error, "operations.systemRunnerLabelsReadOnly");
    assert.equal(runnerLabelPatch({ [key]: "old" }, [{ key, value: "new" }], { [key]: "old" }).error, "operations.systemRunnerLabelsReadOnly");
    assert.equal(runnerLabelPatch({ [key]: "old" }, [{ key, value: "old" }], { [key]: "updated-by-runner" }).error, undefined);
  }
  const result = runnerLabelPatch({}, [{ key: "__proto__", value: "x" }], {});
  assert.equal(result.error, "operations.invalidRunnerLabels");
});

const labelsActions = ts.transpileModule(source.slice(source.indexOf("function openRunnerLabels("), source.indexOf("function openEviction(")), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
}).outputText;

function labelEditor(role = "ops") {
  const calls = [];
  const state = {
    canManageRunners: Vue.ref(["admin", "ops"].includes(role)), labelRunner: Vue.ref(null), labelRows: Vue.ref([]),
    lockedLabelRows: Vue.ref(new Set()), labelsSaving: Vue.ref(false), labelsError: Vue.ref(""),
    evicting: Vue.ref(null), removing: Vue.ref(null), success: Vue.ref(""),
    runnerLabelRows, runnerLabelPatch, isSystemRunnerLabel, t: key => key,
    errorTranslationKey: error => error.key || "operations.runnerLabelsFailed", loadRunners: async () => {},
  };
  const runner = Vue.reactive({ metadata: {
    name: "runner-a", uid: "uid-a", resourceVersion: "old",
    labels: { "ebs.io/runner-type": "ct", "ebs.io/runner-arch": "x86_64", pool: "old" },
  } });
  let current = JSON.parse(JSON.stringify(runner));
  current.metadata.resourceVersion = "latest";
  current.metadata.labels.zone = "added-concurrently";
  state.request = async (url, options) => {
    calls.push({ url, options });
    if (!options) return current;
    return {};
  };
  const actions = new Function(...Object.keys(state), labelsActions + "\nreturn { openRunnerLabels, closeRunnerLabels, saveRunnerLabels };")(...Object.values(state));
  return { state, actions, calls, runner, current };
}

test("Ops and Admin save Runner labels using current UID/version without mutating the selected row", async () => {
  for (const role of ["ops", "admin"]) {
    const { state, actions, calls, runner } = labelEditor(role);
    actions.openRunnerLabels(runner);
    assert.equal(state.lockedLabelRows.value.size, 2);
    state.labelRows.value.find(row => row.key === "pool").value = "community";
    assert.equal(runner.metadata.labels.pool, "old");
    await actions.saveRunnerLabels();
    assert.equal(calls.length, 2);
    assert.equal(calls[1].url, "/apis/ebs/v1/runners/runner-a");
    assert.equal(calls[1].options.method, "PATCH");
    assert.equal(calls[1].options.headers["Content-Type"], "application/merge-patch+json");
    assert.deepEqual(JSON.parse(calls[1].options.body), {
      metadata: { uid: "uid-a", resourceVersion: "latest", labels: { pool: "community" } },
    });
    assert.equal(state.labelRunner.value, null);
    assert.equal(state.success.value, "operations.runnerLabelsSaved");
  }
});

test("Runner label editor rejects ordinary users, replaced objects and concurrent label edits", async () => {
  const denied = labelEditor("user");
  denied.actions.openRunnerLabels(denied.runner);
  assert.equal(denied.state.labelRunner.value, null);
  await denied.actions.saveRunnerLabels();
  assert.equal(denied.calls.length, 0);
  for (const change of [current => { current.metadata.uid = "replacement"; }, current => { current.metadata.labels.pool = "another"; }]) {
    const { state, actions, calls, runner, current } = labelEditor();
    actions.openRunnerLabels(runner);
    state.labelRows.value.find(row => row.key === "pool").value = "community";
    change(current);
    await actions.saveRunnerLabels();
    assert.equal(state.labelsError.value, "errors.conflict");
    assert.equal(calls.length, 1);
    assert.ok(state.labelRunner.value);
    assert.equal(state.labelsSaving.value, false);
  }
});

test("Runner system label inputs and removal buttons are disabled", async () => {
  const rowTemplate = source.match(/<div v-for="\(row, index\) in labelRows"[\s\S]*?<\/div>/)?.[0];
  assert.ok(rowTemplate);
  const rowRender = new Function("Vue", compile(rowTemplate, { mode: "function", prefixIdentifiers: true }).code)(Vue);
  const labelRows = Vue.ref(runnerLabelRows({ "ebs.io/runner-type": "ct", "ebs.io/runner-capability.kvm": "true", pool: "community" }));
  const lockedLabelRows = Vue.ref(new Set(labelRows.value.filter(row => isSystemRunnerLabel(row.key))));
  const html = await renderToString(Vue.createSSRApp({
    setup: () => ({ labelRows, lockedLabelRows, labelsSaving: false, t: key => key }), render: rowRender,
  }));
  assert.equal((html.match(/ disabled/g) || []).length, 6);
  assert.ok(html.includes("community"));
});

test("Runner agent version retains the full commit in the hover title", async () => {
  const version = "434dd25a7d6ea4984b15d1fec870d19ab80fe0e5";
  const html = await renderToString(Vue.createSSRApp({
    setup: () => ({ runner: { status: { info: { agentVersion: version } } }, t: () => "—" }),
    render,
  }));
  assert.ok(html.includes(`title="${version}"`));
  assert.ok(html.includes(`>${version}</code>`));
});

test("Runner agent version uses the empty value placeholder before it is reported", async () => {
  const html = await renderToString(Vue.createSSRApp({
    setup: () => ({ runner: {}, t: () => "—" }),
    render,
  }));
  assert.ok(html.includes(">—</code>"));
});

test("Runner memory capacity is formatted in binary units", () => {
  for (const [value, expected] of [
    ["131072Mi", "128 GiB"], ["128Gi", "128 GiB"], ["137438953472", "128 GiB"],
    ["134217728Ki", "128 GiB"], ["1536Mi", "1.5 GiB"], ["512Mi", "512 MiB"],
    ["128G", "119.21 GiB"], ["130000Mi", "126.95 GiB"], ["0Mi", "0 MiB"],
  ]) assert.equal(formatRunnerMemory(value), expected, value);
  for (const value of [undefined, "", "invalid"]) assert.equal(formatRunnerMemory(value), undefined);
});

const capacityCells = source.match(/<td class="runner-capacity">[\s\S]*?<\/td>/g);
assert.equal(capacityCells?.length, 1);
const capacityRender = new Function("Vue", compile(`<tr>${capacityCells.join("")}</tr>`, {
  mode: "function", prefixIdentifiers: true,
}).code)(Vue);

test("Runner type and architecture share one cell with individual empty placeholders", async () => {
  const cell = source.match(/<td class="runner-type-arch">[\s\S]*?<\/td>/)[0];
  const render = new Function("Vue", compile(`<tr>${cell}</tr>`, { mode: "function", prefixIdentifiers: true }).code)(Vue);
  for (const [runner, expected] of [
    [{ spec: { type: "ct", arch: "aarch64" } }, "ct / aarch64"],
    [{ spec: { type: "ct" } }, "ct / —"],
    [{}, "— / —"],
  ]) {
    const html = await renderToString(Vue.createSSRApp({ setup: () => ({ runner, t: () => "—" }), render }));
    assert.ok(html.includes(`>${expected}</td>`));
  }
});

test("Runner table displays total capacity rather than allocatable resources", async () => {
  const html = await renderToString(Vue.createSSRApp({
    setup: () => ({
      runner: { status: { capacity: { cpu: "32", memory: "131072Mi" }, allocatable: { cpu: "8", memory: "16Gi" } } },
      formatRunnerMemory, t: () => "—",
    }),
    render: capacityRender,
  }));
  assert.match(html, />32U<\/span>/);
  assert.match(html, />128 GiB<\/span>/);
  assert.match(html, /<span>32U<\/span> \/ <span>128 GiB<\/span>/);
});

test("Runner capacity cells show placeholders when resources have not been reported", async () => {
  for (const runner of [{}, { status: { capacity: {} } }]) {
    const html = await renderToString(Vue.createSSRApp({
      setup: () => ({ runner, formatRunnerMemory, t: key => key === "common.emptyValue" ? "—" : key }), render: capacityRender,
    }));
    assert.equal((html.match(/>—<\/span>/g) || []).length, 2);
  }
});

test("Runner capacity headers have Chinese and English labels", async () => {
  const headers = source.match(/<th>\{\{ t\("operations.runnerResources"\) \}\}<\/th>/g);
  assert.equal(headers?.length, 1);
  const headerRender = new Function("Vue", compile(`<tr>${headers.join("")}</tr>`, {
    mode: "function", prefixIdentifiers: true,
  }).code)(Vue);
  for (const [locale, resources] of [["zh-CN", "资源"], ["en-US", "Resources"]]) {
    const messages = readFileSync(new URL(`../src/i18n/messages/${locale}.ts`, import.meta.url), "utf8");
    const t = key => messages.match(new RegExp(`${key.split(".")[1]}: "([^"]+)"`))?.[1];
    const html = await renderToString(Vue.createSSRApp({ setup: () => ({ t }), render: headerRender }));
    assert.ok(html.includes(`<th>${resources}</th>`));
  }
});

function runnerFilterView(list) {
  const declarations = source.slice(source.indexOf("const runners ="), source.indexOf("const resourcesLoading ="));
  const actions = source.slice(source.indexOf("function searchRunners("), source.indexOf("function openRunnerLabels("));
  const pagination = source.slice(source.indexOf("async function loadAll<"), source.indexOf("watch(activeSection"));
  const code = ts.transpileModule(declarations + actions + pagination, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
  }).outputText;
  return new Function("ref", "computed", "list", "errorTranslationKey", "runnerSearchOptions", "runnerSearchSuggestions", "nextTick", "compileRunnerLabelSelector", code +
    "\nreturn { runners, runnerSearch, runnerQuery, runnerLabelQuery, runnerSearchError, runnerSuggestions, filteredRunners, runnersError, runnersLoading, searchRunners, loadRunners, suggestionsOpen, activeSuggestion, showSuggestions, chooseSuggestion, onSearchKeydown };")(
    Vue.ref, Vue.computed, list, () => "operations.loadRunnersFailed", searchHelpers.runnerSearchOptions, searchHelpers.runnerSearchSuggestions, Vue.nextTick, searchHelpers.compileRunnerLabelSelector,
  );
}

function operationsNavigation(router) {
  const calls = [];
  const script = source.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1].replace(/^import .*;\n/gm, "");
  const code = ts.transpileModule(script, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
  }).outputText;
  const dependencies = {
    computed: Vue.computed, ref: Vue.ref, watch: Vue.watch, nextTick: Vue.nextTick,
    useRoute: () => ({ get query() { return router.currentRoute.value.query; } }), useRouter: () => router,
    useI18n: () => ({ t: key => key }), useSessionStore: () => ({ role: "ops" }),
    ...searchHelpers, ...labelHelpers,
    list: async path => { calls.push(path); return { items: [], next: "" }; },
    errorTranslationKey: () => "errors.unavailable",
  };
  const scope = Vue.effectScope();
  const state = scope.run(() => new Function(...Object.keys(dependencies), code +
    "\nreturn { activeSection, selectSection };")(...Object.values(dependencies)));
  return { ...state, calls, dispose: () => scope.stop() };
}

function navigationRouter() {
  return createRouter({ history: createMemoryHistory(), routes: [
    { path: "/operations", name: "operations", component: {} },
    { path: "/operations/runners/:name", name: "runner", component: {} },
  ] });
}

async function navigate(router, action) {
  await new Promise(resolve => {
    const stop = router.afterEach(() => { stop(); resolve(); });
    action();
  });
  await Vue.nextTick();
}

test("Runner tab survives detail navigation, browser back and page reload", async () => {
  const router = navigationRouter();
  await router.push("/operations?extra=keep");
  let view = operationsNavigation(router);
  try {
    assert.equal(view.activeSection.value, "build");
    assert.equal(view.calls.length, 0);
    await navigate(router, () => view.selectSection("runners"));
    assert.deepEqual(router.currentRoute.value.query, { extra: "keep", section: "runners" });
    assert.equal(view.activeSection.value, "runners");
    assert.deepEqual(view.calls, ["/apis/ebs/v1/runners?limit=100"]);
    view.dispose();
    await router.push({ name: "runner", params: { name: "runner-a" } });
    await navigate(router, () => router.back());
    view = operationsNavigation(router);
    assert.equal(view.activeSection.value, "runners");
    assert.equal(view.calls.length, 1);
    view.dispose();
    view = operationsNavigation(router);
    assert.equal(view.activeSection.value, "runners");
    await navigate(router, () => view.selectSection("services"));
    assert.equal(view.activeSection.value, "services");
    await navigate(router, () => view.selectSection("build"));
    assert.deepEqual(router.currentRoute.value.query, { extra: "keep" });
    assert.equal(view.activeSection.value, "build");
  } finally { view.dispose(); }
});

test("Operations honors explicit section links and defaults invalid sections to build configuration", async () => {
  const router = navigationRouter();
  await router.push("/operations?section=runners");
  const view = operationsNavigation(router);
  try {
    assert.equal(view.activeSection.value, "runners");
    for (const section of ["services", "scripts", "unknown", ["runners", "scripts"], undefined]) {
      await router.push({ name: "operations", query: { section } });
      await Vue.nextTick();
      assert.equal(view.activeSection.value, section === "services" || section === "scripts" ? section : "build");
    }
  } finally { view.dispose(); }
});

test("Runner suggestions complete names and label terms without losing existing query clauses", () => {
  const options = searchHelpers.runnerSearchOptions([
    { metadata: { name: "runner-ci", labels: { pool: "ci", zone: "east" } } },
    { metadata: { name: "runner-gating", labels: { pool: "gating", zone: "east", empty: "" } } },
  ]);
  const suggest = input => searchHelpers.runnerSearchSuggestions(input, options);
  assert.ok(suggest("").includes("name:"));
  assert.deepEqual(suggest("lab"), ["labels:"]);
  assert.deepEqual(suggest("name:gat"), ["name:runner-gating"]);
  assert.deepEqual(suggest("name: runner-c"), ["name: runner-ci"]);
  assert.deepEqual(suggest("labels:pool=gat"), ["labels:pool=gating"]);
  assert.ok(suggest("labels:empty").includes("labels:empty="));
  assert.deepEqual(suggest("name:runner-ci labels:pool=ci,zone=e"), ["name:runner-ci labels:pool=ci,zone=east"]);
  assert.deepEqual(suggest("labels:pool=ci name:runner-c"), ["labels:pool=ci name:runner-ci"]);
  assert.deepEqual(suggest("name:runner-ci lab"), ["name:runner-ci labels:"]);
  assert.equal(options.labels.filter(value => value === "zone=east").length, 1);
  assert.equal(searchHelpers.runnerSearchSuggestions("name:", { names: Array.from({ length: 100 }, (_, i) => `runner-${i}`), labels: [] }).length, 30);
});

test("Runner label suggestions omit previously selected keys while allowing the current key to change", () => {
  const options = { names: [], labels: ["pool", "pool=ci", "pool=gating", "zone", "zone=east", "empty", "empty="] };
  for (const input of ["labels:pool=ci,", "labels:pool=ci labels:", "labels:!pool,", "labels:pool!=ci,", "labels:pool in (ci,gating),"]) {
    const suggestions = searchHelpers.runnerSearchSuggestions(input, options);
    assert.deepEqual(suggestions, ["zone", "zone=east", "empty", "empty="].map(value => input + value));
  }
  assert.deepEqual(searchHelpers.runnerSearchSuggestions("labels:pool=gat", options), ["labels:pool=gating"]);
  assert.deepEqual(searchHelpers.runnerSearchSuggestions("labels:pool=ci name:runner labels:zone=east,", options), [
    "labels:pool=ci name:runner labels:zone=east,empty", "labels:pool=ci name:runner labels:zone=east,empty=",
  ]);
});

test("Runner autocomplete supports keyboard selection, dismissal and direct submission", () => {
  const view = runnerFilterView(async () => ({ items: [], next: "" }));
  const key = (value, isComposing = false) => {
    let prevented = false;
    view.onSearchKeydown({ key: value, isComposing, preventDefault: () => { prevented = true; } });
    return prevented;
  };
  view.showSuggestions();
  assert.equal(key("ArrowDown"), true);
  assert.equal(view.activeSuggestion.value, 0);
  assert.equal(key("ArrowDown", true), false);
  assert.equal(view.activeSuggestion.value, 0);
  assert.equal(key("Enter"), true);
  assert.equal(view.runnerSearch.value, "name:");
  assert.equal(view.suggestionsOpen.value, false);
  view.runnerSearch.value = "";
  view.showSuggestions();
  key("ArrowUp");
  assert.equal(key("Tab"), false);
  assert.equal(view.runnerSearch.value, "labels:");
  view.showSuggestions();
  assert.equal(key("Escape"), true);
  assert.equal(view.suggestionsOpen.value, false);
  assert.equal(key("Enter"), false);
  view.runnerSearch.value = "";
  view.showSuggestions();
  view.chooseSuggestion(1);
  assert.equal(view.runnerSearch.value, "labels:");
  assert.equal(view.suggestionsOpen.value, false);
});

test("Runner suggestions retain the full list after local filtering and update on refresh", async () => {
  let full = [{ metadata: { name: "runner-ci", labels: { pool: "ci" } } }, { metadata: { name: "runner-gating" } }];
  let requests = 0;
  const view = runnerFilterView(async () => { requests++; return { items: full, next: "" }; });
  await view.loadRunners();
  view.runnerSearch.value = "labels:pool=ci";
  view.searchRunners();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(view.filteredRunners.value.length, 1);
  assert.equal(view.runners.value.length, 2);
  view.runnerSearch.value = "name:gat";
  assert.deepEqual(view.runnerSuggestions.value, ["name:runner-gating"]);
  full = [{ metadata: { name: "runner-new" } }];
  view.runnerSearch.value = "";
  view.searchRunners();
  assert.equal(requests, 1);
  await view.loadRunners();
  assert.ok(view.runnerSuggestions.value.includes("runner-new"));
  assert.equal(view.runners.value.length, 1);
});

test("Runner search parses names and label selectors in a single input", async () => {
  const view = runnerFilterView(async () => ({ items: [], next: "" }));
  for (const [input, name, labels] of [
    [" runner-01 ", "runner-01", ""],
    ["name:runner-01", "runner-01", ""],
    ["labels:pool=gating", "", "pool=gating"],
    ["labels:pool in (gating, ci) name: runner-01", "runner-01", "pool in (gating, ci)"],
    ["runner-01 labels:pool=gating labels:kvm", "runner-01", "pool=gating,kvm"],
    ["", "", ""],
  ]) {
    view.runnerSearch.value = input;
    view.searchRunners();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(view.runnerQuery.value, name);
    assert.equal(view.runnerLabelQuery.value, labels);
  }
});

test("Runner search filters all loaded pages locally and retains conditions during refresh", async () => {
  const urls = [];
  const view = runnerFilterView(async path => {
    const url = new URL(path, "http://localhost");
    urls.push(url);
    return url.searchParams.has("continue")
      ? { items: [{ metadata: { name: "runner-other" } }], next: "" }
      : { items: [{ metadata: { name: "runner-gating", labels: { "ebs.io/pool": "gating", "ebs.io/runner-capability.kvm": "true" } } }], next: "page-2" };
  });
  await view.loadRunners();
  const selector = "ebs.io/pool=gating,ebs.io/runner-capability.kvm";
  view.runnerSearch.value = ` name:GATING labels:${selector} `;
  view.searchRunners();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(view.runnerLabelQuery.value, selector);
  assert.deepEqual(view.filteredRunners.value.map(runner => runner.metadata.name), ["runner-gating"]);
  assert.equal(urls.length, 2);
  assert.equal(urls[1].searchParams.get("continue"), "page-2");
  await view.loadRunners();
  assert.equal(urls.length, 4);
  assert.ok(urls.every(url => !url.searchParams.has("labelSelector")));
  assert.equal(view.filteredRunners.value.length, 1);
  view.runnerSearch.value = "";
  view.searchRunners();
  await new Promise(resolve => setImmediate(resolve));
  assert.ok(urls.slice(-2).every(url => !url.searchParams.has("labelSelector")));
  assert.equal(view.filteredRunners.value.length, 2);
  assert.equal(urls.length, 4);
});

test("Runner refresh ignores stale responses and reports loading failures", async () => {
  const calls = [];
  const view = runnerFilterView(() => new Promise((resolve, reject) => calls.push({ resolve, reject })));
  const first = view.loadRunners();
  const second = view.loadRunners();
  calls[1].resolve({ items: [{ metadata: { name: "new" } }], next: "" });
  await second;
  calls[0].resolve({ items: [{ metadata: { name: "old" } }], next: "" });
  await first;
  assert.equal(view.runners.value[0].metadata.name, "new");
  const failed = view.loadRunners();
  calls[2].reject(new Error("unavailable"));
  await failed;
  assert.equal(view.runnersError.value, "operations.loadRunnersFailed");
  assert.equal(view.runnersLoading.value, false);
});

test("Runner local label selectors handle existence, equality, sets and missing keys", () => {
  const labels = { pool: "ci", empty: "", zone: "east" };
  for (const selector of ["", "pool", "!missing", "pool=ci", "pool==ci", "empty=", "pool!=gating", "missing!=ci", "pool in (ci,gating)", "pool notin (gating)", "missing notin (ci)", "pool=ci,zone=east"]) {
    assert.equal(searchHelpers.compileRunnerLabelSelector(selector)(labels), true, selector);
  }
  for (const selector of ["missing", "!pool", "missing=", "pool=CI", "pool!=ci", "pool in (gating)", "pool notin (ci)", "pool=ci,zone=west"]) {
    assert.equal(searchHelpers.compileRunnerLabelSelector(selector)(labels), false, selector);
  }
});

test("Invalid local selectors preserve the last filter without making requests", () => {
  let requests = 0;
  const view = runnerFilterView(async () => { requests++; return { items: [], next: "" }; });
  view.runnerSearch.value = "labels:pool=ci";
  view.searchRunners();
  for (const selector of ["pool in (ci", "pool=ci,", "pool===ci", "bad/key/name=ci"]) {
    view.runnerSearch.value = `labels:${selector}`;
    view.searchRunners();
    assert.equal(view.runnerSearchError.value, "operations.invalidRunnerSearch");
    assert.equal(view.runnerLabelQuery.value, "pool=ci");
  }
  view.runnerSearch.value = "";
  view.searchRunners();
  assert.equal(view.runnerSearchError.value, "");
  assert.equal(view.runnerLabelQuery.value, "");
  assert.equal(requests, 0);
});
