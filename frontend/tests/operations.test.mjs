import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import * as Vue from "vue";
import { renderToString } from "@vue/server-renderer";
import ts from "typescript";

const resourcesSource = readFileSync(new URL("../src/utils/runnerResources.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(resourcesSource, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { formatRunnerMemory } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

const source = readFileSync(new URL("../src/views/OperationsView.vue", import.meta.url), "utf8");
const template = source.match(/<code class="runner-agent-version"[\s\S]*?<\/code>/)?.[0];
assert.ok(template);
const { code } = compile(template, { mode: "function", prefixIdentifiers: true });
const render = new Function("Vue", code)(Vue);

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
assert.equal(capacityCells?.length, 2);
const capacityRender = new Function("Vue", compile(`<tr>${capacityCells.join("")}</tr>`, {
  mode: "function", prefixIdentifiers: true,
}).code)(Vue);

test("Runner table displays total capacity rather than allocatable resources", async () => {
  const html = await renderToString(Vue.createSSRApp({
    setup: () => ({
      runner: { status: { capacity: { cpu: "32", memory: "131072Mi" }, allocatable: { cpu: "8", memory: "16Gi" } } },
      formatRunnerMemory, t: () => "—",
    }),
    render: capacityRender,
  }));
  assert.match(html, />32<\/td>/);
  assert.match(html, />128 GiB<\/td>/);
});

test("Runner capacity cells show placeholders when resources have not been reported", async () => {
  for (const runner of [{}, { status: { capacity: {} } }]) {
    const html = await renderToString(Vue.createSSRApp({
      setup: () => ({ runner, formatRunnerMemory, t: () => "—" }), render: capacityRender,
    }));
    assert.equal((html.match(/>—<\/td>/g) || []).length, 2);
  }
});

test("Runner capacity headers have Chinese and English labels", async () => {
  const headers = source.match(/<th>\{\{ t\("operations.runner(?:CPU|Memory)"\) \}\}<\/th>/g);
  assert.equal(headers?.length, 2);
  const headerRender = new Function("Vue", compile(`<tr>${headers.join("")}</tr>`, {
    mode: "function", prefixIdentifiers: true,
  }).code)(Vue);
  for (const [locale, cpu, memory] of [["zh-CN", "CPU（核）", "内存总量"], ["en-US", "CPU (cores)", "Total memory"]]) {
    const messages = readFileSync(new URL(`../src/i18n/messages/${locale}.ts`, import.meta.url), "utf8");
    const t = key => messages.match(new RegExp(`${key.split(".")[1]}: "([^"]+)"`))?.[1];
    const html = await renderToString(Vue.createSSRApp({ setup: () => ({ t }), render: headerRender }));
    assert.ok(html.includes(`<th>${cpu}</th>`));
    assert.ok(html.includes(`<th>${memory}</th>`));
  }
});
