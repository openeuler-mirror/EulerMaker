import { list } from "@/api";
import type { Project } from "@/types";

// Kubernetes label selectors do not support OR across different keys.
// Fetch both filtered lists completely before merging and paginating locally.
export async function listRelatedProjects(username: string, typeSelector: string, isCurrent: () => boolean): Promise<Project[]> {
  if (!username) return [];
  const pages = await Promise.all([
    `ebs.io/owner-user=${username}`,
    `ebs.io/member-user.${username}=true`,
  ].map(async relation => {
    const items: Project[] = [];
    const seen = new Set<string>();
    let token = "";
    do {
      if (!isCurrent()) return [];
      const query = new URLSearchParams({ limit: "100", labelSelector: `${typeSelector},${relation}` });
      if (token) query.set("continue", token);
      const page = await list<Project>(`/apis/ebs/v1/projects?${query}`);
      items.push(...page.items);
      token = page.next;
      if (token && seen.has(token)) throw new Error("Repeated project continuation token");
      seen.add(token);
    } while (token);
    return items;
  }));
  const unique = new Map<string, Project>();
  for (const project of pages.flat()) {
    const labels = project.metadata?.labels || {};
    if (project.metadata?.name && (labels["ebs.io/owner-user"] === username || labels[`ebs.io/member-user.${username}`] === "true")) {
      unique.set(project.metadata.name, project);
    }
  }
  const createdAt = (project: Project): number => Date.parse(project.metadata?.creationTimestamp || "") || 0;
  return [...unique.values()].sort((a, b) =>
    createdAt(b) - createdAt(a) || (a.metadata?.name || "").localeCompare(b.metadata?.name || ""),
  );
}
