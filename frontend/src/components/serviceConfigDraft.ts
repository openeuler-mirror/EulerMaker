export type ServiceEndpoints = {
  internalURL: string;
  runnerInternalURL: string;
  runnerExternalURL: string;
};

export type ServiceConfigContent = {
  artifactManagers: Record<string, ServiceEndpoints>;
  gitServer: ServiceEndpoints;
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

export function parseServiceContent(content: unknown): ServiceConfigContent {
  if (!isRecord(content) || Object.keys(content).some(key => !['artifactManagers', 'gitServer'].includes(key)) ||
    !isRecord(content.artifactManagers) || !Object.keys(content.artifactManagers).length) throw new Error('serviceConfig.invalidContent');
  const entries = Object.entries(content.artifactManagers).map(([name, value]) => [name, parseEndpoints(value)]);
  return { artifactManagers: Object.fromEntries(entries), gitServer: parseEndpoints(content.gitServer) };
}

function parseEndpoints(value: unknown): ServiceEndpoints {
  if (!isRecord(value) || Object.keys(value).some(key => !['internalURL', 'runnerInternalURL', 'runnerExternalURL'].includes(key)) ||
    typeof value.internalURL !== 'string' || (value.runnerInternalURL !== undefined && typeof value.runnerInternalURL !== 'string') ||
    (value.runnerExternalURL !== undefined && typeof value.runnerExternalURL !== 'string')) throw new Error('serviceConfig.invalidContent');
  return { internalURL: value.internalURL,
    runnerInternalURL: value.runnerInternalURL || '', runnerExternalURL: value.runnerExternalURL || '' };
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
  instances: Array<ServiceEndpoints & { name: string }>, gitServer: ServiceEndpoints,
): ServiceConfigContent {
  if (!instances.length) throw new Error('serviceConfig.invalidContent');
  validateEndpoints(gitServer);
  const names = new Set<string>();
  for (const instance of instances) {
    if (instance.name.length > 63 || !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(instance.name)) {
      throw new Error('serviceConfig.invalidName');
    }
    if (names.has(instance.name)) throw new Error('serviceConfig.duplicateName');
    names.add(instance.name);
    validateEndpoints(instance);
  }
  return {
    artifactManagers: Object.fromEntries(instances.map(({ name, internalURL, runnerInternalURL, runnerExternalURL }) =>
      [name, { internalURL, runnerInternalURL, runnerExternalURL }])),
    gitServer: { ...gitServer },
  };
}

function validateEndpoints(endpoints: ServiceEndpoints): void {
  if (!validServiceURL(endpoints.internalURL) || !validServiceURL(endpoints.runnerInternalURL, true) ||
    !validServiceURL(endpoints.runnerExternalURL, true)) throw new Error('serviceConfig.invalidURL');
}
