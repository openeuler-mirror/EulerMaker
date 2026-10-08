package specname

import "strings"

const hexDigits = "0123456789ABCDEF"

// Encode converts a SPEC name to a reversible Kubernetes label value.
// The leading s protects the first byte; each escaped UTF-8 byte is _HH.
func Encode(name string) string {
	var out strings.Builder
	out.Grow(len(name) + 1)
	out.WriteByte('s')
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if isAlphaNum(ch) || (i+1 < len(name) && (ch == '-' || ch == '.')) {
			out.WriteByte(ch)
			continue
		}
		out.WriteByte('_')
		out.WriteByte(hexDigits[ch>>4])
		out.WriteByte(hexDigits[ch&0xf])
	}
	return out.String()
}

// Decode reverses Encode. Malformed values are rejected rather than treated
// as SPEC names, so a damaged label cannot be matched to a build status.
func Decode(value string) (string, bool) {
	if len(value) < 2 || value[0] != 's' {
		return "", false
	}
	var out strings.Builder
	for i := 1; i < len(value); i++ {
		if value[i] != '_' {
			out.WriteByte(value[i])
			continue
		}
		if i+2 >= len(value) {
			return "", false
		}
		hi, lo := fromHex(value[i+1]), fromHex(value[i+2])
		if hi < 0 || lo < 0 {
			return "", false
		}
		out.WriteByte(byte(hi<<4 | lo))
		i += 2
	}
	name := out.String()
	return name, name != "" && Encode(name) == value
}

func isAlphaNum(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
}

func fromHex(ch byte) int {
	switch {
	case ch >= '0' && ch <= '9':
		return int(ch - '0')
	case ch >= 'A' && ch <= 'F':
		return int(ch-'A') + 10
	default:
		return -1
	}
}
