package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

func LockDirectory(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory failed")
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open state directory lock failed")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state directory is locked by another process")
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

type ControlResponse struct {
	Plan     *Plan   `json:"plan,omitempty"`
	Status   *Status `json:"status,omitempty"`
	Queued   bool    `json:"queued,omitempty"`
	Error    string  `json:"error,omitempty"`
	ExitCode int     `json:"exitCode"`
}

func (e *Engine) controlHandler(lifetime context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plan" && r.URL.Path != "/sync" && r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/status" && r.Method != http.MethodGet || r.URL.Path != "/status" && r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Query().Get("binding")
		if r.Method == http.MethodPost {
			var input struct {
				Binding string `json:"binding"`
			}
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			d.DisallowUnknownFields()
			if d.Decode(&input) != nil || d.Decode(&struct{}{}) != io.EOF {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			name = input.Binding
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(lifetime, cancel)
		defer stop()
		response := ControlResponse{}
		var err error
		switch r.URL.Path {
		case "/plan":
			response.Plan, err = e.Plan(ctx, name)
		case "/sync":
			err = e.Queue(name)
			response.Queued = err == nil
		case "/status":
			var status Status
			status, err = e.Status(name)
			if err == nil {
				response.Status = &status
			}
		}
		if err != nil {
			response.Error = failureOf(err).Message
			response.ExitCode = 1
			if failureOf(err).Blocked {
				response.ExitCode = 2
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if response.Queued {
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(response)
	})
	return mux
}

func (e *Engine) healthHandler(ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, e.metrics())
	})
	return mux
}

// Serve owns the process lock for the entire worker and control-server lifetime.
func (e *Engine) Serve(ctx context.Context) error {
	unlock, err := LockDirectory(e.config.DataDir)
	if err != nil {
		return err
	}
	defer unlock()
	socket := filepath.Join(e.config.DataDir, "control.sock")
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("control socket path is occupied by a non-socket file")
		}
		if err := os.Remove(socket); err != nil {
			return fmt.Errorf("remove stale control socket failed")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect control socket failed")
	}
	local, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on control socket failed")
	}
	defer local.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return fmt.Errorf("set control socket permissions failed")
	}
	public, err := net.Listen("tcp", e.config.Listen)
	if err != nil {
		return fmt.Errorf("listen on health endpoint failed")
	}
	defer public.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var ready atomic.Bool
	control := &http.Server{
		Handler: e.controlHandler(ctx), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: roundTimeout + time.Minute, IdleTimeout: time.Minute,
	}
	health := &http.Server{
		Handler: e.healthHandler(&ready), ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: time.Minute,
	}
	errors := make(chan error, 2)
	go func() { errors <- control.Serve(local) }()
	go func() { errors <- health.Serve(public) }()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	ready.Store(true)
	select {
	case <-ctx.Done():
	case <-errors:
		err = fmt.Errorf("HTTP server stopped unexpectedly")
	}
	ready.Store(false)
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if control.Shutdown(shutdownCtx) != nil {
		_ = control.Close()
	}
	if health.Shutdown(shutdownCtx) != nil {
		_ = health.Close()
	}
	<-done
	return err
}
