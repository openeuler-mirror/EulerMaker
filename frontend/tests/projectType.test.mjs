import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/utils/projectType.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { projectTypeSelector } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

test("project tabs select before pagination and include unlabeled personal projects", () => {
  assert.equal(projectTypeSelector("community"), "project.ebs.io/type=community");
  assert.equal(projectTypeSelector("personal"), "project.ebs.io/type!=community");
});
