package mapping

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func executeCommand(args ...string) (string, error) {
	cmd := NewCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestMappingCommand(t *testing.T) {
	var mapping map[string]any
	err := json.Unmarshal([]byte(`{
  "dynamic":"strict","properties":{
    "apiVersion":{"type":"keyword"},"kind":{"type":"keyword"},"documentID":{"type":"keyword"},
    "metadata":{"properties":{
      "name":{"type":"keyword"},"namespace":{"type":"keyword"},"creationTimestamp":{"type":"date"},
      "labels":{"type":"nested","properties":{"key":{"type":"keyword"},"value":{"type":"keyword"}}}
    }},
    "data":{"type":"object","dynamic":false},"credential":{"type":"object","enabled":false}
  }
}`), &mapping)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "ApiKey test-key" {
			t.Error("API key was not loaded")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_alias/ebs-users":
			_, _ = w.Write([]byte(`{"ebs-users-v3":{"aliases":{"ebs-users":{"is_write_index":true}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/ebs-users-v3/_mapping":
			_ = json.NewEncoder(w).Encode(map[string]any{"ebs-users-v3": map[string]any{"mappings": mapping}})
		case r.Method == http.MethodPut && r.URL.Path == "/ebs-users-v3/_mapping":
			writes++
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Error(err)
			}
			for key, value := range patch["properties"].(map[string]any) {
				mapping["properties"].(map[string]any)[key] = value
			}
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		default:
			t.Errorf("unexpected service initialization/request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer upstream.Close()
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--es-servers=" + upstream.URL, "--es-api-key-file=" + keyFile, "--resources=user"}
	out, err := executeCommand(args...)
	if !errors.Is(err, errMappingChanges) || writes != 0 || !strings.Contains(out, "Add externalIdentity") {
		t.Fatalf("preview out=%s err=%v writes=%d", out, err, writes)
	}
	out, err = executeCommand(append(args, "--apply")...)
	if err != nil || writes != 1 || !strings.Contains(out, "Updated=1") {
		t.Fatalf("apply out=%s err=%v writes=%d", out, err, writes)
	}
	out, err = executeCommand(args...)
	if err != nil || writes != 1 || !strings.Contains(out, "Unchanged=1") {
		t.Fatalf("repeat out=%s err=%v writes=%d", out, err, writes)
	}
}

func TestMappingCommandSelection(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/_alias/") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		alias := strings.TrimPrefix(r.URL.Path, "/_alias/")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "alias [" + alias + "] missing", "status": 404})
	}))
	defer upstream.Close()
	args := []string{"--es-servers=" + upstream.URL}
	out, err := executeCommand(args...)
	if err != nil || !strings.Contains(out, "oauthidentityclaim") || !strings.Contains(out, "oldjob") || requests == 0 {
		t.Fatalf("default out=%s err=%v requests=%d", out, err, requests)
	}
	for _, selection := range []string{"", "unknown", "user,,oldjob"} {
		requests = 0
		if _, err := executeCommand(append(args, "--resources="+selection)...); err == nil || requests != 0 {
			t.Fatalf("selection=%q err=%v requests=%d", selection, err, requests)
		}
	}
	if _, err := executeCommand(append(args, "--resources=user")...); err == nil || errors.Is(err, errMappingChanges) {
		t.Fatalf("explicit missing err=%v", err)
	}
}

func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{nil, 0},
		{errMappingChanges, 2},
		{fmt.Errorf("preview: %w", errMappingChanges), 2},
		{errors.New("mapping failed"), 1},
	} {
		if got := ExitCode(tc.err); got != tc.code {
			t.Errorf("ExitCode(%v)=%d, want %d", tc.err, got, tc.code)
		}
	}
}
