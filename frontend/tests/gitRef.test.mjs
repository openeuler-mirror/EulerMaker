import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

function moduleURL(path, replacements = []) {
  let source = readFileSync(new URL(path, import.meta.url), "utf8");
  for (const [from, to] of replacements) source = source.replace(from, to);
  const { outputText } = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
  });
  return "data:text/javascript;base64," + Buffer.from(outputText).toString("base64");
}

const gitRefURL = moduleURL("../src/utils/gitRef.ts");
const { isFullGitRef, toFullGitRef, toGitRefInput } = await import(gitRefURL);
const { projectFromForm, projectFromYaml } = await import(moduleURL("../src/utils/projectManifest.ts", [
  ['"./gitRef"', JSON.stringify(gitRefURL)],
  ['"yaml"', JSON.stringify(import.meta.resolve("yaml"))],
]));

test("full refs accept branches, tags and PR references", () => {
  for (const value of ["refs/heads/main", "refs/tags/v1.0", "refs/merge-requests/501/head", "refs/heads/版本+1"]) {
    assert.equal(isFullGitRef(value), true, value);
  }
});

test("Branch and Tag form values roundtrip through full ref strings", () => {
  for (const [input, fullRef] of [
    [{ type: "Branch", value: "release/main" }, "refs/heads/release/main"],
    [{ type: "Tag", value: "v1" }, "refs/tags/v1"],
    [{ type: "Branch", value: "" }, ""],
    [{ type: "Branch", value: "refs/merge-requests/501/head" }, "refs/merge-requests/501/head"],
  ]) {
    assert.equal(toFullGitRef(input), fullRef);
    assert.deepEqual(toGitRefInput(fullRef), input);
  }
  assert.equal(toFullGitRef({ type: "Branch", value: "refs/heads/main" }), "refs/heads/main");
  assert.equal(toFullGitRef({ type: "Tag", value: "refs/tags/v1" }), "refs/tags/v1");
});

test("full refs enforce Git ref-name syntax", () => {
  for (const value of ["main", "a".repeat(40), "refs/", "refs/heads/a..b", "refs/heads/.hidden",
    "refs/heads/a.lock", "refs/heads/a b", "refs//main", "refs/heads/a.", "refs/heads/a@{1}",
    "refs/heads/a\n", "refs/heads/a\\b", "refs/heads/a^{commit}", "refs/heads/a?b"]) {
    assert.equal(isFullGitRef(value), false, value);
  }
});

test("Project form stores the default ref as a complete string", () => {
  const project = projectFromForm({
    name: "demo", displayName: "Demo", description: "", defaultRef: { type: "Tag", value: "v1" },
    os: "openEuler", arch: "aarch64", buildFlag: true, publishFlag: false, ownerUser: "user",
  });
  assert.equal(project.spec.defaultRef, "refs/tags/v1");
  assert.equal(JSON.parse(JSON.stringify(project)).spec.defaultRef, "refs/tags/v1");
});

test("Project YAML preserves full refs and empty package refs", () => {
  const project = projectFromYaml(`apiVersion: ebs/v1
kind: Project
metadata:
  name: demo
spec:
  defaultRef: refs/heads/main
  buildTargets:
    - os: openEuler
      arch: aarch64
  packageRepos:
    - name: gcc
      ref: refs/merge-requests/501/head
      commitID: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    - name: rpm
      ref: ""
`);
  assert.equal(project.spec.defaultRef, "refs/heads/main");
  assert.deepEqual(project.spec.packageRepos, [
    { name: "gcc", ref: "refs/merge-requests/501/head", commitID: "a".repeat(40) }, { name: "rpm", ref: "" },
  ]);
});
