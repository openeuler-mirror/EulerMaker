export function formatRunnerMemory(value?: string): string | undefined {
  const match = value?.trim().match(/^(\d+(?:\.\d+)?)([KMGTPE]i?|k)?$/);
  if (!match) return undefined;
  const suffix = match[2] || "";
  const power = suffix ? "KMGTPE".indexOf(suffix[0].toUpperCase()) + 1 : 0;
  const bytes = Number(match[1]) * (suffix.endsWith("i") ? 1024 : 1000) ** power;
  if (!Number.isFinite(bytes)) return undefined;
  const unit = bytes >= 1024 ** 3 ? "GiB" : "MiB";
  const amount = bytes / 1024 ** (unit === "GiB" ? 3 : 2);
  return `${Number(amount.toFixed(2))} ${unit}`;
}
