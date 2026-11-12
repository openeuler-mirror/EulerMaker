import type { Build } from "@/types";

export function buildDisplayStatus(build: Build): string | undefined {
  if (build.metadata?.deletionTimestamp) return "Deleting";
  const { phase, stage, conditions } = build.status || {};
  if (phase === "Aborted") return "Aborted";
  const result = conditions?.find(condition => condition.type === "BuildSucceed")?.status;
  if (result === "True") return "BuildCompleted";
  if (result === "False") return "BuildFailed";
  // The publish phase reports the release outcome, not the package build result.
  if (stage === "publish") return undefined;
  if (phase === "Success") return "BuildCompleted";
  if (phase === "Failed") return "BuildFailed";
  if (phase === "Processing" && stage === "build") return "Building";
  return phase;
}

export function buildPublishStatus(build: Build): string | undefined {
  if (build.spec?.buildType === "single") return undefined;
  if (build.spec?.publish === false) return "NotPublishing";
  const { phase, stage, conditions } = build.status || {};
  const result = conditions?.find(condition => condition.type === "PublishSucceed")?.status;
  if (result === "True") return "Published";
  if (result === "False") return "PublishFailed";
  if (phase === "Aborted") return "NotPublished";
  if (stage === "publish") {
    if (phase === "Success") return "Published";
    if (phase === "Failed") return "PublishFailed";
    if (phase === "Processing") return "Publishing";
  }
  if (phase === "Failed" || build.metadata?.deletionTimestamp) return "NotPublished";
  return "PublishPending";
}
