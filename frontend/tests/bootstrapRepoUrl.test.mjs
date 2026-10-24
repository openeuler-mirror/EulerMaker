import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/utils/bootstrapRepoUrl.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { isValidBootstrapRepoUrl } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

test("accepts HTTP(S) bootstrap repository base URLs", () => {
  assert.equal(isValidBootstrapRepoUrl("https://example.com/everything"), true);
  assert.equal(isValidBootstrapRepoUrl("http://localhost:8080/repos/"), true);
});

test("rejects invalid or non-base bootstrap repository URLs", () => {
  for (const value of [
    "", "example.com/repo", "ftp://example.com/repo", "https://", "https://example.com:bad/repo",
    "https://user:pass@example.com/repo", "https://example.com/repo?arch=x86_64",
    "https://example.com/repo#fragment", "https://example.com/repo name",
  ]) {
    assert.equal(isValidBootstrapRepoUrl(value), false, value);
  }
});

test("accepts artifact bootstrap references and rejects invalid logical paths", () => {
  for (const value of ["artifact://art-01/repositories/project/os", "artifact:///repositories/project/os/", "artifact://default/repositories/openEuler%3A24/"]) {
    assert.equal(isValidBootstrapRepoUrl(value), true, value);
  }
  for (const value of ["artifact://art-01/other/repo", "artifact:///repositories/../secret", "artifact:///repositories/%2e%2e/secret", "artifact:///repositories/a//b", "artifact://user:pass@art/repositories/a", "artifact://art:80/repositories/a", "artifact:///repositories/a?x=1", "artifact:///repositories/a#x"]) {
    assert.equal(isValidBootstrapRepoUrl(value), false, value);
  }
});
