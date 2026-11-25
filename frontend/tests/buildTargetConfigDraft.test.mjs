import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
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
  const choices = setup(value => ({ value }), getter => ({ get value() { return getter(); } }), () => {}, async path => {
    assert.equal(path, "/apis/ebs/v1/build-targets");
    if (fail) throw new Error("unavailable");
    return { items: [{ distro: "oe", arches: ["aarch64", "x86_64"] }] };
  }, () => "loadFailed");
  await choices.reload();
  assert.deepEqual(choices.operatingSystems.value, ["oe"]);
  assert.deepEqual(choices.arches("oe"), ["aarch64", "x86_64"]);
  assert.equal(choices.supports({ os: "oe", arch: "x86_64" }), true);
  assert.deepEqual(choices.arches("unknown"), []);
  fail = true;
  await choices.reload();
  assert.equal(choices.supports({ os: "oe", arch: "x86_64" }), false);
  assert.equal(choices.error.value, "loadFailed");
});
