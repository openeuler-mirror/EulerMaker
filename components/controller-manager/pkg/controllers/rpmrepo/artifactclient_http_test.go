package rpmrepo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scriptedArtifactServer serves one scripted response per request and records what the client sent.
type scriptedArtifactServer struct {
	status      int
	body        string
	headers     map[string]string
	requests    []*http.Request
	requestBody []string
	server      *httptest.Server
}

func newScriptedArtifactServer(t *testing.T, status int, body string, headers map[string]string) *scriptedArtifactServer {
	t.Helper()
	scripted := &scriptedArtifactServer{status: status, body: body, headers: headers}
	scripted.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scripted.requests = append(scripted.requests, r.Clone(context.Background()))
		raw, _ := io.ReadAll(r.Body)
		scripted.requestBody = append(scripted.requestBody, string(raw))
		for key, value := range scripted.headers {
			w.Header().Set(key, value)
		}
		if scripted.status == 0 {
			scripted.status = http.StatusOK
		}
		w.WriteHeader(scripted.status)
		_, _ = io.WriteString(w, scripted.body)
	}))
	t.Cleanup(scripted.server.Close)
	return scripted
}

func (s *scriptedArtifactServer) client(t *testing.T) ArtifactManagerClient {
	t.Helper()
	client, err := newArtifactManagerClient(s.server.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("newArtifactManagerClient: %v", err)
	}
	return client
}

func repositoryResponseBody(t *testing.T, uid string) string {
	t.Helper()
	encoded, err := json.Marshal(RepositoryResponse{
		RepositoryUID: uid,
		State:         RepositoryCreating,
		Attempt:       1,
		UpdatedAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

func TestArtifactClientAddressValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address string
		timeout time.Duration
	}{
		{name: "empty", address: "", timeout: time.Second},
		{name: "scheme", address: "ftp://artifact-manager", timeout: time.Second},
		{name: "no-host", address: "http://", timeout: time.Second},
		{name: "timeout", address: "http://artifact-manager", timeout: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newArtifactManagerClient(tc.address, tc.timeout); err == nil {
				t.Fatalf("address %q with timeout %s must be rejected", tc.address, tc.timeout)
			}
		})
	}
	if _, err := newArtifactManagerClient("http://artifact-manager:8080/base", time.Second); err != nil {
		t.Fatalf("a valid address was rejected: %v", err)
	}
}

func TestArtifactClientBuildsRequestsAndParsesResponses(t *testing.T) {
	ctx := context.Background()

	t.Run("submit-repository", func(t *testing.T) {
		server := newScriptedArtifactServer(t, http.StatusAccepted, repositoryResponseBody(t, "repo-1"), nil)
		client := server.client(t)
		response, err := client.SubmitRepository(ctx, CreateRepositoryRequest{
			RepositoryUID: "repo-1", RepositoryName: testBuild, Project: testProject, BuildName: testBuild,
			TargetOS: testOS, TargetArch: testArch,
			Manifests: []ManifestReference{{JobName: "job-a", JobUID: "uid-job-a"}},
		})
		if err != nil {
			t.Fatalf("SubmitRepository: %v", err)
		}
		if response.RepositoryUID != "repo-1" || response.State != RepositoryCreating {
			t.Fatalf("unexpected response %+v", response)
		}
		if server.requests[0].Method != http.MethodPost || server.requests[0].URL.Path != "/internal/v1/repositories" {
			t.Fatalf("unexpected request %s %s", server.requests[0].Method, server.requests[0].URL.Path)
		}
		if server.requests[0].Header.Get("Content-Type") != "application/json" {
			t.Fatalf("the request must carry a JSON content type")
		}
		var sent CreateRepositoryRequest
		if err := json.Unmarshal([]byte(server.requestBody[0]), &sent); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if sent.RepositoryName != testBuild || len(sent.Manifests) != 1 || sent.Manifests[0].JobUID != "uid-job-a" {
			t.Fatalf("unexpected request body %+v", sent)
		}
	})

	t.Run("get-repository", func(t *testing.T) {
		server := newScriptedArtifactServer(t, http.StatusOK, repositoryResponseBody(t, "repo-1"), nil)
		client := server.client(t)
		if _, err := client.GetRepository(ctx, "repo-1"); err != nil {
			t.Fatalf("GetRepository: %v", err)
		}
		if server.requests[0].Method != http.MethodGet || server.requests[0].URL.Path != "/internal/v1/repositories/repo-1" {
			t.Fatalf("unexpected request %s %s", server.requests[0].Method, server.requests[0].URL.Path)
		}
	})

	t.Run("get-manifest-query", func(t *testing.T) {
		body := `{"project":"project","jobName":"job-a","jobUID":"uid-job-a","state":"Completed"}`
		server := newScriptedArtifactServer(t, http.StatusOK, body, nil)
		client := server.client(t)
		manifest, err := client.GetJobManifest(ctx, testProject, "job-a", "uid-job-a")
		if err != nil {
			t.Fatalf("GetJobManifest: %v", err)
		}
		if manifest.State != ManifestCompleted {
			t.Fatalf("unexpected manifest %+v", manifest)
		}
		request := server.requests[0]
		if request.URL.Path != "/artifacts/v1/projects/"+testProject+"/jobs/job-a/manifest" {
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		if request.URL.Query().Get("jobUID") != "uid-job-a" {
			t.Fatalf("unexpected jobUID query %q", request.URL.RawQuery)
		}
	})

	t.Run("submit-release", func(t *testing.T) {
		body := `{"buildName":"build-a","state":"Prepared","attempt":1,"updatedAt":"2026-09-21T10:00:00Z"}`
		server := newScriptedArtifactServer(t, http.StatusAccepted, body, nil)
		client := server.client(t)
		response, err := client.SubmitRelease(ctx, CreateReleaseRequest{
			BuildName: testBuild, Project: testProject, TargetOS: testOS, TargetArch: testArch,
			SourceRepositoryUID: "repo-1", ExcludeSpecs: []string{"gcc"},
		})
		if err != nil {
			t.Fatalf("SubmitRelease: %v", err)
		}
		if response.State != ReleasePrepared || response.BuildName != testBuild {
			t.Fatalf("unexpected response %+v", response)
		}
		if server.requests[0].URL.Path != "/internal/v1/releases" {
			t.Fatalf("unexpected path %q", server.requests[0].URL.Path)
		}
	})

	t.Run("get-and-activate-release", func(t *testing.T) {
		body := `{"buildName":"build-a","state":"Ready","attempt":1,"contentURL":"/repositories/project/openEuler/aarch64/","updatedAt":"2026-09-21T10:00:00Z"}`
		server := newScriptedArtifactServer(t, http.StatusOK, body, nil)
		client := server.client(t)
		if _, err := client.GetRelease(ctx, testBuild); err != nil {
			t.Fatalf("GetRelease: %v", err)
		}
		activated, err := client.ActivateRelease(ctx, testBuild)
		if err != nil {
			t.Fatalf("ActivateRelease: %v", err)
		}
		if activated.ContentURL == "" {
			t.Fatalf("activation must return the stable entry point")
		}
		if server.requests[0].URL.Path != "/internal/v1/releases/"+testBuild {
			t.Fatalf("unexpected get path %q", server.requests[0].URL.Path)
		}
		if server.requests[1].Method != http.MethodPost || server.requests[1].URL.Path != "/internal/v1/releases/"+testBuild+"/activate" {
			t.Fatalf("unexpected activate request %s %s", server.requests[1].Method, server.requests[1].URL.Path)
		}
	})
}

func TestArtifactClientValidatesRequestsLocally(t *testing.T) {
	client, err := newArtifactManagerClient("http://artifact-manager", time.Second)
	if err != nil {
		t.Fatalf("newArtifactManagerClient: %v", err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{
			name: "repository-without-manifests",
			call: func() error {
				_, err := client.SubmitRepository(ctx, CreateRepositoryRequest{RepositoryUID: "repo-1", RepositoryName: testBuild})
				return err
			},
		},
		{
			name: "repository-without-uid",
			call: func() error { _, err := client.GetRepository(ctx, ""); return err },
		},
		{
			name: "manifest-without-job",
			call: func() error { _, err := client.GetJobManifest(ctx, testProject, "", "uid"); return err },
		},
		{
			name: "release-without-source",
			call: func() error {
				_, err := client.SubmitRelease(ctx, CreateReleaseRequest{BuildName: testBuild})
				return err
			},
		},
		{
			name: "release-without-name",
			call: func() error { _, err := client.GetRelease(ctx, ""); return err },
		},
		{
			name: "activate-without-name",
			call: func() error { _, err := client.ActivateRelease(ctx, ""); return err },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("invalid request must be rejected locally")
			}
			var target *artifactError
			if !errors.As(err, &target) || target.kind != artifactPermanent {
				t.Fatalf("a local contract violation must be permanent, got %v", err)
			}
		})
	}
}

func TestArtifactClientClassifiesResponses(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		status   int
		code     string
		headers  map[string]string
		wantCode string
		check    func(error) bool
		minRetry time.Duration
	}{
		{
			name:     "not-found",
			status:   http.StatusNotFound,
			code:     "RepositoryNotFound",
			wantCode: "RepositoryNotFound",
			check:    isArtifactNotFound,
		},
		{
			name:     "deleting",
			status:   http.StatusConflict,
			code:     "RepositoryDeleting",
			wantCode: "RepositoryDeleting",
			check:    isArtifactDeleting,
		},
		{
			name:     "identity-conflict",
			status:   http.StatusConflict,
			code:     "RepositoryIdentityConflict",
			wantCode: "RepositoryIdentityConflict",
			check:    func(err error) bool { return !isArtifactDeleting(err) && !isArtifactRetryable(err) },
		},
		{
			name:     "expired-input",
			status:   http.StatusGone,
			code:     "MaterializationInputExpired",
			wantCode: "MaterializationInputExpired",
			check:    func(err error) bool { return !isArtifactRetryable(err) },
		},
		{
			name:     "manifest-not-ready",
			status:   http.StatusUnprocessableEntity,
			code:     "ManifestNotReady",
			wantCode: "ManifestNotReady",
			check:    func(err error) bool { return !isArtifactRetryable(err) },
		},
		{
			name:     "unauthorized",
			status:   http.StatusUnauthorized,
			code:     "Unauthorized",
			wantCode: "Unauthorized",
			check:    func(err error) bool { return !isArtifactRetryable(err) },
		},
		{
			name:     "queue-full",
			status:   http.StatusTooManyRequests,
			code:     "RepositoryQueueFull",
			headers:  map[string]string{"Retry-After": "7"},
			wantCode: "RepositoryQueueFull",
			check:    isArtifactRetryable,
			minRetry: 7 * time.Second,
		},
		{
			name:     "storage-unavailable",
			status:   http.StatusServiceUnavailable,
			code:     "RepositoryStorageUnavailable",
			headers:  map[string]string{"Retry-After": time.Now().UTC().Add(time.Minute).Format(http.TimeFormat)},
			wantCode: "RepositoryStorageUnavailable",
			check:    isArtifactRetryable,
			minRetry: 30 * time.Second,
		},
		{
			name:     "internal-error",
			status:   http.StatusInternalServerError,
			code:     "RepositoryInternalError",
			wantCode: "RepositoryInternalError",
			check:    isArtifactRetryable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"code":"` + tc.code + `","message":"scripted"}`
			server := newScriptedArtifactServer(t, tc.status, body, tc.headers)
			client := server.client(t)
			_, err := client.GetRepository(ctx, "repo-1")
			if err == nil {
				t.Fatalf("status %d must fail", tc.status)
			}
			if got := artifactErrorCode(err); got != tc.wantCode {
				t.Fatalf("code = %q, want %q", got, tc.wantCode)
			}
			if !tc.check(err) {
				t.Fatalf("unexpected classification for status %d: %v", tc.status, err)
			}
			if tc.minRetry > 0 {
				if got := artifactRetryAfter(err); got < tc.minRetry {
					t.Fatalf("Retry-After = %s, want at least %s", got, tc.minRetry)
				}
			}
			if len(tc.headers) == 0 && artifactRetryAfter(err) != 0 {
				t.Fatalf("a response without Retry-After must not invent a delay")
			}
		})
	}
}

func TestArtifactClientRejectsUnusableResponses(t *testing.T) {
	ctx := context.Background()

	t.Run("unparsable", func(t *testing.T) {
		server := newScriptedArtifactServer(t, http.StatusOK, "{not json", nil)
		client := server.client(t)
		_, err := client.GetRepository(ctx, "repo-1")
		if err == nil || !isArtifactRetryable(err) {
			t.Fatalf("an unparsable body must be retryable, got %v", err)
		}
		if got := artifactErrorCode(err); got != "UnparsableResponse" {
			t.Fatalf("code = %q, want UnparsableResponse", got)
		}
	})

	t.Run("too-large", func(t *testing.T) {
		server := newScriptedArtifactServer(t, http.StatusOK, strings.Repeat("a", maxResponseBytes+1), nil)
		client := server.client(t)
		_, err := client.GetRepository(ctx, "repo-1")
		if err == nil || !isArtifactRetryable(err) {
			t.Fatalf("an oversized body must be retryable, got %v", err)
		}
		if got := artifactErrorCode(err); got != "ResponseTooLarge" {
			t.Fatalf("code = %q, want ResponseTooLarge", got)
		}
	})

	t.Run("transport", func(t *testing.T) {
		server := newScriptedArtifactServer(t, http.StatusOK, "{}", nil)
		address := server.server.URL
		client, err := newArtifactManagerClient(address, time.Second)
		if err != nil {
			t.Fatalf("newArtifactManagerClient: %v", err)
		}
		server.server.Close()
		_, err = client.GetRepository(ctx, "repo-1")
		if err == nil || !isArtifactRetryable(err) {
			t.Fatalf("a transport failure must be retryable, got %v", err)
		}
	})

	t.Run("context-canceled", func(t *testing.T) {
		server := newScriptedArtifactServer(t, http.StatusOK, "{}", nil)
		client := server.client(t)
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.GetRepository(canceled, "repo-1")
		if err == nil || !isArtifactRetryable(err) {
			t.Fatalf("a canceled call must be retryable, got %v", err)
		}
	})
}
