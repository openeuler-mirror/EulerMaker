package es

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectionAndServiceInitialization(t *testing.T) {
	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "ApiKey test-key" {
			t.Error("missing API key")
		}
		if r.URL.Path == "/_alias/ebs-build-target-claims" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"ebs-build-target-claims-v1": map[string]any{
				"aliases": map[string]any{"ebs-build-target-claims": map[string]bool{"is_write_index": true}},
			}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	config := &Config{Addresses: []string{upstream.URL}, APIKey: "test-key"}
	if _, err := config.NewConnection(); err != nil || len(requests) != 0 {
		t.Fatalf("connection err=%v requests=%v", err, requests)
	}
	if _, err := config.NewClient(); err != nil {
		t.Fatal(err)
	}
	if len(requests) != len(coreResources)+2 || requests[0] != "GET /" {
		t.Fatalf("service initialization=%v", requests)
	}
}

func TestLoadAPIKeyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key")
	c := DefaultConfig()
	if err := os.WriteFile(file, []byte(" key-value \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadAPIKeyFile(file); err != nil || c.APIKey != "key-value" {
		t.Fatalf("key loading err=%v", err)
	}
	if err := os.WriteFile(file, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadAPIKeyFile(file); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty key err=%v", err)
	}
}
