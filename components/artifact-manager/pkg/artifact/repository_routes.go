package artifact

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) routeRepositoryManagement(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/v1/repositories" {
		if r.Method != http.MethodPost {
			method(w, http.MethodPost)
			return
		}
		var request CreateRepositoryRequest
		if err := decodeJSON(r.Body, s.cfg.MaxMetadataSize, &request); err != nil {
			writeErr(w, r, http.StatusBadRequest, "InvalidRequest", "invalid request body", false, nil)
			return
		}
		record, status, err := s.repositories.submit(request)
		if err != nil {
			s.writeRepositoryError(w, r, err)
			return
		}
		w.Header().Set("Location", "/internal/v1/repositories/"+record.RepositoryUID)
		writeJSON(w, status, repositoryResponse(record))
		return
	}
	uid := strings.TrimPrefix(r.URL.Path, "/internal/v1/repositories/")
	if !validIdentifier(uid) || strings.Contains(uid, "/") {
		writeErr(w, r, http.StatusNotFound, "RepositoryNotFound", "repository not found", false, nil)
		return
	}
	switch r.Method {
	case http.MethodGet:
		record, ok := s.repositories.get(uid)
		if !ok {
			writeErr(w, r, http.StatusNotFound, "RepositoryNotFound", "repository not found", false, nil)
			return
		}
		writeJSON(w, http.StatusOK, repositoryResponse(record))
	case http.MethodDelete:
		record, status, err := s.repositories.delete(uid)
		if err != nil {
			s.writeRepositoryError(w, r, err)
			return
		}
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, repositoryResponse(record))
	default:
		method(w, "GET, DELETE")
	}
}

func (s *Server) writeRepositoryError(w http.ResponseWriter, r *http.Request, err error) {
	var typed *repositoryError
	if !errors.As(err, &typed) {
		typed = &repositoryError{code: "RepositoryInternalError", status: http.StatusInternalServerError, retryable: true}
	}
	if typed.status == 0 {
		typed.status = http.StatusInternalServerError
	}
	if typed.status == http.StatusTooManyRequests || typed.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeErr(w, r, typed.status, typed.code, typed.code, typed.retryable, nil)
}

func (s *Server) repositoryContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		method(w, "GET, HEAD")
		return
	}
	if strings.Contains(r.Header.Get("Range"), ",") {
		writeErr(w, r, http.StatusRequestedRangeNotSatisfiable, "MultipleRangesNotSupported", "multiple ranges are not supported", false, nil)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repositories/v1/"), "/", 2)
	if len(parts) != 2 || !validIdentifier(parts[0]) {
		writeErr(w, r, http.StatusNotFound, "RepositoryNotFound", "repository not found", false, nil)
		return
	}
	record, ok := s.repositories.get(parts[0])
	if !ok {
		writeErr(w, r, http.StatusNotFound, "RepositoryNotFound", "repository not found", false, nil)
		return
	}
	if record.State == RepositoryCreating {
		writeErr(w, r, http.StatusConflict, "RepositoryNotReady", "repository is not ready", true, nil)
		return
	}
	if record.State != RepositoryReady {
		writeErr(w, r, http.StatusGone, "RepositoryUnavailable", "repository is unavailable", false, nil)
		return
	}
	relative, err := safeRelative(parts[1])
	if err != nil {
		writeErr(w, r, http.StatusNotFound, "RepositoryContentNotFound", "repository content not found", false, nil)
		return
	}
	path := filepath.Join(repositoryVersionPath(s.cfg.DataDir, record.Project, record.TargetArch, record.BuildName, record.RepositoryUID), filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		writeErr(w, r, http.StatusNotFound, "RepositoryContentNotFound", "repository content not found", false, nil)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeErr(w, r, http.StatusNotFound, "RepositoryContentNotFound", "repository content not found", false, nil)
		return
	}
	defer file.Close()
	sum, err := fileSHA256(path)
	if err != nil {
		writeErr(w, r, http.StatusServiceUnavailable, "RepositoryStorageUnavailable", "repository storage unavailable", true, nil)
		return
	}
	w.Header().Set("ETag", `"`+sum+`"`)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
