import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/utils/buildStatus.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { buildDisplayStatus, buildPublishStatus } = await import(
  "data:text/javascript;base64," + Buffer.from(outputText).toString("base64")
);

for (const [name, phase, stage, publish, built, released, expectedBuild, expectedRelease] of [
  ["unpublished successful build", "Skipped", "publish", false, "True", undefined, "BuildCompleted", "NotPublishing"],
  ["unpublished failed build", "Skipped", "publish", false, "False", undefined, "BuildFailed", "NotPublishing"],
  ["waiting for preparation", "Pending", "prepare", true, undefined, undefined, "Pending", "PublishPending"],
  ["prepared", "Prepared", "prepare", true, undefined, undefined, "Prepared", "PublishPending"],
  ["building", "Processing", "build", true, undefined, undefined, "Building", "PublishPending"],
  ["publishing", "Processing", "publish", true, "True", undefined, "BuildCompleted", "Publishing"],
  ["publishing with failed packages", "Processing", "publish", true, "False", undefined, "BuildFailed", "Publishing"],
  ["published", "Success", "publish", true, "True", "True", "BuildCompleted", "Published"],
  ["published with failed packages", "Success", "publish", true, "False", "True", "BuildFailed", "Published"],
  ["publication failed", "Failed", "publish", true, "True", "False", "BuildCompleted", "PublishFailed"],
  ["preparation failed", "Failed", "prepare", true, undefined, undefined, "BuildFailed", "NotPublished"],
  ["aborted before publishing", "Aborted", "build", true, undefined, undefined, "Aborted", "NotPublished"],
  ["aborted during publishing", "Aborted", "publish", true, "True", undefined, "Aborted", "NotPublished"],
  ["publishing enabled by default", "Processing", "build", undefined, undefined, undefined, "Building", "PublishPending"],
  ["unknown build outcome is not inferred from publication", "Success", "publish", true, undefined, "True", undefined, "Published"],
]) {
  test(name, () => {
    const build = {
      spec: { buildType: "full", publish },
      status: { phase, stage, conditions: [
        { type: "BuildSucceed", status: built },
        { type: "PublishSucceed", status: released },
      ] },
    };
    assert.equal(buildDisplayStatus(build), expectedBuild);
    assert.equal(buildPublishStatus(build), expectedRelease);
  });
}

test("single builds only show their build result", () => {
  for (const [phase, status, expected] of [["Success", "True", "BuildCompleted"], ["Failed", "False", "BuildFailed"]]) {
    const build = { spec: { buildType: "single" }, status: { phase, stage: "build", conditions: [
      { type: "BuildSucceed", status },
    ] } };
    assert.equal(buildDisplayStatus(build), expected);
    assert.equal(buildPublishStatus(build), undefined);
  }
});

test("deletion remains the primary displayed status", () => {
  const build = {
    metadata: { deletionTimestamp: "2026-11-12T00:00:00Z" },
    spec: { buildType: "full", publish: false },
    status: { phase: "Skipped", stage: "publish", conditions: [{ type: "BuildSucceed", status: "True" }] },
  };
  assert.equal(buildDisplayStatus(build), "Deleting");
  assert.equal(buildPublishStatus(build), "NotPublishing");
});
