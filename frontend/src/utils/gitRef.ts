export interface GitRefInput {
  type: "Branch" | "Tag";
  value: string;
}

export type PackageRefInput = GitRefInput | { type: "Commit"; value: string };

export function toPackageRefInput(repo: { ref?: string; commitID?: string }): PackageRefInput {
  return repo.commitID ? { type: "Commit", value: repo.commitID } : toGitRefInput(repo.ref);
}

export function toPackageRefFields(input: PackageRefInput): { ref?: string; commitID?: string } {
  return input.type === "Commit"
    ? { ref: undefined, commitID: input.value.trim() || undefined }
    : { ref: toFullGitRef(input) || undefined, commitID: undefined };
}

export function toFullGitRef(ref: GitRefInput): string {
  const value = ref.value.trim();
  if (!value || value.startsWith("refs/")) return value;
  return `refs/${ref.type === "Tag" ? "tags" : "heads"}/${value}`;
}

export function toGitRefInput(ref = ""): GitRefInput {
  if (ref.startsWith("refs/tags/")) return { type: "Tag", value: ref.slice("refs/tags/".length) };
  if (ref.startsWith("refs/heads/")) return { type: "Branch", value: ref.slice("refs/heads/".length) };
  return { type: "Branch", value: ref };
}

export function formatGitRef(ref = ""): string {
  if (ref.startsWith("refs/heads/") || ref.startsWith("refs/tags/")) {
    const { type, value } = toGitRefInput(ref);
    return `${type}/${value}`;
  }
  return ref;
}

export function isFullGitRef(value: string): boolean {
  if (!value.startsWith("refs/") || value.endsWith(".") || value.includes("..") || value.includes("@{")
    || /[\x00-\x20\x7f~^:?*\[\\]/.test(value)) return false;
  return value.split("/").every(part => part !== "" && !part.startsWith(".") && !part.endsWith(".lock"));
}
