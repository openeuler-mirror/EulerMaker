import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
import { parse, stringify } from "yaml";

const source = readFileSync(new URL("../src/components/buildConfDraft.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { buildConfSpec, configuredOS, configuredArches, supportsTarget } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

test("JSON/list/YAML roundtrip preserves both architecture images and extra fields", () => {
  const spec = { targets: { os: { arches: { x86_64: { image: "build:x86" }, aarch64: { image: "build:arm" } } } }, extra: { retained: true } };
  const draft = buildConfSpec(JSON.parse(JSON.stringify(spec)));
  draft.targets.os.arches.x86_64.image = "build:new";
  assert.deepEqual(buildConfSpec(parse(stringify(draft))), draft);
  assert.equal(draft.targets.os.arches.aarch64.image, "build:arm");
  assert.deepEqual(draft.extra, { retained: true });
  delete draft.targets.os;
  assert.deepEqual(buildConfSpec(draft).targets, {});
});

test("invalid raw structures fail without silently resetting targets", () => {
  for (const value of [null, [], {}, { targets: null }, { targets: [] }, { targets: { os: null } },
    { targets: { os: { arches: [] } } }, { targets: { os: { arches: { arch: { image: 1 } } } } }]) {
    assert.throws(() => buildConfSpec(value));
  }
  assert.throws(() => parse("targets: {}\ntargets: {}\n", { uniqueKeys: true }));
});

test("target choices are sorted, absent targets have no fallback, old project targets stay unchanged", () => {
  const conf = { spec: { targets: { z: { arches: { x86_64: { image: "build:x86" }, aarch64: { image: "build:arm" } } }, a: { arches: { arch: { image: "build:a" } } } } } };
  assert.deepEqual(configuredOS(conf), ["a", "z"]);
  assert.deepEqual(configuredArches(conf, "z"), ["aarch64", "x86_64"]);
  assert.deepEqual(configuredArches(conf, "constructor"), []);
  const legacy = { os: "old", arch: "x86_64" };
  assert.equal(supportsTarget(conf, legacy), false);
  assert.deepEqual(legacy, { os: "old", arch: "x86_64" });
  assert.equal(supportsTarget(null, legacy), false);
  assert.equal(supportsTarget(conf, { os: "z", arch: "aarch64" }), true);
  delete conf.spec.targets.z.arches.aarch64;
  assert.equal(supportsTarget(conf, { os: "z", arch: "aarch64" }), false);
});
