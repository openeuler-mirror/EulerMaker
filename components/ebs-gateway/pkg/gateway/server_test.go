package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestHealthzDoesNotRequireAuth(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterCreatesUserWithoutIssuingToken(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/iam/v1/users/register" {
			t.Fatalf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode registration: %v", err)
		}
		if input["username"] != "alice" || input["password"] != "correct password" || input["email"] != "alice@example.com" {
			t.Fatalf("unexpected registration payload: %#v", input)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"username":"alice"}`)
	}), 100, 200)

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"alice","password":"correct password","displayName":"Alice","email":"alice@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["username"] != "alice" || response["token"] != nil {
		t.Fatalf("unexpected registration response: %#v", response)
	}
}

func TestRegisterValidatesInputAndMapsConflict(t *testing.T) {
	var upstreamHits atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		http.Error(w, "exists", http.StatusConflict)
	}), 100, 200)

	invalid := []string{
		`{"username":"Alice","password":"correct password"}`,
		`{"username":"alice","password":"short"}`,
		`{"username":"alice","password":"correct password","enabled":true}`,
		`{"username":"alice","password":"correct password","email":"invalid"}`,
	}
	for _, body := range invalid {
		req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected invalid request 400, got %d for %s", rec.Code, body)
		}
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("invalid requests reached upstream %d times", upstreamHits.Load())
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"alice","password":"correct password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected conflict 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterRateLimitUsesUsernameAndClientIP(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"username":"alice"}`)
	}), 100, 200)
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"alice","password":"correct password"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if i < 3 && rec.Code != http.StatusCreated {
			t.Fatalf("request %d: expected 201, got %d", i+1, rec.Code)
		}
		if i == 3 && (rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60") {
			t.Fatalf("expected fourth request 429 with Retry-After, got %d %#v", rec.Code, rec.Header())
		}
	}
}

func TestLoginAuthenticatesUserAndIssuesUsableUserToken(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/iam/v1/authenticate":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatalf("decode authenticate request: %v", err)
			}
			if input["username"] != "alice" || input["password"] != "correct password" {
				t.Fatalf("unexpected credentials payload: %#v", input)
			}
			_, _ = io.WriteString(w, `{"authenticated":true,"username":"alice"}`)
		case apiPrefix + "/projects":
			_, _ = io.WriteString(w, `{"items":[]}`)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}), 100, 200)
	fixedNow := time.Unix(1790000000, 0)
	gw.now = func() time.Time { return fixedNow }

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"alice","password":"correct password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Token     string `json:"token"`
		TokenType string `json:"tokenType"`
		ExpiresIn int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if response.TokenType != "Bearer" || response.ExpiresIn != 3600 {
		t.Fatalf("unexpected login response: %#v", response)
	}
	identity, err := gw.tokens.parse(response.Token, fixedNow)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	if identity.Subject != "alice" || !identity.IsUser() || identity.JTI == "" {
		t.Fatalf("unexpected issued identity: %#v", identity)
	}

	apiReq := httptest.NewRequest(http.MethodGet, apiPrefix+"/projects", nil)
	apiReq.Header.Set("Authorization", "Bearer "+response.Token)
	apiRec := httptest.NewRecorder()
	gw.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("expected issued token to access API, got %d: %s", apiRec.Code, apiRec.Body.String())
	}
}

func TestLoginRejectsInvalidCredentials(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/iam/v1/authenticate" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}), 100, 200)
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"alice","password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginRejectsDisabledUser(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/iam/v1/authenticate":
			_, _ = io.WriteString(w, `{"authenticated":true,"username":"disabled"}`)
		case "/apis/iam.ebs/v1/users/disabled":
			_, _ = io.WriteString(w, `{"metadata":{"name":"disabled"},"spec":{"enabled":false}}`)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}), 100, 200)
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"disabled","password":"correct password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMissingTokenReturnsUnauthorizedAndDoesNotProxy(t *testing.T) {
	var upstreamHits atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}), 100, 200)

	req := httptest.NewRequest(http.MethodGet, apiPrefix+"/runners", nil)
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
		if r.Header.Get("X-EBS-User") != "scheduler" {
			t.Fatalf("expected trusted user header, got %q", r.Header.Get("X-EBS-User"))
		}
		if r.Header.Get("X-EBS-Scopes") != "ebs:system" {
			t.Fatalf("expected trusted scopes header, got %q", r.Header.Get("X-EBS-Scopes"))
		}
		if r.Header.Get("X-EBS-Admin") != "" {
			t.Fatalf("expected arbitrary client X-EBS header to be removed")
		}
		w.WriteHeader(http.StatusAccepted)
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/runners?watch=true", nil, systemClaims())
	req.Header.Set("X-EBS-Admin", "spoofed")
	req.Header.Set("X-EBS-User", "mallory")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected upstream status, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectCreateInjectsOwnerUserLabel(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		labels := labelsFromObject(obj)
		if labels[ownerUserLabel] != "alice" {
			t.Fatalf("expected injected owner user alice, got labels %#v", labels)
		}
		w.WriteHeader(http.StatusCreated)
	}), 100, 200)

	body := `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"bob"}}}`
	req := authenticatedRequest(t, http.MethodPost, apiPrefix+"/projects", strings.NewReader(body), userClaims("alice"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSystemProjectCreateRequiresEnabledOwnerUser(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		labels := labelsFromObject(obj)
		if labels[ownerUserLabel] != "alice" {
			t.Fatalf("expected system-provided owner user alice, got labels %#v", labels)
		}
		w.WriteHeader(http.StatusCreated)
	}), 100, 200)

	body := `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"alice"}}}`
	req := authenticatedRequest(t, http.MethodPost, apiPrefix+"/projects", strings.NewReader(body), systemClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSystemProjectCreateRejectsMissingOwnerUser(t *testing.T) {
	var upstreamHits atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodPost, apiPrefix+"/projects", strings.NewReader(`{"metadata":{"name":"project-a"}}`), systemClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("expected no upstream request, got %d", upstreamHits.Load())
	}
}

func TestProjectListFiltersByOwnerAndMemberUser(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPrefix+"/projects" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"kind":"ProjectList",
			"metadata":{"resourceVersion":"1"},
			"items":[
				{"metadata":{"name":"owned","labels":{"ebs.io/owner-user":"alice"}}},
				{"metadata":{"name":"member","labels":{"ebs.io/owner-user":"bob","ebs.io/member-user.alice":"true"}}},
				{"metadata":{"name":"denied","labels":{"ebs.io/owner-user":"carol"}}}
			]
		}`)
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/projects", nil, userClaims("alice"))
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

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/jobs?watch=true", nil, userClaims("alice"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("expected no upstream request, got %d", upstreamHits.Load())
	}
}

func TestProjectSubresourceRequiresProjectAccess(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/projects/project-a":
			_, _ = io.WriteString(w, `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"bob"}}}`)
		default:
			t.Fatalf("unexpected upstream proxy request %s", r.URL.Path)
		}
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/projects/project-a/jobs", nil, userClaims("alice"))
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
			_, _ = io.WriteString(w, `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"bob","ebs.io/member-user.alice":"true"}}}`)
		default:
			t.Fatalf("unexpected upstream proxy request %s", r.URL.Path)
		}
	}), 100, 200)

	body := `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"bob","ebs.io/member-user.carol":"true"}}}`
	req := authenticatedRequest(t, http.MethodPut, apiPrefix+"/projects/project-a", strings.NewReader(body), userClaims("alice"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectMemberCannotDeleteSubresource(t *testing.T) {
	var deletes atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/projects/project-a":
			_, _ = io.WriteString(w, `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"bob","ebs.io/member-user.alice":"true"}}}`)
		case apiPrefix + "/projects/project-a/builds/build-a":
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodDelete, apiPrefix+"/projects/project-a/builds/build-a", nil, userClaims("alice"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if deletes.Load() != 0 {
		t.Fatalf("expected delete not to be proxied")
	}
}

func TestProjectOwnerCanModifyMemberLabelsButNotOwnerLabel(t *testing.T) {
	var proxied atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/projects/project-a":
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"alice"}}}`)
				return
			}
			proxied.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}), 100, 200)

	body := `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"alice","ebs.io/member-user.bob":"true"}}}`
	req := authenticatedRequest(t, http.MethodPut, apiPrefix+"/projects/project-a", strings.NewReader(body), userClaims("alice"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if proxied.Load() != 1 {
		t.Fatalf("expected project update proxied once, got %d", proxied.Load())
	}

	body = `{"metadata":{"name":"project-a","labels":{"ebs.io/owner-user":"bob"}}}`
	req = authenticatedRequest(t, http.MethodPut, apiPrefix+"/projects/project-a", strings.NewReader(body), userClaims("alice"))
	rec = httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunnerCanCreateItself(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != apiPrefix+"/runners" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-EBS-User") != "runner-a" || r.Header.Get("X-EBS-Scopes") != "ebs:runner" {
			t.Fatalf("missing runner identity headers: %#v", r.Header)
		}
		w.WriteHeader(http.StatusCreated)
	}), 100, 200)
	body := `{"apiVersion":"ebs/v1","kind":"Runner","metadata":{"name":"runner-a","labels":{"ebs.io/runner-type":"ct","ebs.io/runner-arch":"x86_64"}},"spec":{"type":"ct","arch":"x86_64","hostname":"runner-a"},"status":{}}`
	req := authenticatedRequest(t, http.MethodPost, apiPrefix+"/runners", strings.NewReader(body), runnerClaims("runner-a"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunnerScopedJobsRequiresMatchingIdentityAndRejectsSelector(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPrefix+"/runners/runner-a/jobs" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
	}), 100, 200)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/runners/runner-a/jobs", nil, runnerClaims("runner-a"))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected scoped list 200, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, target := range []string{
		apiPrefix + "/runners/runner-b/jobs",
		apiPrefix + "/runners/runner-a/jobs?fieldSelector=status.runner%3Drunner-b",
		apiPrefix + "/jobs?watch=true",
	} {
		req := authenticatedRequest(t, http.MethodGet, target, nil, runnerClaims("runner-a"))
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		want := http.StatusForbidden
		if target == apiPrefix+"/jobs?watch=true" {
			want = http.StatusNotFound
		}
		if rec.Code != want {
			t.Fatalf("expected %s to be denied, got %d", target, rec.Code)
		}
	}
}

func TestRunnerPatchPreservesProtectedFieldsAndBecomesPut(t *testing.T) {
	var puts atomic.Int32
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPrefix+"/runners/runner-a" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"apiVersion":"ebs/v1","kind":"Runner","metadata":{"name":"runner-a","resourceVersion":"7","labels":{"ebs.io/runner-type":"ct","ebs.io/runner-arch":"x86_64","ebs.io/zone":"zone-a"}},"spec":{"type":"ct","arch":"x86_64","hostname":"old","unschedulable":true,"taints":[{"key":"dedicated","effect":"NoSchedule"}]},"status":{"phase":"Idle"}}`)
			return
		}
		if r.Method != http.MethodPut || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("expected converted PUT, got %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Fatalf("decode candidate: %v", err)
		}
		meta := obj["metadata"].(map[string]any)
		labels := meta["labels"].(map[string]any)
		spec := obj["spec"].(map[string]any)
		status := obj["status"].(map[string]any)
		if meta["resourceVersion"] != "7" || labels["ebs.io/zone"] != "zone-a" || spec["unschedulable"] != true || status["phase"] != "Idle" {
			t.Fatalf("protected fields were not preserved: %#v", obj)
		}
		puts.Add(1)
		w.WriteHeader(http.StatusOK)
	}), 100, 200)
	patch := `{"metadata":{"labels":{"ebs.io/runner-type":"ct","ebs.io/runner-arch":"x86_64"}},"spec":{"type":"ct","arch":"x86_64","hostname":"new"}}`
	req := authenticatedRequest(t, http.MethodPatch, apiPrefix+"/runners/runner-a", strings.NewReader(patch), runnerClaims("runner-a"))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || puts.Load() != 1 {
		t.Fatalf("expected protected patch to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunnerWatchConcurrencyLimit(t *testing.T) {
	gw := newTestGateway(t, http.NotFoundHandler(), 100, 200)
	if !gw.acquireRunnerWatch("runner-a") {
		t.Fatal("first runner watch was rejected")
	}
	if gw.acquireRunnerWatch("runner-a") {
		t.Fatal("second runner watch was allowed")
	}
	gw.releaseRunnerWatch("runner-a")
	if !gw.acquireRunnerWatch("runner-a") {
		t.Fatal("runner watch slot was not released")
	}
}

func TestRateLimitReturnsTooManyRequests(t *testing.T) {
	gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 1, 1)

	req := authenticatedRequest(t, http.MethodGet, apiPrefix+"/runners", nil, systemClaims())
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected first request 200, got %d", rec.Code)
	}

	req = authenticatedRequest(t, http.MethodGet, apiPrefix+"/runners", nil, systemClaims())
	rec = httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second request 429, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOpsCanOnlyGetAndListRunners(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-EBS-User") != "operator" || r.Header.Get("X-EBS-Scopes") != "ebs:ops" {
			t.Fatalf("unexpected identity headers: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	gw := newTestGateway(t, upstream, 100, 200)
	for _, path := range []string{apiPrefix + "/runners", apiPrefix + "/runners/runner-a"} {
		req := authenticatedRequest(t, http.MethodGet, path, nil, opsClaims())
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	denied := []struct{ method, path string }{
		{http.MethodGet, apiPrefix + "/runners?watch=true"},
		{http.MethodGet, apiPrefix + "/runners?watch=1"},
		{http.MethodGet, apiPrefix + "/runners/runner-a/status"},
		{http.MethodPost, apiPrefix + "/runners"},
		{http.MethodGet, apiPrefix + "/projects"},
	}
	for _, tc := range denied {
		req := authenticatedRequest(t, tc.method, tc.path, nil, opsClaims())
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func newTestGateway(t *testing.T, upstream http.Handler, rate float64, burst int) *Gateway {
	t.Helper()
	secretFile := filepath.Join(t.TempDir(), "jwt-secret")
	if err := os.WriteFile(secretFile, []byte(base64.StdEncoding.EncodeToString([]byte(testSecret))), 0o600); err != nil {
		t.Fatalf("write jwt secret: %v", err)
	}
	gw, err := NewGateway(Config{
		Port:                  8080,
		APIServerAddr:         "http://ebs-apiserver",
		JWTSecretFile:         secretFile,
		MaxRequestBodyBytes:   1048576,
		RateLimitPerSec:       rate,
		RateLimitBurst:        burst,
		PublicRateLimitPerSec: 20,
		PublicRateLimitBurst:  40,
		PublicMaxListLimit:    100,
	})
	if err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		if r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/admin" {
			_, _ = io.WriteString(rec, `{"metadata":{"name":"admin"},"spec":{"enabled":true,"scopes":["ebs:admin"]}}`)
			return rec.Result(), nil
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/apis/iam.ebs/v1/users/alice" || r.URL.Path == "/apis/iam.ebs/v1/users/bob") {
			name := strings.TrimPrefix(r.URL.Path, "/apis/iam.ebs/v1/users/")
			_, _ = io.WriteString(rec, `{"metadata":{"name":"`+name+`"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`)
			return rec.Result(), nil
		}
		if r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/operator" {
			_, _ = io.WriteString(rec, `{"metadata":{"name":"operator"},"spec":{"enabled":true,"scopes":["ebs:ops"]}}`)
			return rec.Result(), nil
		}
		upstream.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	gw.client.Transport = transport
	gw.proxy.Transport = transport
	return gw
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := f(r)
	if response != nil && response.Request == nil {
		response.Request = r
	}
	return response, err
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

func userClaims(sub string) jwtClaims {
	now := time.Now()
	return jwtClaims{
		Subject:   sub,
		Scopes:    []string{"ebs:user"},
		Issuer:    "ebs-gateway",
		Audience:  "ebs-api",
		IssuedAt:  now.Unix(),
		NotBefore: now.Unix(),
		Exp:       now.Add(time.Hour).Unix(),
		JTI:       "user-test-token",
	}
}

func opsClaims() jwtClaims {
	claims := userClaims("operator")
	claims.Scopes = []string{"ebs:ops"}
	claims.JTI = "ops-test-token"
	return claims
}

func systemClaims() jwtClaims {
	now := time.Now()
	return jwtClaims{
		Subject:   "scheduler",
		Scopes:    []string{"ebs:system"},
		Issuer:    "ebs-gateway",
		Audience:  "ebs-api",
		IssuedAt:  now.Unix(),
		NotBefore: now.Unix(),
		Exp:       now.Add(time.Hour).Unix(),
		JTI:       "system-test-token",
	}
}

func runnerClaims(name string) jwtClaims {
	now := time.Now()
	return jwtClaims{
		Subject: name, Runner: name, Scopes: []string{"ebs:runner"},
		Issuer: "ebs-gateway", Audience: "ebs-api", IssuedAt: now.Unix(), NotBefore: now.Unix(), Exp: now.Add(time.Hour).Unix(), JTI: "runner-test-token",
	}
}
