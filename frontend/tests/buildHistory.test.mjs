import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import * as Vue from "vue";
import { renderToString } from "@vue/server-renderer";

const source = readFileSync(new URL("../src/views/ProjectView.vue", import.meta.url), "utf8");
const template = source.match(/<small\s[^>]*class="build-history-git-ref"[^>]*>[\s\S]*?<\/small>/)?.[0];
assert.ok(template);
const { code } = compile(template, { mode: "function", prefixIdentifiers: true });
const render = new Function("Vue", code)(Vue);

test("build history shows the full Git ref annotation with a hover title", async () => {
  const ref = "refs/merge-requests/501/head";
  const html = await renderToString(Vue.createSSRApp({
    setup: () => ({ build: { metadata: { annotations: { "ebs.io/git-ref": ref } } } }),
    render,
  }));
  assert.ok(html.includes(`title="${ref}"`));
  assert.ok(html.includes(`Git ref: <code>${ref}</code>`));
});

test("build history without a Git ref keeps its existing layout", async () => {
  const html = await renderToString(Vue.createSSRApp({
    setup: () => ({ build: { metadata: { name: "build-a" } } }),
    render,
  }));
  assert.equal(html.replace(/<!--[\s\S]*?-->/g, ""), "");
});

const packageTemplate = source.match(/<small\s[^>]*class="build-history-packages"[^>]*>[\s\S]*?<\/small>/)?.[0];
assert.ok(packageTemplate);
const packageCode = compile(packageTemplate, { mode: "function", prefixIdentifiers: true }).code;
const packageRender = new Function("Vue", packageCode)(Vue);

test("build history limits package names to three and puts the full list in the title", async () => {
  for (const [buildType, packages, visible] of [
    ["single", ["gcc"], "gcc"],
    ["specified", ["gcc", "rpm"], "gcc, rpm"],
    ["specified", ["gcc", "rpm", "bash"], "gcc, rpm, bash"],
    ["incremental", ["gcc", "rpm", "bash", "glibc"], "gcc, rpm, bash, …"],
  ]) {
    const html = await renderToString(Vue.createSSRApp({
      setup: () => ({ build: { spec: { buildType, packages } }, t: () => "软件包" }),
      render: packageRender,
    }));
    assert.ok(html.includes(`title="${packages.join(", ")}"`));
    assert.ok(html.includes(`>软件包: ${visible}</small>`));
  }
});

test("full builds and empty package lists have no package row", async () => {
  for (const spec of [{ buildType: "full", packages: ["gcc"] }, { buildType: "single", packages: [] }, {}]) {
    const html = await renderToString(Vue.createSSRApp({
      setup: () => ({ build: { spec }, t: () => "软件包" }),
      render: packageRender,
    }));
    assert.equal(html.replace(/<!--[\s\S]*?-->/g, ""), "");
  }
});
