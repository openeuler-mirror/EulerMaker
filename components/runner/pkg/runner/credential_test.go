package runner

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMachineCredential(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	path := writeCredentialFile(t, `{"clientID":"runner-site-a","clientSecret":"`+secret+`"}`)
	credential, err := LoadMachineCredential(path)
	if err != nil {
		t.Fatalf("load credential: %v", err)
	}
	if credential.ClientID != "runner-site-a" || credential.ClientSecret != secret {
		t.Fatalf("unexpected credential: %#v", credential)
	}
}

func TestLoadMachineCredentialRejectsInvalidJSONAndValues(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	tests := []string{
		`{"clientID":"runner-a","clientID":"runner-b","clientSecret":"` + secret + `"}`,
		`{"clientID":"runner-a","clientSecret":"` + secret + `","extra":true}`,
		`{"clientID":"Runner-A","clientSecret":"` + secret + `"}`,
		`{"clientID":"runner-a","clientSecret":"short"}`,
		`{"clientID":"runner-a","clientSecret":"` + secret + `"} {}`,
	}
	for _, body := range tests {
		if _, err := LoadMachineCredential(writeCredentialFile(t, body)); err == nil {
			t.Fatalf("expected rejection for %s", body)
		}
	}
}

func writeCredentialFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	return path
}
