package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestControlHandler(t *testing.T) {
	e, _, client := testEngine(t)
	handler := e.controlHandler(context.Background())
	for _, command := range []string{"plan", "sync", "status"} {
		method, target := http.MethodPost, "/"+command
		if command == "status" {
			method, target = http.MethodGet, "/status?binding=demo"
		}
		r := httptest.NewRequest(method, target, bytes.NewBufferString(`{"binding":"demo"}`))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var result ControlResponse
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.ExitCode != 0 {
			t.Fatal(result, err)
		}
		if command == "plan" && result.Plan.Difference.Added != 1 {
			t.Fatal(result.Plan)
		}
		if command == "sync" && (w.Code != http.StatusAccepted || !result.Queued) {
			t.Fatal(w.Code, result)
		}
	}
	if client.patches != 0 {
		t.Fatal("plan or queue applied synchronously")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/plan", bytes.NewBufferString(`{"binding":"demo","repository":"https://unexpected.example.com"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatal("untrusted control fields accepted")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status?binding=unknown", nil))
	var result ControlResponse
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result.ExitCode != 2 {
		t.Fatal(result)
	}
}

func TestHealthAndWebhookDisabled(t *testing.T) {
	e, _, _ := testEngine(t)
	var ready atomic.Bool
	handler := e.healthHandler(&ready)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
	ready.Store(true)
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatal(path, w.Code)
		}
	}
	for _, path := range []string{"/webhooks/atomgit/release", "/plan", "/sync", "/status"} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatal(path, w.Code)
		}
	}
}

func TestServerLifecycleAndLock(t *testing.T) {
	e, _, _ := testEngine(t)
	e.config.Listen = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- e.Serve(ctx) }()
	defer cancel()
	socket := filepath.Join(e.config.DataDir, "control.sock")
	waitFor(t, func() bool { _, err := os.Stat(socket); return err == nil })
	if unlock, err := LockDirectory(e.config.DataDir); err == nil {
		unlock()
		t.Fatal("second writer acquired lock")
	}
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get("http://local/status?binding=demo")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	unlock, err := LockDirectory(e.config.DataDir)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	unlock()
}
