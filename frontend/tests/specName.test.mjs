import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/utils/specName.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { encodeSpecName, decodeSpecName } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

test("SPEC names use the same reversible label encoding as the controller", () => {
  for (const [name, encoded] of [
    ["gcc", "gcc"],
    ["dvd+rw-tools", "dvd_2Brw-tools"],
    ["-leading", "X_2Dleading"],
    ["Xray", "X_58ray"],
    ["中文+包", "X_E4_B8_AD_E6_96_87_2B_E5_8C_85"],
  ]) {
    assert.equal(encodeSpecName(name), encoded);
    assert.equal(decodeSpecName(encoded), name);
  }
  assert.equal(decodeSpecName("X_41"), null);
});
