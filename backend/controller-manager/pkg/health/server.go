package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/metrics"
)

type Server struct {
	address        string
	ready          atomic.Bool
	mu             sync.RWMutex
	started        bool
	healthCheckers map[string]controller.HealthChecker
}

func New(address string) *Server {
	return &Server{address: address, healthCheckers: make(map[string]controller.HealthChecker)}
}
func (s *Server) SetReady(value bool) { s.ready.Store(value) }
func (s *Server) AddHealthChecker(name string, checker controller.HealthChecker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("health checker registration is closed")
	}
	if name == "" || checker == nil {
		return fmt.Errorf("health checker name and implementation are required")
	}
	if _, exists := s.healthCheckers[name]; exists {
		return fmt.Errorf("health checker %q already registered", name)
	}
	s.healthCheckers[name] = checker
	return nil
}
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("health server already started")
	}
	s.started = true
	s.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, request *http.Request) {
		failures := s.checkHealth(request.Context())
		if len(failures) != 0 {
			http.Error(w, strings.Join(failures, "\n"), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/metrics", metrics.Handler())
	server := &http.Server{Addr: s.address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) checkHealth(parent context.Context) []string {
	s.mu.RLock()
	names := make([]string, 0, len(s.healthCheckers))
	for name := range s.healthCheckers {
		names = append(names, name)
	}
	sort.Strings(names)
	checkers := make([]controller.HealthChecker, 0, len(names))
	for _, name := range names {
		checkers = append(checkers, s.healthCheckers[name])
	}
	s.mu.RUnlock()

	failures := make([]string, 0)
	for i, checker := range checkers {
		checkCtx, cancel := context.WithTimeout(parent, time.Second)
		err := checker.Check(checkCtx)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", names[i], err))
		}
	}
	return failures
}
