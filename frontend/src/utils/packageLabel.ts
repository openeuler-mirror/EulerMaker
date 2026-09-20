export const PACKAGE_NAME_LABEL = "ebs.io/package-name";

export async function packageNameLabelValue(packageName: string): Promise<string> {
  const validLabelCharacters = /^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$/;
  if (validLabelCharacters.test(packageName)) {
    const truncated = packageName.slice(0, 63).replace(/[-_.]+$/, "");
    if (!/^sha256-[a-z2-7]{52}$/.test(truncated)) return truncated;
  }
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(packageName)));
  const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
  let encoded = "";
  let bits = 0;
  let buffer = 0;
  for (const byte of digest) {
    buffer = (buffer << 8) | byte;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      encoded += alphabet[(buffer >> bits) & 31];
    }
    buffer &= (1 << bits) - 1;
  }
  if (bits) encoded += alphabet[(buffer << (5 - bits)) & 31];
  return `sha256-${encoded}`;
}
