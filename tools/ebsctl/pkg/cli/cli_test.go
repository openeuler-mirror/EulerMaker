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
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"ebsctl/pkg/config"
)

func TestPublicReadFallbackPreservesConfigAndOutput(t *testing.T) {
	t.Setenv("EBS_TOKEN", "")
	t.Setenv("EBS_GATEWAY", "")
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if r.Header.Get("Authorization") != "Bearer expired-token" {
						t.Error("expected saved token on first request")
					}
					http.Error(w, "invalid token", http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("expected anonymous retry")
				}
				_, _ = io.WriteString(w, `{"kind":"ProjectList","items":[{"metadata":{"name":"project-a"}}]}`)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "config.yaml")
			configuration := config.New()
			configuration.CurrentContext = "dev"
			configuration.Contexts["dev"] = config.Context{Gateway: server.URL}
			configuration.Credentials["dev"] = config.Credential{Token: "expired-token"}
			if err := config.Save(path, configuration); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			code := Execute(context.Background(), Streams{Out: out, ErrOut: errOut}, []string{
				"--config", path, "get", "projects", "-o", format,
			})
			if code != 0 || calls.Load() != 2 {
				t.Fatalf("code=%d calls=%d stderr=%q", code, calls.Load(), errOut.String())
			}
			var result map[string]any
			if err := yaml.Unmarshal(out.Bytes(), &result); err != nil || result["kind"] != "ProjectList" {
				t.Fatalf("invalid %s output %q: %v", format, out.String(), err)
			}
			if format == "json" && !json.Valid(out.Bytes()) {
				t.Fatalf("invalid JSON output: %q", out.String())
			}
			if !strings.Contains(errOut.String(), "retrying this public read anonymously") ||
				!strings.Contains(errOut.String(), "ebsctl login") || strings.Contains(errOut.String(), "expired-token") {
				t.Fatalf("unexpected diagnostic: %q", errOut.String())
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("configuration changed: %v", err)
			}
		})
	}
}

func TestAuthenticatedCommandsKeepAuthenticationFailures(t *testing.T) {
	t.Setenv("EBS_TOKEN", "expired-token")
	for _, test := range []struct {
		name string
		args []string
	}{
		{"config list", []string{"get", "configs"}},
		{"config get", []string{"get", "cfg", "build-target"}},
		{"config describe", []string{"describe", "cfg", "build-resource"}},
		{"watch initial list", []string{"get", "jobs", "--watch"}},
		{"watch stream", []string{"get", "jobs", "--watch-only"}},
		{"wait initial get", []string{"wait", "job", "job-a", "--for=delete"}},
		{"mine list", []string{"get", "projects", "--mine"}},
		{"delete", []string{"delete", "build", "build-a", "--yes"}},
		{"delete initial list", []string{"delete", "builds", "--all", "--yes"}},
		{"patch", []string{"patch", "job", "job-a", "--patch", `{}`}},
		{"create", []string{"create", "-f", "-"}},
		{"replace", []string{"replace", "-f", "-"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer expired-token" {
					t.Error("expected authenticated request")
				}
				if r.URL.Path == "/auth/check" {
					_, _ = io.WriteString(w, `{"authenticated":true,"identity":{"type":"user","name":"alice","scopes":["ebs:user"]},"expiresAt":"2099-01-01T00:00:00Z"}`)
					return
				}
				http.Error(w, "invalid token", http.StatusUnauthorized)
			}))
			defer server.Close()
			directory := t.TempDir()
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			args := append([]string{"--config", filepath.Join(directory, "missing.yaml"), "--gateway", server.URL, "-p", "project-a"}, test.args...)
			manifest := "apiVersion: ebs/v1\nkind: Build\nmetadata:\n  name: build-a\n  resourceVersion: '1'\n"
			code := Execute(context.Background(), Streams{In: strings.NewReader(manifest), Out: out, ErrOut: errOut}, args)
			wantCalls, wantCode := int32(1), 3
			if test.name == "mine list" {
				wantCalls = 2
			}
			if test.name == "create" || test.name == "replace" {
				wantCode = 1 // Manifest commands report aggregate failures.
			}
			if code != wantCode || calls.Load() != wantCalls || out.Len() != 0 ||
				!strings.Contains(errOut.String(), "ebsctl login") || strings.Contains(errOut.String(), "retrying") {
				t.Fatalf("code=%d calls=%d stdout=%q stderr=%q", code, calls.Load(), out.String(), errOut.String())
			}
		})
	}
}

func TestProjectFlagAliases(t *testing.T) {
	for _, flags := range [][]string{
		{"-p", "project-a"},
		{"--project=project-a"},
		{"-n", "project-a"},
		{"--namespace=project-a"},
		{"-p", "other", "-n", "project-a"},
		{"-n", "other", "-p", "project-a"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			command, app := NewCommand(Streams{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard})
			command.SetArgs(append([]string{"version"}, flags...))
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			if app.project != "project-a" {
				t.Fatalf("project = %q, want project-a", app.project)
			}
		})
	}
}

func TestCreateJobRejectedLocally(t *testing.T) {
	t.Setenv("EBS_TOKEN", "token-a")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unsupported creation reached Gateway: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	manifest := "apiVersion: ebs/v1\nkind: Job\nmetadata:\n  name: job-a\n"
	for _, validate := range []string{"true", "false"} {
		errOut := &bytes.Buffer{}
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		args := []string{"--config", filepath.Join(directory, "missing.yaml"), "--gateway", server.URL, "-p", "project-a", "create", "-f", "-", "--validate=" + validate}
		code := Execute(context.Background(), Streams{In: strings.NewReader(manifest), Out: io.Discard, ErrOut: errOut}, args)
		if code != 1 || !strings.Contains(errOut.String(), "Job does not support create") {
			t.Fatalf("validate=%s code=%d stderr=%s", validate, code, errOut)
		}
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
			_, _ = io.WriteString(w, `{"authenticated":true,"identity":{"type":"user","name":"alice","scopes":["ebs:user"]},"expiresAt":"2099-01-01T00:00:00Z"}`)
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

func TestRejectsUnsupportedResourceCommands(t *testing.T) {
	streams := Streams{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}
	for _, args := range [][]string{
		{"get", "configs", "--watch"},
		{"delete", "cfg", "build-target", "--yes"},
		{"delete", "cfg", "--all", "--yes"},
		{"delete", "job", "job-a", "--yes"},
		{"delete", "jobs", "--all", "--yes"},
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
