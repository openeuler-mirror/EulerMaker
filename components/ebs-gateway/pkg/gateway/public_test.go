package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAnonymousPublicReadsProxyCompleteObjects(t *testing.T) {
	paths := []string{
		apiPrefix + "/projects",
		apiPrefix + "/projects?watch=false",
		apiPrefix + "/projects/project-a",
		apiPrefix + "/projects/project-a/status",
		apiPrefix + "/projects/project-a/snapshots",
		apiPrefix + "/projects/project-a/snapshots/snapshot-a",
		apiPrefix + "/projects/project-a/snapshots/snapshot-a/status",
		apiPrefix + "/projects/project-a/builds/build-a",
		apiPrefix + "/projects/project-a/buildinfos/build-a",
		apiPrefix + "/projects/project-a/rpmrepos/repo-a",
		apiPrefix + "/projects/project-a/jobs/job-a/status",
	}
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-EBS-User") != "" || r.Header.Get("X-EBS-Scopes") != "" || r.Header.Get("X-EBS-Spoofed") != "" {
			t.Fatalf("anonymous request contained identity headers: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", "internal-etag")
		w.Header().Set("X-Resource-Version", "7")
		w.Header().Set("X-EBS-Upstream-User", "internal")
		_, _ = io.WriteString(w, `{"metadata":{"uid":"uid-a","resourceVersion":"7"},"spec":{"payload":{"secret":"visible"}},"status":{"runner":"runner-a","message":"visible"}}`)
	}), 100, 200)

	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-EBS-Spoofed", "true")
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d: %s", path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("ETag") != "" || rec.Header().Get("X-Resource-Version") != "" || rec.Header().Get("X-EBS-Upstream-User") != "" {
			t.Fatalf("GET %s: internal upstream response headers leaked: %#v", path, rec.Header())
		}
		for _, field := range []string{"uid-a", "resourceVersion", "secret", "runner-a", "visible"} {
			if !strings.Contains(rec.Body.String(), field) {
				t.Fatalf("GET %s: complete object field %q was removed: %s", path, field, rec.Body.String())
			}
		}
	}
}

func TestAnonymousHEADReturnsNoBody(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Fatalf("expected HEAD upstream, got %s", r.Method)
		}
		w.Header().Set("X-Result", "present")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"must-not-be-written"}`)
	}), 100, 200)
	req := httptest.NewRequest(http.MethodHead, apiPrefix+"/projects/project-a/jobs/job-a/status", nil)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 || rec.Header().Get("X-Result") != "present" {
		t.Fatalf("unexpected HEAD response: status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestAnonymousPublicCollectionEnforcesLimit(t *testing.T) {
	var requests atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("limit") != "100" {
			t.Fatalf("expected default limit 100, got %q", r.URL.Query().Get("limit"))
		}
		_, _ = io.WriteString(w, `{}`)
	}), 100, 200)

	req := httptest.NewRequest(http.MethodGet, apiPrefix+"/projects/project-a/jobs", nil)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, query := range []string{"limit=101", "limit=0", "limit=abc", "limit=1&limit=2"} {
		req = httptest.NewRequest(http.MethodGet, apiPrefix+"/projects?"+query, nil)
		rec = httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: expected 400, got %d", query, rec.Code)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("invalid limits reached upstream; requests=%d", requests.Load())
	}
}

func TestAnonymousReadDeniesNonPublicRequests(t *testing.T) {
	var upstreamHits atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
	}), 100, 200)
	tests := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, apiPrefix + "/jobs", http.StatusNotFound},
		{http.MethodGet, apiPrefix + "/builds", http.StatusNotFound},
		{http.MethodGet, apiPrefix + "/runners", http.StatusUnauthorized},
		{http.MethodGet, apiPrefix + "/projects?watch=true", http.StatusUnauthorized},
		{http.MethodGet, apiPrefix + "/projects?watch=1", http.StatusUnauthorized},
		{http.MethodGet, apiPrefix + "/projects?watch=false&watch=true", http.StatusUnauthorized},
		{http.MethodGet, apiPrefix + "/projects?watch=true;limit=1", http.StatusUnauthorized},
		{http.MethodPost, apiPrefix + "/projects", http.StatusUnauthorized},
		{http.MethodGet, apiPrefix + "/projects/project-a/jobs/job-a/log", http.StatusUnauthorized},
		{http.MethodGet, "/apis/iam.ebs/v1/users", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s %s: expected %d, got %d: %s", tc.method, tc.path, tc.want, rec.Code, rec.Body.String())
		}
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("denied requests reached upstream %d times", upstreamHits.Load())
	}
}

func TestInvalidTokenDoesNotDowngradeToAnonymous(t *testing.T) {
	var upstreamHits atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
	}), 100, 200)
	req := httptest.NewRequest(http.MethodGet, apiPrefix+"/projects", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || upstreamHits.Load() != 0 {
		t.Fatalf("expected invalid token 401 without proxy, got status=%d hits=%d", rec.Code, upstreamHits.Load())
	}
}

func TestAnonymousReadUsesIndependentRateLimit(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}), 100, 200)
	gw.publicLimiter = NewRateLimiter(1, 1)

	for index, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodGet, apiPrefix+"/projects/project-a", nil)
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("anonymous request %d: expected %d, got %d", index+1, want, rec.Code)
		}
	}

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/runners", nil, systemClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous limit affected authenticated request: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInternalGlobalAPIsAreNotGatewayRoutes(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)
	for _, resource := range []string{"snapshots", "builds", "buildinfos", "rpmrepos", "jobs"} {
		req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/"+resource, nil, systemClaims())
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("global %s: expected 404, got %d: %s", resource, rec.Code, rec.Body.String())
		}
	}
}
