package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

func projectBytes(version string, repos []ebsv1.PackageRepo) []byte {
	data, _ := json.Marshal(map[string]any{
		"apiVersion": "ebs/v1", "kind": "Project",
		"metadata": map[string]any{"name": "demo", "uid": "uid-demo", "resourceVersion": version, "labels": map[string]string{"owner": "owner"}},
		"spec":     map[string]any{"defaultRef": "refs/heads/main", "distro": "openEuler", "packageRepos": repos},
		"status":   map[string]any{"retained": true},
	})
	return data
}

func TestGatewayClientAnonymousGetAndAuthenticatedPATCH(t *testing.T) {
	c := testConfig(t)
	tokens := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/machine-token" {
			assertTokenExchange(t, r)
			_, _ = w.Write([]byte(`{"accessToken":"service-token","tokenType":"Bearer","expiresIn":3600}`))
			return
		}
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.URL.Path != "/apis/ebs/v1/projects/demo" {
			t.Error(r.URL.Path)
		}
		if r.Method == http.MethodPatch && r.Header.Get("Content-Type") != "application/merge-patch+json" {
			t.Error("wrong patch type")
		}
		_, _ = w.Write(projectBytes("2", nil))
	}))
	defer server.Close()
	client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: server.URL, maxBytes: c.MaxProjectBytes, http: server.Client()}
	p, err := client.Get(context.Background(), c.Bindings[0])
	if err != nil {
		t.Fatal(err)
	}
	patch, err := preflight(p, []ebsv1.PackageRepo{{Name: "gcc", URL: "https://example.com/gcc"}}, c.MaxProjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(patch, &body)
	if len(body) != 2 || body["metadata"] == nil || body["spec"] == nil {
		t.Fatal(string(patch))
	}
	if _, err := client.Patch(context.Background(), c.Bindings[0], patch); err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 || tokens[0] != "" || tokens[1] != "Bearer service-token" {
		t.Fatal(tokens)
	}
	if string(p.Object["status"]) != `{"retained":true}` {
		t.Fatal("preflight changed original object")
	}
}

func TestGatewayClientTrustedHTTP(t *testing.T) {
	c := testConfig(t)
	var exchanges, patches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/machine-token" {
			assertTokenExchange(t, r)
			exchanges++
			_, _ = w.Write([]byte(`{"accessToken":"service-token","tokenType":"Bearer","expiresIn":3600}`))
			return
		}
		if r.Method == http.MethodPatch {
			patches++
			if r.Header.Get("Authorization") != "Bearer service-token" {
				t.Error("PATCH requires Service Token")
			}
		} else if r.Header.Get("Authorization") != "" {
			t.Error("GET must be anonymous")
		}
		_, _ = w.Write(projectBytes("2", nil))
	}))
	defer server.Close()
	c.GatewayURL = server.URL
	client, err := NewGatewayClient(c)
	if err != nil {
		t.Fatal(err)
	}
	p, err := client.Get(context.Background(), c.Bindings[0])
	if err != nil {
		t.Fatal(err)
	}
	body, err := preflight(p, []ebsv1.PackageRepo{{Name: "gcc", URL: "https://example.com/gcc"}}, c.MaxProjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range []Binding{c.Bindings[0], {Name: "other", Project: "other"}} {
		if _, err := client.Patch(context.Background(), binding, body); err != nil {
			t.Fatal(err)
		}
	}
	if exchanges != 1 || patches != 2 {
		t.Fatalf("exchanges=%d patches=%d", exchanges, patches)
	}
}

func TestGatewayFailureClassification(t *testing.T) {
	for _, test := range []struct {
		status  int
		body    string
		blocked bool
	}{
		{403, `{"error":"private response text"}`, true},
		{409, `{}`, false}, {429, `{}`, false}, {413, `{}`, true},
		{502, `{"error":"current object exceeds body limit"}`, true},
		{502, `{"error":"transient"}`, false}, {503, `{}`, false},
	} {
		t.Run(fmt.Sprint(test.status, test.body), func(t *testing.T) {
			c := testConfig(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: server.URL, maxBytes: c.MaxProjectBytes, http: server.Client()}
			_, err := client.Get(context.Background(), c.Bindings[0])
			failure := failureOf(err)
			if err == nil || failure.Blocked != test.blocked || failure.Status != test.status {
				t.Fatal(failure)
			}
			if test.status == 429 && failure.RetryAfter != 7*time.Second {
				t.Fatal(failure)
			}
			if strings.Contains(err.Error(), "private response text") {
				t.Fatal("upstream body leaked")
			}
		})
	}
}

func TestGatewayPATCHAuthenticationRecovery(t *testing.T) {
	for _, test := range []struct {
		name      string
		first     int
		second    int
		patches   int32
		exchanges int32
	}{
		{"refresh once", 401, 200, 2, 2},
		{"still unauthorized", 401, 401, 2, 2},
		{"forbidden", 403, 200, 1, 1},
		{"server failure", 503, 200, 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testConfig(t)
			var exchanges, patches atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth/machine-token" {
					assertTokenExchange(t, r)
					fmt.Fprintf(w, `{"accessToken":"token-%d","tokenType":"Bearer","expiresIn":3600}`, exchanges.Add(1))
					return
				}
				n := patches.Add(1)
				if r.Method != http.MethodPatch || r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", n) {
					t.Error("invalid authenticated PATCH")
				}
				data, _ := io.ReadAll(r.Body)
				if string(data) != `{"metadata":{"resourceVersion":"1"},"spec":{"packageRepos":[]}}` {
					t.Error("PATCH retry changed original preconditions")
				}
				status := test.first
				if n == 2 {
					status = test.second
				}
				w.WriteHeader(status)
				_, _ = w.Write(projectBytes("2", nil))
			}))
			defer server.Close()
			client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: server.URL, maxBytes: c.MaxProjectBytes, http: server.Client()}
			_, err := client.Patch(context.Background(), c.Bindings[0], []byte(`{"metadata":{"resourceVersion":"1"},"spec":{"packageRepos":[]}}`))
			success := test.patches == 2 && test.second == 200
			if (err == nil) != success || exchanges.Load() != test.exchanges || patches.Load() != test.patches {
				t.Fatalf("error=%v exchanges=%d patches=%d", err, exchanges.Load(), patches.Load())
			}
			if test.second == 401 && !failureOf(err).Blocked {
				t.Fatal("repeated unauthorized response must block the binding")
			}
		})
	}
}

func TestAnonymousGetDoesNotReadCredentials(t *testing.T) {
	c := testConfig(t)
	c.MachineCredentialFile = "/missing/credential.json"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
			t.Error("GET must be deliberately anonymous")
		}
		_, _ = w.Write(projectBytes("1", nil))
	}))
	defer server.Close()
	client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: server.URL, maxBytes: c.MaxProjectBytes, http: server.Client()}
	if _, err := client.Get(context.Background(), c.Bindings[0]); err != nil {
		t.Fatal(err)
	}
}

func TestProjectSizeLimitsAndRetryAfter(t *testing.T) {
	p, err := decodeProject(projectBytes("1", nil))
	if err != nil {
		t.Fatal(err)
	}
	p.Object["status"] = json.RawMessage(`{"large":"` + strings.Repeat("x", 2000) + `"}`)
	if _, err := preflight(p, nil, 1024); err == nil || !failureOf(err).Blocked {
		t.Fatal("complete object limit not enforced")
	}
	p, _ = decodeProject(projectBytes("1", nil))
	if _, err := preflight(p, []ebsv1.PackageRepo{{Name: "gcc", URL: "https://example.com/" + strings.Repeat("x", 900)}}, 1024); err == nil {
		t.Fatal("merged candidate limit not enforced")
	}
	now := time.Now().UTC().Truncate(time.Second)
	if retryAfter(now.Add(time.Minute).Format(http.TimeFormat), now) != time.Minute || retryAfter("invalid", now) != 0 {
		t.Fatal("Retry-After parsing")
	}
}

func TestGatewayDoesNotFollowRedirects(t *testing.T) {
	c := testConfig(t)
	client, err := NewGatewayClient(c)
	if err != nil {
		t.Fatal(err)
	}
	if client.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect enabled")
	}
}

func TestValidationDiagnosticOmitsMessagesAndValues(t *testing.T) {
	c := testConfig(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"private value","details":{"causes":[{"field":"spec.packageRepos[0].ref","message":"secret value"}]}}`))
	}))
	defer server.Close()
	client := &GatewayClient{credentialFile: c.MachineCredentialFile, base: server.URL, maxBytes: c.MaxProjectBytes, http: server.Client()}
	_, err := client.Get(context.Background(), c.Bindings[0])
	if err == nil || !strings.Contains(err.Error(), "spec.packageRepos[0].ref") || strings.Contains(err.Error(), "value") {
		t.Fatal(err)
	}
}
