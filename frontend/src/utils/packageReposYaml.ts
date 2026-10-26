import type { PackageRepo } from '@/types';
import { isFullGitRef } from './gitRef';

export function validatePackageRepos(value: unknown): PackageRepo[] {
  if (!Array.isArray(value)) throw new Error('project.packageYamlInvalid');
  const names = new Set<string>();
  return value.map(item => {
    if (!item || typeof item !== 'object' || Array.isArray(item)
      || typeof item.name !== 'string' || !item.name.trim()
      || typeof item.url !== 'string' || !item.url.trim()) throw new Error('project.packageYamlInvalid');
    const name = item.name.trim();
    if (names.has(name)) throw new Error('project.packageAlreadyAdded');
    names.add(name);
    if (item.ref !== undefined && (typeof item.ref !== 'string' || item.ref && !isFullGitRef(item.ref))) throw new Error('project.invalidDefaultRef');
    if (item.commitID !== undefined && (typeof item.commitID !== 'string' || item.commitID && !/^[0-9a-f]{40}$/.test(item.commitID))) throw new Error('project.invalidCommitID');
    return { ...item, name, url: item.url.trim() };
  });
}
