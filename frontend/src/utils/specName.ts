// Keep this byte-level encoding in sync with controller-manager/pkg/controllers/specname.
export function encodeSpecName(name: string): string {
  const bytes = new TextEncoder().encode(name);
  const alphaNumeric = (byte: number) => byte >= 65 && byte <= 90 || byte >= 97 && byte <= 122 || byte >= 48 && byte <= 57;
  const escape = (byte: number) => byte.toString(16).toUpperCase().padStart(2, '0');
  const escapedFirst = bytes.length > 0 && (!alphaNumeric(bytes[0]) || bytes[0] === 88);
  let encoded = escapedFirst ? `X_${escape(bytes[0])}` : '';
  for (let i = escapedFirst ? 1 : 0; i < bytes.length; i++) {
    const byte = bytes[i];
    if (alphaNumeric(byte) || i + 1 < bytes.length && (byte === 45 || byte === 46)) {
      encoded += String.fromCharCode(byte);
    } else {
      encoded += `_${escape(byte)}`;
    }
  }
  return encoded;
}

export function decodeSpecName(value: string): string | null {
  if (!value || !/^[A-Za-z0-9]/.test(value) || value.startsWith('X') && !value.startsWith('X_')) return null;
  const bytes: number[] = [];
  let start = 0;
  if (value.startsWith('X_')) {
    const first = value.slice(2, 4);
    if (!/^[0-9A-F]{2}$/.test(first)) return null;
    bytes.push(Number.parseInt(first, 16));
    start = 4;
  }
  for (let i = start; i < value.length; i++) {
    if (value[i] !== '_') {
      bytes.push(value.charCodeAt(i));
      continue;
    }
    const escape = value.slice(i + 1, i + 3);
    if (!/^[0-9A-F]{2}$/.test(escape)) return null;
    bytes.push(Number.parseInt(escape, 16));
    i += 2;
  }
  try {
    const name = new TextDecoder('utf-8', { fatal: true }).decode(new Uint8Array(bytes));
    return name && encodeSpecName(name) === value ? name : null;
  } catch {
    return null;
  }
}
