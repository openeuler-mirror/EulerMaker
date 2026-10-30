import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { parse } from "@vue/compiler-sfc";
import { computed, ref } from "vue";
import ts from "typescript";

const component = readFileSync(new URL("../src/views/ProjectView.vue", import.meta.url), "utf8");
const { descriptor } = parse(component);
const source = ts.createSourceFile("ProjectView.ts", descriptor.scriptSetup.content, ts.ScriptTarget.Latest, true);
const functions = source.statements.filter(statement => ts.isFunctionDeclaration(statement)
  && ["completeBuildGitRef", "openBuildDialog", "closeBuildDialog", "createBuild", "openTargetEditor", "addTarget", "saveTargets"].includes(statement.name?.text));
assert.equal(functions.length, 7);
const { outputText } = ts.transpileModule(functions.map(statement => statement.getText(source)).join("\n"), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
});

function setup() {
  const requests = [];
  let nextBuild = 0;
  const state = {
    project: ref({ spec: {
      packageRepos: [{ name: "gcc" }, { name: "rpm" }],
      distro: "openEuler", buildArches: ["x86_64", "aarch64"],
    } }),
    canEditProject: ref(true), editingTargets: ref([]), targetEditorOpen: ref(false), savingTargets: ref(false),
    targetSaveSuccess: ref(false), targetErrorKey: ref(""), targetErrorParams: ref({}),
    canStartBuild: ref(true), projectIsFull: ref(true), buildConfigurationMissing: ref(false),
    buildType: ref("full"), fixedBuildPackage: ref(""), buildGitRef: ref(""), selectedBuildPackages: ref([]),
    buildPackageSearch: ref(""), buildTargetDrafts: ref([]), creatingBuild: ref(false),
    buildDialogErrorKey: ref(""), buildActionErrorKey: ref(""), createdBuildNames: ref([]),
    abortedBuildName: ref(""), buildDialogOpen: ref(false), unsupportedBuildSelection: ref(false),
    selectedBuildName: ref(""), name: ref("example"),
    reloadBuildTargetConfig: async () => {}, loadProject: async () => true, loadResources: async () => {},
    selectTab: () => {}, createBuildName: () => `build-${++nextBuild}`,
    ApiError: class extends Error {}, errorTranslationKey: () => "errors.createBuild",
    request: async (url, options) => {
      const body = JSON.parse(options.body);
      requests.push({ url, method: options.method, body });
      return body;
    },
  };
  state.requiresBuildPackages = computed(() => ["single", "specified"].includes(state.buildType.value));
  state.availableBuildPackages = computed(() => state.project.value.spec.packageRepos.map(repo => repo.name));
  const actions = new Function(...Object.keys(state), outputText
    + "\nreturn { completeBuildGitRef, openBuildDialog, closeBuildDialog, createBuild, openTargetEditor, addTarget, saveTargets };")(...Object.values(state));
  return { state, actions, requests };
}

test("architecture editing preserves the project distribution", async () => {
  const { state, actions, requests } = setup();
  actions.openTargetEditor();
  assert.equal(state.targetEditorOpen.value, true);
  state.editingTargets.value = [{ os: "openEuler", arch: "aarch64" }];
  actions.addTarget();
  assert.equal(state.editingTargets.value[1].os, "openEuler");
  state.editingTargets.value[1].arch = "riscv64";
  await actions.saveTargets();
  assert.equal(requests.length, 1);
  assert.equal(requests[0].method, "PUT");
  assert.deepEqual(requests[0].body.spec, {
    distro: "openEuler", buildArches: ["aarch64", "riscv64"],
    packageRepos: [{ name: "gcc" }, { name: "rpm" }],
  });
  assert.equal(state.targetSaveSuccess.value, true);
});

test("Tab completes an empty Git ref from its placeholder and then allows normal focus navigation", () => {
  const { state, actions } = setup();
  let prevented = 0;
  const event = {
    currentTarget: { placeholder: "refs/merge-requests/501/head" },
    preventDefault: () => { prevented++; },
  };
  actions.completeBuildGitRef(event);
  assert.equal(state.buildGitRef.value, event.currentTarget.placeholder);
  assert.equal(prevented, 1);
  state.buildGitRef.value = "refs/merge-requests/512/head";
  actions.completeBuildGitRef(event);
  assert.equal(state.buildGitRef.value, "refs/merge-requests/512/head");
  assert.equal(prevented, 1);
});

test("package action fixes one package and submits single builds for the selected targets", async () => {
  const { state, actions, requests } = setup();
  await actions.openBuildDialog("single", "gcc");
  assert.equal(state.buildDialogOpen.value, true);
  assert.equal(state.fixedBuildPackage.value, "gcc");
  assert.equal(state.buildGitRef.value, "");
  assert.deepEqual(state.selectedBuildPackages.value, ["gcc"]);
  await actions.createBuild();
  assert.equal(requests.length, 2);
  assert.deepEqual(requests.map(request => request.body.spec.arch), ["x86_64", "aarch64"]);
  for (const request of requests) {
    assert.equal(request.method, "POST");
    assert.equal(request.url, "/apis/ebs/v1/projects/example/builds");
    assert.equal(request.body.spec.buildType, "single");
    assert.equal(request.body.spec.publish, undefined);
    assert.deepEqual(request.body.spec.packages, ["gcc"]);
    assert.equal(request.body.metadata.annotations, undefined);
  }
  assert.equal(state.buildDialogOpen.value, false);
  assert.deepEqual(state.createdBuildNames.value, ["build-1", "build-2"]);
});

test("package build respects target selection and resets when a general build dialog is opened", async () => {
  const { state, actions, requests } = setup();
  await actions.openBuildDialog("single", "rpm");
  state.buildTargetDrafts.value[1].selected = false;
  await actions.createBuild();
  assert.equal(requests.length, 1);
  assert.deepEqual(requests[0].body.spec.packages, ["rpm"]);
  await actions.openBuildDialog("single");
  assert.equal(state.fixedBuildPackage.value, "");
  assert.deepEqual(state.selectedBuildPackages.value, []);
  state.selectedBuildPackages.value = ["gcc", "rpm"];
  await actions.createBuild();
  assert.deepEqual(requests[1].body.spec.packages, ["gcc", "rpm"]);
  await actions.openBuildDialog("full");
  await actions.createBuild();
  assert.equal(requests.at(-1).body.spec.buildType, "full");
  assert.equal(requests.at(-1).body.spec.packages, undefined);
});

test("non-single builds default to publishing and allow opting out per target", async () => {
  for (const type of ["full", "incremental", "specified"]) {
    const { state, actions, requests } = setup();
    await actions.openBuildDialog(type);
    assert.deepEqual(state.buildTargetDrafts.value.map(target => target.publish), [true, true]);
    state.selectedBuildPackages.value = ["gcc"];
    await actions.createBuild();
    assert.deepEqual(requests.map(request => request.body.spec.publish), [true, true]);
    assert.equal(requests[0].body.spec.arch, "x86_64");
    await actions.openBuildDialog(type);
    state.selectedBuildPackages.value = ["gcc"];
    state.buildTargetDrafts.value[0].publish = false;
    await actions.createBuild();
    assert.deepEqual(requests.slice(2).map(request => request.body.spec.publish), [false, true]);
    assert.deepEqual(requests.slice(2).map(request => request.body.spec.arch), ["x86_64", "aarch64"]);
    await actions.openBuildDialog(type);
    assert.deepEqual(state.buildTargetDrafts.value.map(target => target.publish), [true, true]);
  }
});

test("package action uses the same build permission and configuration checks as other build entry points", async () => {
  const { state, actions, requests } = setup();
  state.canStartBuild.value = false;
  await actions.openBuildDialog("single", "gcc");
  assert.equal(state.buildDialogOpen.value, false);
  await actions.createBuild();
  assert.equal(requests.length, 0);
  state.canStartBuild.value = true;
  state.buildConfigurationMissing.value = true;
  await actions.openBuildDialog("single", "gcc");
  assert.equal(state.buildDialogOpen.value, false);
});

test("single-package builds attach the optional full Git ref to every selected target and reset it on reopen", async () => {
  const { state, actions, requests } = setup();
  await actions.openBuildDialog("single", "gcc");
  state.buildGitRef.value = "refs/merge-requests/501/head";
  await actions.createBuild();
  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.deepEqual(request.body.metadata.annotations, { "ebs.io/git-ref": "refs/merge-requests/501/head" });
    assert.equal(request.body.spec.buildType, "single");
    assert.deepEqual(request.body.spec.packages, ["gcc"]);
  }
  await actions.openBuildDialog("single", "rpm");
  assert.equal(state.buildGitRef.value, "");
  state.buildGitRef.value = "refs/heads/test";
  actions.closeBuildDialog();
  await actions.openBuildDialog("full");
  assert.equal(state.buildGitRef.value, "");
  await actions.createBuild();
  assert.equal(requests.at(-1).body.metadata.annotations, undefined);
});

test("general single-build dialog submits selected packages without a Git ref override", async () => {
  const { state, actions, requests } = setup();
  await actions.openBuildDialog("single", "gcc");
  state.buildGitRef.value = "refs/heads/test";
  actions.closeBuildDialog();
  await actions.openBuildDialog("single");
  assert.equal(state.fixedBuildPackage.value, "");
  assert.equal(state.buildGitRef.value, "");
  state.selectedBuildPackages.value = ["gcc", "rpm"];
  await actions.createBuild();
  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.deepEqual(request.body.spec.packages, ["gcc", "rpm"]);
    assert.equal(request.body.metadata.annotations, undefined);
  }
});
