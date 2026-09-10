package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"git-server/pkg/command"
	"git-server/pkg/repository"
	"git-server/pkg/storage"
)

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RepositoryRequest struct {
	OriginURL string `json:"origin_url"`
}

type Server struct {
	manager        repositoryManager
	commands       *command.Executor
	maxRequestBody int64
	mux            *http.ServeMux
}

type repositoryManager interface {
	Ready() bool
	Sync(string) (repository.Response, error)
	Status(string) (repository.Response, bool, error)
	Delete(string) (repository.Response, bool, error)
	WithRepository(context.Context, string, func(string) error) error
}

func New(manager repositoryManager, commands *command.Executor, maxRequestBody int64) *Server {
	s := &Server{manager: manager, commands: commands, maxRequestBody: maxRequestBody, mux: http.NewServeMux()}
	s.mux.HandleFunc("/healthz", method(http.MethodGet, s.health))
	s.mux.HandleFunc("/readyz", method(http.MethodGet, s.ready))
	s.mux.HandleFunc("/metrics", method(http.MethodGet, s.metrics))
	s.mux.HandleFunc("/api/v1/repo/sync", method(http.MethodPost, s.sync))
	s.mux.HandleFunc("/api/v1/repo/status", method(http.MethodPost, s.status))
	s.mux.HandleFunc("/api/v1/repo/delete", method(http.MethodPost, s.delete))
	s.mux.HandleFunc("/command", method(http.MethodPost, s.command))
	return s
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	provider, ok := s.manager.(interface{ Metrics() repository.Metrics })
	if !ok {
		writeError(w, http.StatusNotFound, "NotFound", "metrics are not available")
		return
	}
	m := provider.Metrics()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "git_server_queue_depth %d\n", m.QueueDepth)
	fmt.Fprintf(w, "git_server_active_workers %d\n", m.ActiveWorkers)
	fmt.Fprintf(w, "git_server_repositories %d\n", m.Repositories)
	fmt.Fprintf(w, "git_server_operations_total{operation=\"sync\",result=\"success\"} %d\n", m.SyncSuccess)
	fmt.Fprintf(w, "git_server_operations_total{operation=\"sync\",result=\"failure\"} %d\n", m.SyncFailure)
	fmt.Fprintf(w, "git_server_operations_total{operation=\"delete\",result=\"success\"} %d\n", m.DeleteSuccess)
	fmt.Fprintf(w, "git_server_operations_total{operation=\"delete\",result=\"failure\"} %d\n", m.DeleteFailure)
	fmt.Fprintf(w, "git_server_retries_total %d\n", m.Retries)
	fmt.Fprintf(w, "git_server_operation_duration_seconds_sum{operation=\"sync\"} %g\n", m.SyncDurationSeconds)
	fmt.Fprintf(w, "git_server_operation_duration_seconds_count{operation=\"sync\"} %d\n", m.SyncDurationCount)
	fmt.Fprintf(w, "git_server_operation_duration_seconds_sum{operation=\"delete\"} %g\n", m.DeleteDurationSeconds)
	fmt.Fprintf(w, "git_server_operation_duration_seconds_count{operation=\"delete\"} %d\n", m.DeleteDurationCount)
}

func method(expected string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != expected {
			w.Header().Set("Allow", expected)
			writeError(w, http.StatusMethodNotAllowed, "InvalidRequest", "HTTP method is not allowed")
			return
		}
		handler(w, r)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	if !s.manager.Ready() {
		writeError(w, http.StatusServiceUnavailable, "NotReady", "service is not ready")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) requireReady(w http.ResponseWriter) bool {
	if s.manager.Ready() {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "NotReady", "service is not ready")
	return false
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	if !s.requireReady(w) {
		return
	}
	var request RepositoryRequest
	if !s.decode(w, r, &request) {
		return
	}
	if request.OriginURL == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "origin_url is required")
		return
	}
	response, err := s.manager.Sync(request.OriginURL)
	if err != nil {
		s.writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if !s.requireReady(w) {
		return
	}
	var request RepositoryRequest
	if !s.decode(w, r, &request) {
		return
	}
	if request.OriginURL == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "origin_url is required")
		return
	}
	response, found, err := s.manager.Status(request.OriginURL)
	if err != nil {
		s.writeManagerError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "RepositoryNotFound", "repository was not found")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if !s.requireReady(w) {
		return
	}
	var request RepositoryRequest
	if !s.decode(w, r, &request) {
		return
	}
	if request.OriginURL == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "origin_url is required")
		return
	}
	response, queued, err := s.manager.Delete(request.OriginURL)
	if err != nil {
		s.writeManagerError(w, err)
		return
	}
	if !queued {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		Key string `json:"key"`
	}{Key: response.Key})
}

func (s *Server) command(w http.ResponseWriter, r *http.Request) {
	if !s.requireReady(w) {
		return
	}
	var request command.Request
	if !s.decode(w, r, &request) {
		return
	}
	if err := command.Validate(request); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	var response command.Response
	var kind command.ResultKind
	err := s.manager.WithRepository(r.Context(), request.Repo, func(path string) error {
		var runErr error
		response, kind, runErr = s.commands.Run(r.Context(), path, request)
		return runErr
	})
	if err != nil {
		if repository.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "RepositoryNotFound", "repository is not available")
		} else {
			writeError(w, http.StatusInternalServerError, "InternalError", "command could not be started")
		}
		return
	}
	switch kind {
	case command.Success:
		writeJSON(w, http.StatusOK, response)
	case command.Failed:
		writeJSON(w, http.StatusUnprocessableEntity, response)
	case command.OutputLimit:
		writeJSON(w, http.StatusRequestEntityTooLarge, response)
	case command.TimedOut:
		writeJSON(w, http.StatusGatewayTimeout, response)
	default:
		writeError(w, http.StatusInternalServerError, "InternalError", "command could not be started")
	}
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "RequestTooLarge", "request body is too large")
		} else {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON request")
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "request body must contain one JSON object")
		return false
	}
	return true
}

func (s *Server) writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrConflict):
		writeError(w, http.StatusConflict, "RepositoryConflict", "repository path conflicts with an existing object")
	case strings.Contains(err.Error(), "queue is closed"):
		writeError(w, http.StatusServiceUnavailable, "QueueClosed", "repository queue is closed")
	default:
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
