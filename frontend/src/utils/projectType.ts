export const PROJECT_TYPE_LABEL = "project.ebs.io/type";
export type ProjectType = "community" | "personal";

export function projectTypeSelector(type: ProjectType): string {
  // Not-equal also matches legacy projects without the label.
  return `${PROJECT_TYPE_LABEL}${type === "community" ? "=" : "!="}community`;
}
