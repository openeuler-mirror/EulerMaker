import type { BuildTargetContent, BuildTarget } from "@/types";

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

export function configuredOS(conf: BuildTargetContent | null): string[] {
  return Object.keys(conf?.targets || {}).sort();
}

export function configuredArches(conf: BuildTargetContent | null, os?: string): string[] {
  const targets = conf?.targets;
  if (!targets || !os || !Object.hasOwn(targets, os)) return [];
  return Object.keys(targets[os].arches).sort();
}

export function supportsTarget(conf: BuildTargetContent | null, target: BuildTarget): boolean {
  if (!target.os || !target.arch || !configuredArches(conf, target.os).includes(target.arch)) return false;
  return Boolean(conf!.targets[target.os].arches[target.arch].image);
}
