import type { BuildTargetContent, ConfiguredBuildTarget } from "@/types";

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

// Keep all fields when changing editor modes; the server performs authoritative
// field validation, rather than silently dropping unknown data here.
export function parseBuildTargetContent(value: unknown): BuildTargetContent {
  if (!isRecord(value) || !isRecord(value.targets)) throw new Error("buildTargetConfig.invalid");
  for (const target of Object.values(value.targets)) {
    if (!isRecord(target) || !isRecord(target.arches)) throw new Error("buildTargetConfig.invalid");
    for (const arch of Object.values(target.arches)) {
      if (!isRecord(arch) || typeof arch.image !== "string") throw new Error("buildTargetConfig.invalid");
    }
  }
  return value as unknown as BuildTargetContent;
}

export function supportsTarget(conf: BuildTargetContent | null, target: ConfiguredBuildTarget): boolean {
  if (!conf || !target.os || !target.arch || !Object.hasOwn(conf.targets, target.os)) return false;
  const arches = conf.targets[target.os].arches;
  return Object.hasOwn(arches, target.arch) && Boolean(arches[target.arch].image);
}
