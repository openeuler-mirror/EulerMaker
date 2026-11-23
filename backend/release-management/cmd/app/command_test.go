package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"release-management/pkg/syncer"
)

func commandConfig(t *testing.T) (string, syncer.Config) {
	t.Helper()
	c, err := syncer.LoadConfig("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.DataDir = t.TempDir()
	filename := filepath.Join(c.DataDir, "config.yaml")
	data, _ := json.Marshal(c)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	return filename, c
}

func TestCommandArgumentsAndOfflineLock(t *testing.T) {
	filename, c := commandConfig(t)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"unknown"}, &stdout, &stderr); code != 2 {
		t.Fatal(code)
	}
	if code := Run(context.Background(), []string{"plan", "--config", filename}, &stdout, &stderr); code != 2 {
		t.Fatal(code)
	}
	if code := Run(context.Background(), []string{"sync", "--config", filename, "--binding", c.Bindings[0].Name}, &stdout, &stderr); code != 1 {
		t.Fatal(code)
	}
	unlock, err := syncer.LockDirectory(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if code := Run(context.Background(), []string{"plan", "--config", filename, "--binding", c.Bindings[0].Name}, &stdout, &stderr); code != 1 {
		t.Fatal("standalone plan bypassed lock", code)
	}
}

func TestCommandUsesRunningService(t *testing.T) {
	filename, c := commandConfig(t)
	listener, err := net.Listen("unix", filepath.Join(c.DataDir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plan" {
			_ = json.NewEncoder(w).Encode(syncer.ControlResponse{ExitCode: 2, Error: "empty candidate list is blocked"})
			return
		}
		if r.URL.Path == "/sync" {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(syncer.ControlResponse{Queued: true})
			return
		}
		_ = json.NewEncoder(w).Encode(syncer.ControlResponse{Status: &syncer.Status{Phase: "Idle"}})
	})}
	go server.Serve(listener)
	defer server.Close()
	for _, test := range []struct {
		command string
		exit    int
	}{{"plan", 2}, {"sync", 0}, {"status", 0}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), []string{test.command, "--config", filename, "--binding", c.Bindings[0].Name}, &stdout, &stderr); code != test.exit {
			t.Fatal(code, stderr.String())
		}
		var response syncer.ControlResponse
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
}
