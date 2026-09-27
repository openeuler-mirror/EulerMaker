package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ebs-gateway/internal/identity"
)

func TestRunnerStatusPatchBecomesValidatedPUT(t *testing.T) {
	old := `{"apiVersion":"ebs/v1","kind":"Runner","metadata":{"name":"runner-1","resourceVersion":"7"},"spec":{"instanceId":"constant","type":"ct"},"status":{"phase":"Offline"}}`
	writes := 0
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/apis/ebs/v1/runners/runner-1/status" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		switch request.Method {
		case http.MethodGet:
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(old))}, nil
		case http.MethodPut:
			writes++
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"phase":"Online"`) || !strings.Contains(string(body), `"instanceId":"constant"`) {
				t.Errorf("invalid complete object forwarded: %s", body)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		default:
			t.Fatalf("unexpected method %s", request.Method)
			return nil, nil
		}
	}))
	token, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/apis/ebs/v1/runners/runner-1/status", strings.NewReader(`{"status":{"phase":"Online"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK || writes != 1 {
		t.Fatalf("status=%d writes=%d body=%q", response.Code, writes, response.Body.String())
	}
}

func TestRunnerCannotUpdateUnassignedJob(t *testing.T) {
	old := `{"apiVersion":"ebs/v1","kind":"Job","metadata":{"name":"job-1","namespace":"team","resourceVersion":"7"},"spec":{},"status":{"runner":"runner-2","phase":"Running"}}`
	api := newTestAPI(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("unassigned Job was written with %s", request.Method)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(old))}, nil
	}))
	token, err := api.tokens.Issue("runner-1", "runner-1", identity.RunnerType, "", time.Hour, api.now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/apis/ebs/v1/projects/team/jobs/job-1/status", strings.NewReader(`{"status":{"phase":"Succeeded"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response := httptest.NewRecorder()
	api.Router().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
