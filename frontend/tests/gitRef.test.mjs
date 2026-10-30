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
const { formatGitRef, isFullGitRef, toFullGitRef, toGitRefInput, toPackageRefFields, toPackageRefInput } = await import(gitRefURL);
const { projectFromForm, projectFromYaml } = await import(moduleURL("../src/utils/projectManifest.ts", [
  ['"./gitRef"', JSON.stringify(gitRefURL)],
  ['"yaml"', JSON.stringify(import.meta.resolve("yaml"))],
]));

test("full refs accept branches, tags and PR references", () => {
  for (const value of ["refs/heads/main", "refs/tags/v1.0", "refs/merge-requests/501/head", "refs/heads/版本+1"]) {
    assert.equal(isFullGitRef(value), true, value);
  }
});

test("ref labels show Branch/Tag names and preserve other full refs", () => {
  for (const [ref, label] of [
    ["refs/heads/openEuler-24.03-LTS-SP4", "Branch/openEuler-24.03-LTS-SP4"],
    ["refs/heads/release/main", "Branch/release/main"],
    ["refs/tags/v1", "Tag/v1"],
    ["refs/merge-requests/501/head", "refs/merge-requests/501/head"],
    ["", ""],
  ]) assert.equal(formatGitRef(ref), label);
  assert.equal(formatGitRef(), "");
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

test("package ref inputs roundtrip Branch, Tag and Commit ID", () => {
  const commitID = "a".repeat(40);
  for (const [input, fields] of [
    [{ type: "Branch", value: "main" }, { ref: "refs/heads/main" }],
    [{ type: "Tag", value: "v1" }, { ref: "refs/tags/v1" }],
    [{ type: "Commit", value: commitID }, { commitID }],
    [{ type: "Branch", value: "" }, {}],
    [{ type: "Branch", value: "refs/merge-requests/501/head" }, { ref: "refs/merge-requests/501/head" }],
  ]) {
    assert.deepEqual(JSON.parse(JSON.stringify(toPackageRefFields(input))), fields);
    assert.deepEqual(toPackageRefInput(fields), input);
  }
  assert.deepEqual(toPackageRefInput({ ref: "refs/heads/main", commitID }), { type: "Commit", value: commitID });
});

test("switching package ref type clears the previously selected API field", () => {
  const commitID = "a".repeat(40);
  let repo = { name: "gcc", ref: "refs/heads/main" };
  repo = { ...repo, ...toPackageRefFields({ type: "Commit", value: ` ${commitID} ` }) };
  assert.deepEqual(JSON.parse(JSON.stringify(repo)), { name: "gcc", commitID });
  repo = { ...repo, ...toPackageRefFields({ type: "Tag", value: "v1" }) };
  assert.deepEqual(JSON.parse(JSON.stringify(repo)), { name: "gcc", ref: "refs/tags/v1" });
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
    os: "openEuler", arch: "aarch64", ownerUser: "user",
  });
  assert.equal(project.spec.defaultRef, "refs/tags/v1");
  assert.equal(project.spec.distro, "openEuler");
  assert.deepEqual(project.spec.buildArches, ["aarch64"]);
  assert.equal(JSON.parse(JSON.stringify(project)).spec.defaultRef, "refs/tags/v1");
});

test("Project YAML preserves full refs and empty package refs", () => {
  const project = projectFromYaml(`apiVersion: ebs/v1
kind: Project
metadata:
  name: demo
spec:
  defaultRef: refs/heads/main
  distro: openEuler
  buildArches:
    - aarch64
  packageRepos:
    - name: gcc
      ref: refs/merge-requests/501/head
      commitID: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    - name: rpm
      ref: ""
`);
  assert.equal(project.spec.defaultRef, "refs/heads/main");
  assert.equal(project.spec.distro, "openEuler");
  assert.deepEqual(project.spec.buildArches, ["aarch64"]);
  assert.deepEqual(project.spec.packageRepos, [
    { name: "gcc", ref: "refs/merge-requests/501/head", commitID: "a".repeat(40) }, { name: "rpm", ref: "" },
  ]);
});
