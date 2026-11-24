import { PROJECT_TYPE_LABEL } from "./projectType";

export const PROJECT_OWNER_LABEL = "ebs.io/owner-user";
export const PROJECT_PURPOSE_LABEL = "ebs.io/build-purpose";
export type ProjectLabelRow = { key: string; value: string };

export function projectLabelReadOnly(key: string, role: string, original: Record<string, string>): boolean {
  if (key === PROJECT_OWNER_LABEL) return true;
  if (role === "admin") return key !== PROJECT_PURPOSE_LABEL;
  if (role === "ops") return false;
  if (role !== "user") return true;
  return key === PROJECT_PURPOSE_LABEL || (key === PROJECT_TYPE_LABEL && Object.hasOwn(original, key));
}

const labelPart = /^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$/;
const dnsPart = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

function validKey(key: string): boolean {
  const parts = key.split("/");
  const name = parts.at(-1)!;
  return parts.length <= 2 && name.length <= 63 && labelPart.test(name) &&
    (parts.length === 1 || (parts[0].length <= 253 &&
      parts[0].split(".").every(part => part.length <= 63 && dnsPart.test(part))));
}

// Preserve unrelated label changes; reject edits to keys changed by another writer.
export function projectLabelPatch(original: Record<string, string>, rows: ProjectLabelRow[], current: Record<string, string>, role: string): { labels: Record<string, string | null>; error?: string } {
  const next = new Map<string, string>();
  for (const { key, value } of rows) {
    if (!validKey(key) || value.length > 63 || (value !== "" && !labelPart.test(value)) || next.has(key)) {
      return { labels: {}, error: "project.invalidLabels" };
    }
    next.set(key, value);
  }
  const labels: Record<string, string | null> = Object.create(null);
  for (const key of new Set([...Object.keys(original), ...next.keys()])) {
    const oldValue = Object.hasOwn(original, key) ? original[key] : undefined;
    const value = next.get(key);
    if (oldValue === value) continue;
    if (projectLabelReadOnly(key, role, original) ||
      (role === "user" && key === PROJECT_TYPE_LABEL && value !== "personal")) {
      return { labels: {}, error: "project.labelsReadOnly" };
    }
    if ((key === PROJECT_TYPE_LABEL && value !== undefined && !["personal", "community"].includes(value)) ||
      (key === PROJECT_PURPOSE_LABEL && value !== undefined && value !== "gating") ||
      (key.startsWith("ebs.io/member-user.") && value !== undefined && value !== "true")) {
      return { labels: {}, error: "project.invalidLabelValue" };
    }
    const currentValue = Object.hasOwn(current, key) ? current[key] : undefined;
    if (currentValue !== oldValue && currentValue !== value) return { labels: {}, error: "errors.conflict" };
    labels[key] = value ?? null;
  }
  return { labels };
}
