// Keep this byte-level encoding in sync with controller-manager/pkg/controllers/specname.
export function encodeSpecName(name: string): string {
  const bytes = new TextEncoder().encode(name);
  let encoded = 's';
  for (let i = 0; i < bytes.length; i++) {
    const byte = bytes[i];
    const alphaNumeric = byte >= 65 && byte <= 90 || byte >= 97 && byte <= 122 || byte >= 48 && byte <= 57;
    if (alphaNumeric || i + 1 < bytes.length && (byte === 45 || byte === 46)) {
      encoded += String.fromCharCode(byte);
    } else {
      encoded += `_${byte.toString(16).toUpperCase().padStart(2, '0')}`;
    }
  }
  return encoded;
}

export function decodeSpecName(value: string): string | null {
  if (!value.startsWith('s') || value.length < 2) return null;
  const bytes: number[] = [];
  for (let i = 1; i < value.length; i++) {
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
