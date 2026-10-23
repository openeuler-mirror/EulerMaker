import type { BuildInfo, Snapshot } from "../types";

export function buildInfoSpecRows(buildInfo: BuildInfo | null, snapshot: Snapshot | null) {
  return Object.entries(buildInfo?.status?.specStatus?.build || {})
    .map(([name, value]) => {
      const packageName = buildInfo?.status?.specRepoNames?.[name];
      return {
        name,
        packageName,
        commitID: packageName ? snapshot?.status?.packageRepoStatuses?.[packageName]?.commitID : undefined,
        buildStatus: value.status,
        displayStatus: value.status === "Failed" &&
          value.conditions?.some(condition => condition.reason === "RpmDependsMissing")
          ? "RpmDependsMissing" : value.status,
      };
    })
    .sort((left, right) => left.name.localeCompare(right.name));
}
