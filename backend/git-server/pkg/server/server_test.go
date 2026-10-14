package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
	return repository.Response{}, false, nil
}
func (f *fakeManager) Delete(string) (repository.Response, bool, error) {
	return repository.Response{}, false, nil
}
func (f *fakeManager) WithRepository(context.Context, string, func(string) error) error {
	return nil
}

func TestSync(t *testing.T) {
	manager := &fakeManager{ready: true, syncResponse: repository.Response{Key: "example.com/repo.git"}}
	handler := New(manager, nil, 1024)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/repo/sync", bytes.NewBufferString(`{"origin_url":"https://example.com/repo.git"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || manager.syncCalls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, manager.syncCalls, recorder.Body.String())
	}
}

func TestDecodeErrorWritesOneResponse(t *testing.T) {
	manager := &fakeManager{ready: true}
	handler := New(manager, nil, 1024)
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
	handler := New(&fakeManager{}, nil, 1024)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/repo/status", bytes.NewBufferString("not-json"))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
