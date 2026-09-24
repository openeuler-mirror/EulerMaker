export type JSONMap = Record<string, unknown>;
export interface ArchRow { name: string; resources: JSONMap }
export interface PackageRow { name: string; extra: JSONMap; defaults: JSONMap; arches: ArchRow[] }
export interface ResourceDraft { extra: JSONMap; defaults: JSONMap; packages: PackageRow[] }

function object(value: unknown): JSONMap {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("operations.invalidContent");
  return value as JSONMap;
}
function optionalObject(value: unknown): JSONMap { return value === undefined ? {} : object(value); }
function requirements(value: unknown): JSONMap {
  const result = optionalObject(value);
  for (const key of ["requests", "limits"]) {
    for (const quantity of Object.values(optionalObject(result[key]))) {
      if (typeof quantity !== "string") throw new Error("operations.listUnsupported");
    }
  }
  return result;
}
export function parseResourceContentDraft(source: string): ResourceDraft {
  let content: JSONMap;
  try { content = object(JSON.parse(source)); } catch { throw new Error("operations.invalidContent"); }
  const { default: defaults, packages, ...extra } = content;
  return {
    extra, defaults: requirements(defaults),
    packages: Object.entries(optionalObject(packages)).map(([name, value]) => {
      const { default: defaults, arches, ...extra } = object(value);
      return { name, extra, defaults: requirements(defaults),
        arches: Object.entries(optionalObject(arches)).map(([name, value]) => ({ name, resources: requirements(value) })) };
    }),
  };
}
export function serializeResourceContent(draft: ResourceDraft): JSONMap {
  const names = new Set<string>();
  const packages = draft.packages.map(pkg => {
    const name = pkg.name.trim();
    if (!name || names.has(name)) throw new Error("operations.invalidPackageRows");
    names.add(name);
    const arches = new Set<string>();
    const entries = pkg.arches.map(arch => {
      const name = arch.name.trim();
      if (!name || arches.has(name)) throw new Error("operations.invalidArchRows");
      arches.add(name);
      return [name, arch.resources];
    });
    return [name, { ...pkg.extra,
      ...(Object.keys(pkg.defaults).length ? { default: pkg.defaults } : {}),
      ...(entries.length ? { arches: Object.fromEntries(entries) } : {}),
    }];
  });
  return { ...draft.extra,
    ...(Object.keys(draft.defaults).length ? { default: draft.defaults } : {}),
    ...(packages.length ? { packages: Object.fromEntries(packages) } : {}),
  };
}
