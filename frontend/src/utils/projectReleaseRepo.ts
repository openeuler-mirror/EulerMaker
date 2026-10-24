import type { Project } from '@/types';

export function releaseOperatingSystems(project: Project): string[] {
  return [...new Set((project.spec?.buildTargets || []).map(target => target.os).filter((os): os is string => Boolean(os)))].sort();
}

export function projectReleaseRepoUrl(project: Project, os: string): string {
  const name = project.metadata?.name;
  if (!name || !releaseOperatingSystems(project).includes(os)) return '';
  const instance = project.metadata?.annotations?.['ebs.io/artifacts'] || 'default';
  return `artifact://${instance}/repositories/${encodeURIComponent(name)}/${encodeURIComponent(os)}`;
}

export function filterReleaseProjects(projects: Project[], currentProject: string, query: string, os: string): Project[] {
  const search = query.trim().toLowerCase();
  return projects.filter(project => {
    const name = project.metadata?.name;
    return name && name !== currentProject
      && `${name} ${project.spec?.displayName || ''}`.toLowerCase().includes(search)
      && releaseOperatingSystems(project).some(target => !os || target === os);
  }).sort((a, b) => a.metadata!.name!.localeCompare(b.metadata!.name!));
}
