import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import * as Vue from "vue";
import { renderToString } from "@vue/server-renderer";

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
