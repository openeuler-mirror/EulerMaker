// Bootstrap repositories accept HTTP(S) base paths and artifact logical references. The controller appends
// the target architecture, so credentials, queries and fragments are invalid.
export function isValidBootstrapRepoUrl(value: string): boolean {
  if (!value || /\s/.test(value)) return false;
  try {
    const url = new URL(value);
    if (url.username || url.password || url.search || url.hash) return false;
    if (url.protocol === "artifact:") {
      const path = decodeURIComponent(value.match(/^artifact:\/\/[^/]*(\/.*)$/)?.[1] || '');
      return value.startsWith('artifact://')
        && !url.port
        && !url.host.includes(':')
        && path.startsWith('/repositories/')
        && !path.slice(1).replace(/\/$/, '').split('/').some(part => !part || part === '.' || part === '..');
    }
    return (url.protocol === "http:" || url.protocol === "https:") && Boolean(url.hostname);
  } catch {
    return false;
  }
}
