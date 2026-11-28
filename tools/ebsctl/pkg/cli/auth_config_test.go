package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ebsctl/pkg/config"
)

const personalToken = "ebs_pat_v1.YWxpY2U.0123456789abcdef0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
const userIdentity = `{"authenticated":true,"identity":{"type":"user","name":"verified-user","scopes":["ebs:user"]},"expiresAt":"2099-01-01T00:00:00Z"}`

func trustedGateway(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return server, ca
}

func TestTokenLoginThenCreateGetAndLogout(t *testing.T) {
	t.Setenv("EBS_GATEWAY", "")
	for _, token := range []string{personalToken, "user.jwt.signature"} {
		t.Run(strings.Split(token, ".")[0], func(t *testing.T) {
			t.Setenv("EBS_TOKEN", "environment-token-is-not-login-input")
			var created map[string]any
			checks := 0
			server, ca := trustedGateway(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("expected stdin credential")
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /auth/check":
					checks++
					body, _ := io.ReadAll(r.Body)
					if len(body) != 0 {
						t.Error("credential check must have no body")
					}
					io.WriteString(w, userIdentity)
				case "POST /apis/ebs/v1/projects/demo/builds":
					json.NewDecoder(r.Body).Decode(&created)
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(created)
				case "GET /apis/ebs/v1/projects/demo/builds/build-a":
					json.NewEncoder(w).Encode(created)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			})
			path := filepath.Join(t.TempDir(), "config.yaml")
			before := config.New()
			before.Contexts["production"] = config.Context{Gateway: "https://old.example", User: "old", Project: "demo"}
			before.Credentials["production"] = config.Credential{Token: "old-token"}
			if err := config.Save(path, before); err != nil {
				t.Fatal(err)
			}
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			code := Execute(context.Background(), Streams{In: strings.NewReader(token + "\r\n"), Out: out, ErrOut: errOut}, []string{
				"--config", path, "--verbose", "login", server.URL, "--token-stdin", "--context-name", "production", "--certificate-authority", ca,
			})
			if code != 0 || checks != 1 {
				t.Fatalf("code=%d checks=%d stderr=%s", code, checks, errOut)
			}
			if strings.Contains(out.String()+errOut.String(), token) || !strings.Contains(out.String(), "verified-user") {
				t.Fatal("unsafe or unverified login output")
			}
			saved, err := config.Load(path)
			if err != nil || saved.Credentials["production"].Token != token || saved.Contexts["production"].User != "verified-user" || saved.Contexts["production"].Project != "demo" {
				t.Fatal("login did not preserve the context and save verified identity")
			}
			for target, mode := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
				info, err := os.Stat(target)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatal("unsafe config permissions")
				}
			}
			t.Setenv("EBS_TOKEN", "")
			manifest := "apiVersion: ebs/v1\nkind: Build\nmetadata:\n  name: build-a\n"
			for _, args := range [][]string{{"create", "-f", "-"}, {"get", "build", "build-a"}, {"logout"}} {
				out.Reset()
				errOut.Reset()
				code = Execute(context.Background(), Streams{In: strings.NewReader(manifest), Out: out, ErrOut: errOut}, append([]string{"--config", path}, args...))
				if code != 0 {
					t.Fatalf("%v code=%d stderr=%s", args, code, errOut)
				}
			}
			saved, err = config.Load(path)
			if err != nil || len(saved.Credentials) != 0 || saved.CurrentContext != "production" || checks != 1 {
				t.Fatal("logout must only remove the local credential")
			}
		})
	}
}

func TestLoginRejectsInputAndRemovedFlags(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		flags       []string
	}{
		{"missing flag", personalToken, nil},
		{"disabled flag", personalToken, []string{"--token-stdin=false"}},
		{"username", personalToken, []string{"--username", "alice"}},
		{"password stdin", "password", []string{"--password-stdin"}},
		{"literal token", "", []string{"--token", "private-value"}},
		{"empty", "\n", []string{"--token-stdin"}},
		{"multiple", personalToken + "\nother", []string{"--token-stdin"}},
		{"extra newline", personalToken + "\n\n", []string{"--token-stdin"}},
		{"whitespace", " " + personalToken, []string{"--token-stdin"}},
		{"header injection", personalToken + "\rX: value", []string{"--token-stdin"}},
		{"oversize", strings.Repeat("x", 4097), []string{"--token-stdin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			path := filepath.Join(t.TempDir(), "config.yaml")
			code := Execute(context.Background(), Streams{In: strings.NewReader(tc.input), Out: out, ErrOut: errOut}, append([]string{"--config", path, "login", "https://unused.invalid"}, tc.flags...))
			if code == 0 || out.Len() != 0 || strings.Contains(errOut.String(), personalToken) || strings.Contains(errOut.String(), "private-value") {
				t.Fatalf("unexpected result code=%d stderr=%s", code, errOut)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid input wrote configuration")
			}
		})
	}
}

func TestLoginStdinSizeBoundary(t *testing.T) {
	for _, size := range []int{4096, 4097} {
		calls := 0
		server, ca := trustedGateway(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if len(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) != 4096 {
				t.Error("stdin was truncated")
			}
			io.WriteString(w, userIdentity)
		})
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		code := Execute(context.Background(), Streams{In: strings.NewReader(strings.Repeat("x", size)), Out: io.Discard, ErrOut: io.Discard}, []string{
			"--config", filepath.Join(directory, "config.yaml"), "login", server.URL, "--token-stdin", "--certificate-authority", ca,
		})
		if size == 4096 && (code != 0 || calls != 1) || size == 4097 && (code == 0 || calls != 0) {
			t.Fatalf("size=%d code=%d calls=%d", size, code, calls)
		}
	}
}

func TestLoginFailurePreservesConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"revoked", 401, personalToken},
		{"forbidden", 403, personalToken},
		{"unavailable", 503, personalToken},
		{"redirect", 302, personalToken},
		{"invalid JSON", 200, personalToken},
		{"unauthenticated", 200, strings.Replace(userIdentity, "true", "false", 1)},
		{"runner", 200, strings.Replace(userIdentity, `"type":"user"`, `"type":"runner"`, 1)},
		{"service", 200, strings.Replace(userIdentity, `"type":"user"`, `"type":"service"`, 1)},
		{"invalid name", 200, strings.Replace(userIdentity, "verified-user", "bad/name", 1)},
		{"unknown role", 200, strings.Replace(userIdentity, "ebs:user", "ebs:unknown", 1)},
		{"multiple roles", 200, strings.Replace(userIdentity, `"ebs:user"`, `"ebs:user","ebs:admin"`, 1)},
		{"missing role", 200, strings.Replace(userIdentity, `"ebs:user"`, "", 1)},
		{"expired", 200, strings.Replace(userIdentity, "2099", "2000", 1)},
		{"missing expiry", 200, `{"authenticated":true,"identity":{"type":"user","name":"alice","scopes":["ebs:user"]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server, ca := trustedGateway(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/auth/check" || r.Header.Get("Authorization") != "Bearer "+personalToken {
					t.Error("unexpected request")
				}
				w.Header().Set("Location", "/unexpected")
				w.Header().Set("X-Request-ID", personalToken)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			path := filepath.Join(t.TempDir(), "config.yaml")
			configuration := config.New()
			configuration.CurrentContext = "old"
			configuration.Credentials["old"] = config.Credential{Token: "keep"}
			if err := config.Save(path, configuration); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			code := Execute(context.Background(), Streams{In: strings.NewReader(personalToken), Out: out, ErrOut: errOut}, []string{"--config", path, "--verbose", "login", server.URL, "--token-stdin", "--certificate-authority", ca})
			after, _ := os.ReadFile(path)
			if code == 0 || calls != 1 || !bytes.Equal(before, after) || out.Len() != 0 || strings.Contains(errOut.String(), personalToken) {
				t.Fatalf("code=%d calls=%d stderr=%s", code, calls, errOut)
			}
		})
	}
}

func TestCredentialTransportAndEnvironment(t *testing.T) {
	t.Setenv("EBS_GATEWAY", "")
	server, ca := trustedGateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+personalToken {
			t.Error("environment credential not used")
		}
		io.WriteString(w, `{"kind":"ProjectList","items":[]}`)
	})
	for _, tc := range []struct {
		name, gateway string
		flags         []string
		environment   bool
		wantSuccess   bool
	}{
		{"http login", "http://127.0.0.1:1", nil, false, false},
		{"insecure login", server.URL, []string{"--insecure-skip-tls-verify"}, false, false},
		{"untrusted CA", server.URL, nil, false, false},
		{"http environment", "http://127.0.0.1:1", nil, true, false},
		{"insecure environment", server.URL, []string{"--insecure-skip-tls-verify"}, true, false},
		{"trusted environment", server.URL, nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			configuration := config.New()
			configuration.CurrentContext = "dev"
			configuration.Contexts["dev"] = config.Context{Gateway: tc.gateway, TLS: config.TLSConfig{CAFile: ca}}
			configuration.Credentials["dev"] = config.Credential{Token: "old-token"}
			if err := config.Save(path, configuration); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			args := append([]string{"--config", path}, tc.flags...)
			if tc.environment {
				t.Setenv("EBS_TOKEN", personalToken)
				args = append(args, "get", "projects")
			} else {
				args = append(args, "login", tc.gateway, "--token-stdin")
			}
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			code := Execute(context.Background(), Streams{In: strings.NewReader(personalToken), Out: out, ErrOut: errOut}, args)
			after, _ := os.ReadFile(path)
			if (code == 0) != tc.wantSuccess || !bytes.Equal(before, after) || strings.Contains(out.String()+errOut.String(), personalToken) {
				t.Fatalf("code=%d stderr=%s", code, errOut)
			}
		})
	}
}
