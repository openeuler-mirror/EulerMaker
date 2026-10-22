package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git-server/pkg/repository"
)

type fakeManager struct {
	ready        bool
	syncResponse repository.Response
	syncCalls    int
}

func (f *fakeManager) Ready() bool { return f.ready }
func (f *fakeManager) Sync(string) (repository.Response, error) {
	f.syncCalls++
	return f.syncResponse, nil
}
func (f *fakeManager) Status(string) (repository.Response, bool, error) {
	return f.syncResponse, f.syncResponse.SyncTime != nil, nil
}
func (f *fakeManager) Delete(string) (repository.Response, bool, error) {
	return repository.Response{}, false, nil
}
func (f *fakeManager) WithRepository(context.Context, string, func(string) error) error {
	return nil
}

func TestSync(t *testing.T) {
	manager := &fakeManager{ready: true, syncResponse: repository.Response{Key: "example.com/repo.git"}}
	handler := New(manager, nil, 1024, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/repo/sync", bytes.NewBufferString(`{"origin_url":"https://example.com/repo.git"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || manager.syncCalls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, manager.syncCalls, recorder.Body.String())
	}
}

func TestSyncedRepositoryResponses(t *testing.T) {
	now := time.Now().UTC()
	manager := &fakeManager{ready: true, syncResponse: repository.Response{
		Key: "example.com/repo%2Bname.git", ClonePath: "/git/example.com/repo%252Bname.git", SyncTime: &now,
	}}
	handler := New(manager, nil, 1024, nil)
	for _, tc := range []struct {
		operation string
		status    int
	}{{"sync", http.StatusAccepted}, {"status", http.StatusOK}} {
		t.Run(tc.operation, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/repo/"+tc.operation,
				bytes.NewBufferString(`{"origin_url":"https://example.com/repo+name.git"}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != tc.status || body["clone_path"] != manager.syncResponse.ClonePath ||
				body["sync_time"] != now.Format(time.RFC3339Nano) {
				t.Fatalf("status=%d body=%v", recorder.Code, body)
			}
		})
	}
}

func TestDecodeErrorWritesOneResponse(t *testing.T) {
	manager := &fakeManager{ready: true}
	handler := New(manager, nil, 1024, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/repo/sync", bytes.NewBufferString(`{"unknown":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Code != "InvalidRequest" {
		t.Fatalf("response=%#v err=%v body=%q", response, err, recorder.Body.String())
	}
}

func TestNotReadyBeforeDecode(t *testing.T) {
	handler := New(&fakeManager{}, nil, 1024, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/repo/status", bytes.NewBufferString("not-json"))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
