import type { BuildConf, BuildTarget } from "@/types";

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

// Keep all fields when changing editor modes; the server performs authoritative
// field validation, rather than silently dropping unknown data here.
export function buildConfSpec(value: unknown): BuildConf["spec"] {
  if (!isRecord(value) || !isRecord(value.targets)) throw new Error("buildConf.invalid");
  for (const target of Object.values(value.targets)) {
    if (!isRecord(target) || !isRecord(target.arches)) throw new Error("buildConf.invalid");
    for (const arch of Object.values(target.arches)) {
      if (!isRecord(arch) || typeof arch.image !== "string") throw new Error("buildConf.invalid");
    }
  }
  return value as BuildConf["spec"];
}

export function configuredOS(conf: BuildConf | null): string[] {
  return Object.keys(conf?.spec.targets || {}).sort();
}

export function configuredArches(conf: BuildConf | null, os?: string): string[] {
  const targets = conf?.spec.targets;
  if (!targets || !os || !Object.hasOwn(targets, os)) return [];
  return Object.keys(targets[os].arches).sort();
}

export function supportsTarget(conf: BuildConf | null, target: BuildTarget): boolean {
  if (!target.os || !target.arch || !configuredArches(conf, target.os).includes(target.arch)) return false;
  return Boolean(conf!.spec.targets[target.os].arches[target.arch].image);
}
