import { parse } from "yaml";

import type { BuildTarget, Project } from "@/types";

const NAME_PATTERN = /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/;

export type ProjectValidationKey =
  | "projects.invalidDocument"
  | "projects.invalidApiVersion"
  | "projects.invalidKind"
  | "projects.invalidName"
  | "projects.reservedName"
  | "projects.targetRequired";

export interface BasicProjectForm {
  name: string;
  displayName: string;
  description: string;
  defaultRef: { type: "Branch" | "Tag"; value: string };
  os: string;
  arch: string;
  buildFlag: boolean;
  publishFlag: boolean;
  ownerUser: string;
}

export class ProjectManifestError extends Error {
  constructor(readonly translationKey: ProjectValidationKey) {
    super(translationKey);
  }
}

export function projectFromForm(form: BasicProjectForm): Project {
  const name = form.name.trim();
  validateName(name);
  const target = normalizeTarget({
    os: form.os,
    arch: form.arch,
    buildFlag: form.buildFlag,
    publishFlag: form.publishFlag,
  });
  return {
    apiVersion: "ebs/v1",
    kind: "Project",
    metadata: {
      name,
      ...(form.ownerUser.trim() ? { labels: { "ebs.io/owner-user": form.ownerUser.trim() } } : {}),
    },
    spec: {
      displayName: form.displayName.trim() || name,
      description: form.description.trim(),
      defaultRef: { type: form.defaultRef.type, value: form.defaultRef.value.trim() },
      buildTargets: [target],
    },
  };
}

export function projectFromYaml(source: string): Project {
  let input: unknown;
  try {
    input = parse(source);
  } catch {
    throw new ProjectManifestError("projects.invalidDocument");
  }
  if (!isRecord(input)) throw new ProjectManifestError("projects.invalidDocument");
  if (input.apiVersion !== "ebs/v1") throw new ProjectManifestError("projects.invalidApiVersion");
  if (input.kind !== "Project") throw new ProjectManifestError("projects.invalidKind");

  const metadata = isRecord(input.metadata) ? input.metadata : {};
  const spec = isRecord(input.spec) ? input.spec : {};
  const name = typeof metadata.name === "string" ? metadata.name.trim() : "";
  validateName(name);
  const targets = Array.isArray(spec.buildTargets) ? spec.buildTargets.map(normalizeTarget) : [];
  if (!targets.length) throw new ProjectManifestError("projects.targetRequired");

  const project = structuredClone(input) as Project;
  project.apiVersion = "ebs/v1";
  project.kind = "Project";
  project.metadata = {
    name,
    ...(isStringMap(metadata.labels) ? { labels: metadata.labels } : {}),
    ...(isStringMap(metadata.annotations) ? { annotations: metadata.annotations } : {}),
  };
  project.spec = { ...spec, buildTargets: targets } as Project["spec"];
  delete project.status;
  return project;
}

function validateName(name: string): void {
  if (!name || name.length > 63 || !NAME_PATTERN.test(name)) {
    throw new ProjectManifestError("projects.invalidName");
  }
  if (name === "default") throw new ProjectManifestError("projects.reservedName");
}

function normalizeTarget(value: unknown): BuildTarget {
  if (!isRecord(value)) throw new ProjectManifestError("projects.targetRequired");
  const os = typeof value.os === "string" ? value.os.trim() : "";
  const arch = typeof value.arch === "string" ? value.arch.trim() : "";
  if (!os || !arch) throw new ProjectManifestError("projects.targetRequired");
  return {
    os,
    arch,
    ...(typeof value.buildFlag === "boolean" ? { buildFlag: value.buildFlag } : {}),
    ...(typeof value.publishFlag === "boolean" ? { publishFlag: value.publishFlag } : {}),
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isStringMap(value: unknown): value is Record<string, string> {
  return isRecord(value) && Object.values(value).every((item) => typeof item === "string");
}
