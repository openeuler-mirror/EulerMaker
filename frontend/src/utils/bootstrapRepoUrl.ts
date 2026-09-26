// Bootstrap repository URLs are HTTP(S) base paths. The controller appends
// the target architecture, so credentials, queries and fragments are invalid.
export function isValidBootstrapRepoUrl(value: string): boolean {
  if (!value || /\s/.test(value)) return false;
  try {
    const url = new URL(value);
    return (url.protocol === "http:" || url.protocol === "https:")
      && Boolean(url.hostname)
      && !url.username
      && !url.password
      && !url.search
      && !url.hash;
  } catch {
    return false;
  }
}
