package artifact

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) routeReleaseManagement(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/v1/releases" {
		if r.Method != http.MethodPost {
			method(w, http.MethodPost)
			return
		}
		var request CreateReleaseRequest
		if err := decodeJSON(r.Body, s.cfg.MaxMetadataSize, &request); err != nil {
			writeErr(w, r, http.StatusBadRequest, "InvalidRequest", "invalid request body", false, nil)
			return
		}
		record, status, err := s.releases.submit(request)
		if err != nil {
			s.writeReleaseError(w, r, err)
			return
		}
		w.Header().Set("Location", "/internal/v1/releases/"+record.BuildName)
		writeJSON(w, status, releaseResponse(record))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/internal/v1/releases/")
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[1] == "activate" {
		if !validIdentifier(parts[0]) {
			s.releaseNotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			method(w, http.MethodPost)
			return
		}
		if r.ContentLength > 0 {
			writeErr(w, r, http.StatusBadRequest, "InvalidRequest", "request body is not allowed", false, nil)
			return
		}
		record, status, err := s.releases.activate(parts[0])
		if err != nil {
			s.writeReleaseError(w, r, err)
			return
		}
		writeJSON(w, status, releaseResponse(record))
		return
	}
	if len(parts) != 1 || !validIdentifier(parts[0]) {
		s.releaseNotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		record, ok := s.releases.get(parts[0])
		if !ok {
			s.releaseNotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, releaseResponse(record))
	case http.MethodDelete:
		record, status, err := s.releases.delete(parts[0])
		if err != nil {
			s.writeReleaseError(w, r, err)
			return
		}
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, releaseResponse(record))
	default:
		method(w, "GET, DELETE")
	}
}

func (s *Server) writeReleaseError(w http.ResponseWriter, r *http.Request, err error) {
	var typed *releaseError
	if !errors.As(err, &typed) {
		typed = &releaseError{code: "ReleaseInternalError", status: 500, retryable: true}
	}
	if typed.status == 0 {
		typed.status = http.StatusInternalServerError
	}
	if typed.status == http.StatusTooManyRequests || typed.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeErr(w, r, typed.status, typed.code, typed.code, typed.retryable, nil)
}

func (s *Server) releaseNotFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, r, http.StatusNotFound, "ReleaseNotFound", "release not found", false, nil)
}

func (s *Server) releaseVersionContent(w http.ResponseWriter, r *http.Request) {
	prefix := "/repositories/releases/v1/"
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, prefix), "/", 2)
	if len(parts) != 2 || !validIdentifier(parts[0]) {
		s.releaseNotFound(w, r)
		return
	}
	record, ok := s.releases.get(parts[0])
	if !ok {
		s.releaseNotFound(w, r)
		return
	}
	if record.State != ReleaseReady {
		writeErr(w, r, http.StatusConflict, "ReleaseNotReady", "release is not ready", true, nil)
		return
	}
	s.serveReleaseFile(w, r, record, parts[1])
}

func (s *Server) releaseCurrentContent(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repositories/"), "/", 3)
	if len(parts) != 3 || !validIdentifier(parts[0]) || !validIdentifier(parts[1]) {
		s.releaseNotFound(w, r)
		return
	}
	current := filepath.Join(s.cfg.DataDir, "repositories", parts[0], parts[1], "current")
	target, err := os.Readlink(current)
	if err != nil {
		s.releaseNotFound(w, r)
		return
	}
	targetParts := strings.Split(filepath.ToSlash(target), "/")
	if len(targetParts) != 2 || targetParts[0] != "releases" || !validIdentifier(targetParts[1]) {
		s.releaseNotFound(w, r)
		return
	}
	record, ok := s.releases.get(targetParts[1])
	if !ok || record.State != ReleaseReady || record.Project != parts[0] || record.TargetArch != parts[1] {
		s.releaseNotFound(w, r)
		return
	}
	s.serveReleaseFile(w, r, record, parts[2])
}

func (s *Server) serveReleaseFile(w http.ResponseWriter, r *http.Request, record *ReleaseRecord, value string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		method(w, "GET, HEAD")
		return
	}
	if strings.Contains(r.Header.Get("Range"), ",") {
		writeErr(w, r, http.StatusRequestedRangeNotSatisfiable, "MultipleRangesNotSupported", "multiple ranges are not supported", false, nil)
		return
	}
	relative, err := safeRelative(value)
	if err != nil {
		s.releaseNotFound(w, r)
		return
	}
	path := filepath.Join(s.cfg.DataDir, "repositories", record.Project, record.TargetArch, "releases", record.BuildName, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		s.releaseNotFound(w, r)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		s.releaseNotFound(w, r)
		return
	}
	defer file.Close()
	sum, err := fileSHA256(path)
	if err != nil {
		writeErr(w, r, http.StatusServiceUnavailable, "ReleaseStorageUnavailable", "release storage unavailable", true, nil)
		return
	}
	w.Header().Set("ETag", `"`+sum+`"`)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
