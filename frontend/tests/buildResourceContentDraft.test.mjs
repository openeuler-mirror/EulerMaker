import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/components/buildResourceContentDraft.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { parseResourceContentDraft, serializeResourceContent } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

test("round trip preserves defaults, architectures, custom resources and extra fields", () => {
  const spec = {
    extra: { retained: true },
    default: { requests: { cpu: "4", memory: "8Gi", "example.com/gpu": "1" }, limits: { cpu: "8" } },
    packages: {
      "kernel:kernel-rt": { extra: 1, default: { requests: { memory: "16Gi" } }, arches: {
        aarch64: { requests: { cpu: "8" }, limits: { memory: "32Gi" }, extra: true },
      } },
    },
  };
  assert.deepEqual(serializeResourceContent(parseResourceContentDraft(JSON.stringify(spec))), spec);
});

test("edits, additions, removal and switching back use the latest draft", () => {
  const draft = parseResourceContentDraft('{"default":{"requests":{"cpu":"4","memory":"8Gi"}},"packages":{"gcc":{"default":{"requests":{"cpu":"8"}}}}}');
  draft.defaults.requests.memory = "16Gi";
  draft.packages[0].name = "gcc-next";
  draft.packages[0].arches.push({ name: "aarch64", resources: { requests: { cpu: "16" } } });
  const spec = serializeResourceContent(draft);
  assert.equal(spec.default.requests.memory, "16Gi");
  assert.equal(spec.packages["gcc-next"].arches.aarch64.requests.cpu, "16");
  assert.deepEqual(serializeResourceContent(parseResourceContentDraft(JSON.stringify(spec))), spec);
  draft.packages.splice(0, 1);
  assert.equal(serializeResourceContent(draft).packages, undefined);
});

test("duplicate or blank package and architecture names are rejected", () => {
  const draft = parseResourceContentDraft('{"packages":{"gcc":{}}}');
  draft.packages.push({ name: " gcc ", extra: {}, defaults: {}, arches: [] });
  assert.throws(() => serializeResourceContent(draft), /invalidPackageRows/);
  draft.packages.pop();
  draft.packages[0].arches = [
    { name: "aarch64", resources: {} }, { name: " aarch64 ", resources: {} },
  ];
  assert.throws(() => serializeResourceContent(draft), /invalidArchRows/);
  draft.packages[0].arches[1].name = "";
  assert.throws(() => serializeResourceContent(draft), /invalidArchRows/);
  draft.packages[0].name = "";
  assert.throws(() => serializeResourceContent(draft), /invalidPackageRows/);
});

test("malformed JSON or structures cannot silently discard data in list mode", () => {
  for (const source of ["{", "null", "[]", '{"packages":[]}', '{"packages":{"gcc":null}}',
    '{"default":{"requests":{"cpu":4}}}', '{"packages":{"gcc":{"arches":[]}}}']) {
    assert.throws(() => parseResourceContentDraft(source));
  }
});

test("special keys are retained as own properties", () => {
  const spec = JSON.parse('{"packages":{"__proto__":{"arches":{"constructor":{"requests":{"cpu":"1"}}}}}}');
  assert.deepEqual(serializeResourceContent(parseResourceContentDraft(JSON.stringify(spec))), spec);
});
