package release

import (
	"encoding/hex"
	"strings"
)

func validHash(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil && v == strings.ToLower(v)
}

func validIdentifier(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return v != "." && v != ".."
}
