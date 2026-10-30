import type { Project } from '@/types';

export function projectReleaseRepoUrl(project: Project): string {
  const name = project.metadata?.name;
  if (!name) return '';
  const instance = project.metadata?.annotations?.['ebs.io/artifacts'] || 'default';
  return `artifact://${instance}/repositories/${encodeURIComponent(name)}`;
}

export function filterReleaseProjects(projects: Project[], currentProject: string, query: string): Project[] {
  const search = query.trim().toLowerCase();
  return projects.filter(project => {
    const name = project.metadata?.name;
    return name && name !== currentProject
      && `${name} ${project.spec?.displayName || ''}`.toLowerCase().includes(search);
  }).sort((a, b) => a.metadata!.name!.localeCompare(b.metadata!.name!));
}
