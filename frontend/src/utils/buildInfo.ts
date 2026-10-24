import type { BuildInfo, Snapshot } from "../types";

export function buildInfoSpecRows(buildInfo: BuildInfo | null, snapshot: Snapshot | null) {
  return Object.entries(buildInfo?.status?.specResults || {})
    .map(([key, value]) => {
      const packageName = key.split("/", 1)[0];
      const name = value.specName || key;
      return {
        key,
        name,
        specName: value.specName,
        reason: value.reason,
        message: value.message,
        packageName,
        commitID: packageName ? snapshot?.status?.packageRepoStatuses?.[packageName]?.commitID : undefined,
        buildStatus: value.status,
        displayStatus: value.status === "BuildFailed" &&
          value.reason === "RpmDependsMissing"
          ? "RpmDependsMissing" : value.status,
      };
    })
    .sort((left, right) => left.name.localeCompare(right.name));
}
