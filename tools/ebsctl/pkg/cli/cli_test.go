package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginThenCreateAndGet(t *testing.T) {
	var created map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/auth/login":
			var login map[string]string
			_ = json.NewDecoder(r.Body).Decode(&login)
			if login["username"] != "alice" || login["password"] != "password-value" {
				t.Fatalf("unexpected login: %#v", login)
			}
			_, _ = io.WriteString(w, `{"token":"token-a","tokenType":"Bearer","expiresIn":3600}`)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/ebs/v1/projects/project-a/jobs":
			if r.Header.Get("Authorization") != "Bearer token-a" {
				t.Fatalf("missing token")
			}
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/ebs/v1/projects/project-a/jobs/job-a":
			_ = json.NewEncoder(w).Encode(created)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.yaml")
	loginOut, loginErr := &bytes.Buffer{}, &bytes.Buffer{}
	code := Execute(context.Background(), Streams{In: strings.NewReader("password-value\n"), Out: loginOut, ErrOut: loginErr}, []string{"--config", configPath, "--insecure-skip-tls-verify", "login", server.URL, "--username", "alice", "--password-stdin"})
	if code != 0 {
		t.Fatalf("login code=%d stderr=%s", code, loginErr.String())
	}
	manifest := filepath.Join(directory, "job.yaml")
	data := []byte("apiVersion: ebs/v1\nkind: Job\nmetadata:\n  name: job-a\nspec:\n  priority: 10\n")
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	createOut, createErr := &bytes.Buffer{}, &bytes.Buffer{}
	code = Execute(context.Background(), Streams{In: strings.NewReader(""), Out: createOut, ErrOut: createErr}, []string{"--config", configPath, "-p", "project-a", "create", "-f", manifest, "-o", "name"})
	if code != 0 || strings.TrimSpace(createOut.String()) != "job/job-a" {
		t.Fatalf("create code=%d stdout=%q stderr=%q", code, createOut.String(), createErr.String())
	}
	metadata := created["metadata"].(map[string]any)
	if metadata["namespace"] != "project-a" {
		t.Fatalf("namespace not injected: %#v", created)
	}
	getOut, getErr := &bytes.Buffer{}, &bytes.Buffer{}
	code = Execute(context.Background(), Streams{In: strings.NewReader(""), Out: getOut, ErrOut: getErr}, []string{"--config", configPath, "-p", "project-a", "get", "job", "job-a", "-o", "json"})
	if code != 0 || !strings.Contains(getOut.String(), `"job-a"`) {
		t.Fatalf("get code=%d stdout=%q stderr=%q", code, getOut.String(), getErr.String())
	}
}

func TestGetProjectCanUseAnonymousGatewayOverride(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("anonymous request had Authorization header")
		}
		_, _ = io.WriteString(w, `{"kind":"ProjectList","items":[{"metadata":{"name":"project-a"}}]}`)
	}))
	defer server.Close()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	code := Execute(context.Background(), Streams{In: strings.NewReader(""), Out: out, ErrOut: errOut}, []string{"--config", filepath.Join(directory, "missing.yaml"), "--gateway", server.URL, "get", "projects", "-o", "name"})
	if code != 0 || out.String() != "project/project-a\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestGetProjectsMineFiltersTrustedUserProjects(t *testing.T) {
	t.Setenv("EBS_TOKEN", "token-a")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/auth/check":
			if r.Header.Get("Authorization") != "Bearer token-a" {
				t.Fatalf("missing token")
			}
			_, _ = io.WriteString(w, `{"authenticated":true,"identity":{"type":"user","name":"alice"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/ebs/v1/projects":
			_, _ = io.WriteString(w, `{"kind":"ProjectList","metadata":{"resourceVersion":"9"},"items":[`+
				`{"metadata":{"name":"owned","labels":{"ebs.io/owner-user":"alice"}}},`+
				`{"metadata":{"name":"member","labels":{"ebs.io/owner-user":"bob","ebs.io/member-user.alice":"true"}}},`+
				`{"metadata":{"name":"other","labels":{"ebs.io/owner-user":"carol"}}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := Execute(context.Background(), Streams{In: strings.NewReader(""), Out: out, ErrOut: errOut}, []string{
		"--config", filepath.Join(directory, "missing.yaml"), "--gateway", server.URL,
		"get", "projects", "--mine", "-o", "name",
	})
	if code != 0 || out.String() != "project/owned\nproject/member\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestGetProjectsMineRejectsUnsupportedUsage(t *testing.T) {
	streams := Streams{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}
	for _, args := range [][]string{
		{"get", "jobs", "--mine"},
		{"get", "project", "project-a", "--mine"},
		{"get", "projects", "--mine", "--watch"},
	} {
		if code := Execute(context.Background(), streams, args); code != 2 {
			t.Fatalf("args=%v exit code=%d", args, code)
		}
	}
}

func TestUsageAndAuthenticationExitCodes(t *testing.T) {
	streams := Streams{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}
	if code := Execute(context.Background(), streams, []string{"get", "runner"}); code != 2 {
		t.Fatalf("unknown resource exit code=%d", code)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if code := Execute(context.Background(), streams, []string{"--config", filepath.Join(directory, "missing"), "--gateway", server.URL, "get", "projects"}); code != 3 {
		t.Fatalf("authentication exit code=%d", code)
	}
}
