import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { parse as parseComponent } from "@vue/compiler-sfc";
import ts from "typescript";
import { parse, stringify } from "yaml";

const source = readFileSync(new URL("../src/components/buildTargetConfigDraft.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { parseBuildTargetContent, supportsTarget } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

test("JSON/list/YAML roundtrip preserves both architecture images and extra fields", () => {
  const spec = { targets: { os: { arches: { x86_64: { image: "build:x86" }, aarch64: { image: "build:arm" } } } }, extra: { retained: true } };
  const draft = parseBuildTargetContent(JSON.parse(JSON.stringify(spec)));
  draft.targets.os.arches.x86_64.image = "build:new";
  assert.deepEqual(parseBuildTargetContent(parse(stringify(draft))), draft);
  assert.equal(draft.targets.os.arches.aarch64.image, "build:arm");
  assert.deepEqual(draft.extra, { retained: true });
  delete draft.targets.os;
  assert.deepEqual(parseBuildTargetContent(draft).targets, {});
});

test("invalid raw structures fail without silently resetting targets", () => {
  for (const value of [null, [], {}, { targets: null }, { targets: [] }, { targets: { os: null } },
    { targets: { os: { arches: [] } } }, { targets: { os: { arches: { arch: { image: 1 } } } } }]) {
    assert.throws(() => parseBuildTargetContent(value));
  }
  assert.throws(() => parse("targets: {}\ntargets: {}\n", { uniqueKeys: true }));
});

test("editor detects removed target mappings without changing the saved selection", () => {
  const conf = { targets: { z: { arches: { x86_64: { image: "build:x86" }, aarch64: { image: "build:arm" } } }, a: { arches: { arch: { image: "build:a" } } } } };
  const legacy = { os: "old", arch: "x86_64" };
  assert.equal(supportsTarget(conf, legacy), false);
  assert.deepEqual(legacy, { os: "old", arch: "x86_64" });
  assert.equal(supportsTarget(null, legacy), false);
  assert.equal(supportsTarget(conf, { os: "z", arch: "aarch64" }), true);
  delete conf.targets.z.arches.aarch64;
  assert.equal(supportsTarget(conf, { os: "z", arch: "aarch64" }), false);
});

test("project selectors use only the published distro and architecture choices", async () => {
  const source = readFileSync(new URL("../src/composables/useBuildTargetConfig.ts", import.meta.url), "utf8");
  const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
  const setup = new Function("ref", "computed", "onMounted", "request", "errorTranslationKey",
    outputText.replace(/^import .*;$/gm, "").replace("export function", "function") + "; return useBuildTargetConfig()"
  );
  let fail = false;
  let requests = 0;
  const mounted = [];
  const choices = setup(value => ({ value }), getter => ({ get value() { return getter(); } }), callback => mounted.push(callback), async path => {
    requests++;
    assert.equal(path, "/apis/ebs/v1/build-targets");
    if (fail) throw new Error("unavailable");
    return { items: [{ distro: "oe", arches: ["aarch64", "x86_64"] }] };
  }, () => "loadFailed");
  for (const callback of mounted) callback();
  assert.equal(requests, 0, "mounting a page must not request build targets");
  assert.equal(choices.loading.value, false);
  const first = choices.reload();
  const concurrent = choices.reload();
  assert.equal(choices.loading.value, true);
  assert.equal(requests, 1, "concurrent form requests should share one request");
  await Promise.all([first, concurrent]);
  assert.deepEqual(choices.operatingSystems.value, ["oe"]);
  assert.deepEqual(choices.arches("oe"), ["aarch64", "x86_64"]);
  assert.equal(choices.supports({ os: "oe", arch: "x86_64" }), true);
  assert.deepEqual(choices.arches("unknown"), []);
  fail = true;
  await choices.reload();
  assert.equal(choices.supports({ os: "oe", arch: "x86_64" }), false);
  assert.equal(choices.error.value, "loadFailed");
  assert.equal(choices.loading.value, false);
  fail = false;
  await choices.reload();
  assert.equal(choices.error.value, "");
  assert.equal(choices.supports({ os: "oe", arch: "aarch64" }), true);
});

test("project creation loads build targets on demand, but browsing and YAML import do not", () => {
  const component = readFileSync(new URL("../src/views/ProjectsView.vue", import.meta.url), "utf8");
  const { descriptor } = parseComponent(component);
  const source = ts.createSourceFile("ProjectsView.ts", descriptor.scriptSetup.content, ts.ScriptTarget.Latest, true);
  const openDialog = source.statements.find(statement => ts.isFunctionDeclaration(statement) && statement.name?.text === "openDialog");
  const { outputText } = ts.transpileModule(openDialog.getText(source), {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
  });
  let loads = 0;
  const redirects = [];
  const state = {
    actionErrorKey: { value: "" }, successKey: { value: "" }, submitErrorKey: { value: "" },
    session: { authenticated: false }, canCreate: { value: false }, dialog: { value: "" },
    router: { push: route => redirects.push(route) },
    reloadTargetConfig: async () => { loads++; },
  };
  const open = new Function(...Object.keys(state), outputText + "; return openDialog;")(...Object.values(state));
  assert.equal(loads, 0);
  open("create");
  assert.equal(redirects[0].name, "login");
  assert.equal(loads, 0);
  state.session.authenticated = true;
  open("create");
  assert.equal(state.actionErrorKey.value, "projects.noPermission");
  assert.equal(loads, 0);
  state.canCreate.value = true;
  open("import");
  assert.equal(state.dialog.value, "import");
  assert.equal(loads, 0);
  open("create");
  assert.equal(state.dialog.value, "create");
  assert.equal(loads, 1);
  open("create");
  assert.equal(loads, 2, "reopening the form refreshes build targets");
});
