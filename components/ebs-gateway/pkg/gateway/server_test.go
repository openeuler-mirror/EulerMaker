package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testSecret = "dev-secret"

func TestHealthzDoesNotRequireAuth(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMissingTokenReturnsUnauthorizedAndDoesNotProxy(t *testing.T) {
	var upstreamHits atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}), 100, 200)

	req := httptest.NewRequest(http.MethodGet, apiPrefix+"/projects", nil)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("expected no upstream request, got %d", upstreamHits.Load())
	}
}

func TestProxyInjectsTrustedIdentityHeaders(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-EBS-Tenant") != "system" {
			t.Fatalf("expected trusted tenant header, got %q", r.Header.Get("X-EBS-Tenant"))
		}
		if r.Header.Get("X-EBS-User") != "scheduler" {
			t.Fatalf("expected trusted user header, got %q", r.Header.Get("X-EBS-User"))
		}
		if r.Header.Get("X-EBS-Scopes") != "ebs:system" {
			t.Fatalf("expected trusted scopes header, got %q", r.Header.Get("X-EBS-Scopes"))
		}
		w.WriteHeader(http.StatusAccepted)
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/jobs?watch=true", nil, systemClaims())
	req.Header.Set("X-EBS-Tenant", "spoofed")
	req.Header.Set("X-EBS-User", "mallory")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected upstream status, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectCreateInjectsOwnerTenantLabel(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		labels := labelsFromObject(obj)
		if labels[ownerTenantLabel] != "tenant-a" {
			t.Fatalf("expected injected owner label tenant-a, got labels %#v", labels)
		}
		w.WriteHeader(http.StatusCreated)
	}), 100, 200)

	body := `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-tenant":"tenant-b"}}}`
	req := authenticatedRequest(t, http.MethodPost, apiPrefix+"/projects", strings.NewReader(body), userClaims("alice", "tenant-a"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSystemProjectCreateAlsoInjectsOwnerTenantLabel(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		labels := labelsFromObject(obj)
		if labels[ownerTenantLabel] != "system" {
			t.Fatalf("expected injected owner label system, got labels %#v", labels)
		}
		w.WriteHeader(http.StatusCreated)
	}), 100, 200)

	body := `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-tenant":"tenant-b"}}}`
	req := authenticatedRequest(t, http.MethodPost, apiPrefix+"/projects", strings.NewReader(body), systemClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectListFiltersByOwnerAndMemberTenant(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPrefix+"/projects" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"kind":"ProjectList",
			"metadata":{"resourceVersion":"1"},
			"items":[
				{"metadata":{"name":"owned","labels":{"ebs.io/owner-tenant":"tenant-a"}}},
				{"metadata":{"name":"member","labels":{"ebs.io/owner-tenant":"tenant-b","ebs.io/member-tenant.tenant-a":"true"}}},
				{"metadata":{"name":"denied","labels":{"ebs.io/owner-tenant":"tenant-c"}}}
			]
		}`)
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/projects", nil, userClaims("alice", "tenant-a"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	items := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 visible projects, got %d: %s", len(items), rec.Body.String())
	}
}

func TestUserCannotAccessGlobalProjectScopedResource(t *testing.T) {
	var upstreamHits atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/jobs?watch=true", nil, userClaims("alice", "tenant-a"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("expected no upstream request, got %d", upstreamHits.Load())
	}
}

func TestProjectSubresourceRequiresProjectAccess(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/projects/project-a":
			_, _ = io.WriteString(w, `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-tenant":"tenant-b"}}}`)
		default:
			t.Fatalf("unexpected upstream proxy request %s", r.URL.Path)
		}
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/projects/project-a/jobs", nil, userClaims("alice", "tenant-a"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectMemberCannotModifyAccessLabels(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/projects/project-a":
			_, _ = io.WriteString(w, `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-tenant":"tenant-b","ebs.io/member-tenant.tenant-a":"true"}}}`)
		default:
			t.Fatalf("unexpected upstream proxy request %s", r.URL.Path)
		}
	}), 100, 200)

	body := `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-tenant":"tenant-b","ebs.io/member-tenant.tenant-c":"true"}}}`
	req := authenticatedRequest(t, http.MethodPut, apiPrefix+"/projects/project-a", strings.NewReader(body), userClaims("alice", "tenant-a"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectOwnerCanModifyMemberLabelsButNotOwnerLabel(t *testing.T) {
	var proxied atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/projects/project-a":
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-tenant":"tenant-a"}}}`)
				return
			}
			proxied.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}), 100, 200)

	body := `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-tenant":"tenant-a","ebs.io/member-tenant.tenant-b":"true"}}}`
	req := authenticatedRequest(t, http.MethodPut, apiPrefix+"/projects/project-a", strings.NewReader(body), userClaims("alice", "tenant-a"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if proxied.Load() != 1 {
		t.Fatalf("expected project update proxied once, got %d", proxied.Load())
	}

	body = `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-tenant":"tenant-b"}}}`
	req = authenticatedRequest(t, http.MethodPut, apiPrefix+"/projects/project-a", strings.NewReader(body), userClaims("alice", "tenant-a"))
	rec = httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRateLimitReturnsTooManyRequests(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 1, 1)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/jobs", nil, systemClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected first request 200, got %d", rec.Code)
	}

	req = authenticatedRequest(t, http.MethodGet, apiPrefix+"/jobs", nil, systemClaims())
	rec = httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second request 429, got %d: %s", rec.Code, rec.Body.String())
	}
}

func newTestGateway(t *testing.T, upstream http.Handler, rate float64, burst int) *Gateway {
	t.Helper()
	gw, err := NewGateway(Config{
		Port:            8080,
		APIServerAddr:   "http://ebs-apiserver",
		JWTSecret:       testSecret,
		RateLimitPerSec: rate,
		RateLimitBurst:  burst,
	})
	if err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		upstream.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	gw.client.Transport = transport
	gw.proxy.Transport = transport
	return gw
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func authenticatedRequest(t *testing.T, method, target string, body io.Reader, claims jwtClaims) *http.Request {
	t.Helper()
	token, err := signTestJWT(claims, testSecret)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func signTestJWT(claims jwtClaims, secret string) (string, error) {
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(claimsBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func userClaims(sub, tenant string) jwtClaims {
	return jwtClaims{
		Subject: sub,
		Tenant:  tenant,
		Scopes:  []string{"ebs:user"},
		Exp:     time.Now().Add(time.Hour).Unix(),
	}
}

func systemClaims() jwtClaims {
	return jwtClaims{
		Subject: "scheduler",
		Tenant:  "system",
		Scopes:  []string{"ebs:system"},
		Exp:     time.Now().Add(time.Hour).Unix(),
	}
}
