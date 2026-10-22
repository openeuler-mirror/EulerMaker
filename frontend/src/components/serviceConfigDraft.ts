export type ArtifactInstance = {
  internalURL: string;
  runnerInternalURL: string;
  runnerExternalURL: string;
};

export type ServiceConfigContent = {
  artifactManagers: Record<string, ArtifactInstance>;
  gitServer: { internalURL: string };
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

export function parseServiceContent(content: unknown): ServiceConfigContent {
  if (!isRecord(content) || Object.keys(content).some(key => !['artifactManagers', 'gitServer'].includes(key)) ||
    !isRecord(content.artifactManagers) || !Object.keys(content.artifactManagers).length ||
    !isRecord(content.gitServer) || typeof content.gitServer.internalURL !== 'string' ||
    Object.keys(content.gitServer).some(key => key !== 'internalURL')) throw new Error('serviceConfig.invalidContent');
  const entries = Object.entries(content.artifactManagers).map(([name, value]) => {
    if (!isRecord(value) || Object.keys(value).some(key => !['internalURL', 'runnerInternalURL', 'runnerExternalURL'].includes(key)) ||
      typeof value.internalURL !== 'string' || (value.runnerInternalURL !== undefined && typeof value.runnerInternalURL !== 'string') ||
      (value.runnerExternalURL !== undefined && typeof value.runnerExternalURL !== 'string')) throw new Error('serviceConfig.invalidContent');
    return [name, { internalURL: value.internalURL,
      runnerInternalURL: value.runnerInternalURL || '', runnerExternalURL: value.runnerExternalURL || '' }] as const;
  });
  return { artifactManagers: Object.fromEntries(entries), gitServer: { internalURL: content.gitServer.internalURL } };
}

export function validServiceURL(value: string, optional = false): boolean {
  if (!value) return optional;
  if (/\s/.test(value) || value.includes('?') || value.includes('#') || !/^https?:\/\//.test(value)) return false;
  const authority = value.slice(value.indexOf('://') + 3).split('/')[0];
  if (!authority || authority.includes('@') || authority.endsWith(':') || value.includes('\\')) return false;
  try {
    const parsed = new URL(value);
    return Boolean(parsed.hostname) && !parsed.username && !parsed.password &&
      (!parsed.port || Number(parsed.port) >= 1);
  } catch { return false; }
}

export function buildServiceContent(
  instances: Array<ArtifactInstance & { name: string }>, gitServerURL: string,
): ServiceConfigContent {
  if (!instances.length) throw new Error('serviceConfig.invalidContent');
  if (!validServiceURL(gitServerURL)) throw new Error('serviceConfig.invalidURL');
  const names = new Set<string>();
  for (const instance of instances) {
    if (instance.name.length > 63 || !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(instance.name)) {
      throw new Error('serviceConfig.invalidName');
    }
    if (names.has(instance.name)) throw new Error('serviceConfig.duplicateName');
    names.add(instance.name);
    if (!validServiceURL(instance.internalURL) || !validServiceURL(instance.runnerInternalURL, true) ||
      !validServiceURL(instance.runnerExternalURL, true)) throw new Error('serviceConfig.invalidURL');
  }
  return {
    artifactManagers: Object.fromEntries(instances.map(({ name, internalURL, runnerInternalURL, runnerExternalURL }) =>
      [name, { internalURL, runnerInternalURL, runnerExternalURL }])),
    gitServer: { internalURL: gitServerURL },
  };
}
