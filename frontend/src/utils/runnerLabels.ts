export type RunnerLabelRow = { key: string; value: string };

export function isSystemRunnerLabel(key: string): boolean {
  return key === "ebs.io/runner-type" || key === "ebs.io/runner-arch" || key.startsWith("ebs.io/runner-capability.");
}

export function runnerLabelRows(labels: Record<string, string> = {}): RunnerLabelRow[] {
  return Object.entries(labels).sort(([a], [b]) => a.localeCompare(b)).map(([key, value]) => ({ key, value }));
}

const labelPart = /^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$/;
const dnsPart = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

export function validRunnerLabelKey(key: string): boolean {
  const parts = key.split("/");
  if (parts.length > 2) return false;
  const name = parts.at(-1)!;
  if (name.length > 63 || !labelPart.test(name)) return false;
  if (parts.length === 1) return true;
  return parts[0].length <= 253 && parts[0].split(".").every(part => part.length <= 63 && dnsPart.test(part));
}

// Only patch edited keys. Preserve concurrent changes to unrelated labels and reject conflicting edits.
export function runnerLabelPatch(
  original: Record<string, string>, rows: RunnerLabelRow[], current: Record<string, string>,
): { labels: Record<string, string | null>; error?: string } {
  const next = new Map<string, string>();
  for (const { key, value } of rows) {
    if (!validRunnerLabelKey(key) || value.length > 63 || (value !== "" && !labelPart.test(value)) || next.has(key)) {
      return { labels: {}, error: "operations.invalidRunnerLabels" };
    }
    next.set(key, value);
  }
  const labels: Record<string, string | null> = Object.create(null);
  for (const key of new Set([...Object.keys(original), ...next.keys()])) {
    const oldValue = Object.hasOwn(original, key) ? original[key] : undefined;
    const value = next.get(key);
    if (oldValue === value) continue;
    if (isSystemRunnerLabel(key)) return { labels: {}, error: "operations.systemRunnerLabelsReadOnly" };
    const currentValue = Object.hasOwn(current, key) ? current[key] : undefined;
    if (currentValue !== oldValue && currentValue !== value) return { labels: {}, error: "errors.conflict" };
    labels[key] = value ?? null;
  }
  return { labels };
}
