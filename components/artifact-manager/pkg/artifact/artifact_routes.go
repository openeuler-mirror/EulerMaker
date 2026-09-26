package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) route(w http.ResponseWriter, r *http.Request, p []string) {
	if len(p) == 3 && p[0] == "artifacts" && p[1] != "" && p[2] == "content" && r.Method == http.MethodGet {
		s.download(w, r, p[1])
		return
	}
	if len(p) < 4 || p[0] != "projects" || p[2] != "jobs" {
		writeErr(w, r, 404, "NotFound", "not found", false, nil)
		return
	}
	project, job := p[1], p[3]
	if !validIdentifier(project) || !validIdentifier(job) {
		writeErr(w, r, 400, "InvalidRequest", "invalid project or job name", false, nil)
		return
	}
	if len(p) == 5 && p[4] == "artifacts" {
		if r.Method == http.MethodPost {
			s.upload(w, r, project, job)
		} else if r.Method == http.MethodGet {
			s.list(w, r, project, job)
		} else {
			method(w, "GET, POST")
		}
		return
	}
	if len(p) == 6 && p[4] == "manifest" && p[5] == "complete" && r.Method == http.MethodPost {
		s.completeManifest(w, r, project, job)
		return
	}
	if len(p) == 5 && p[4] == "manifest" && r.Method == http.MethodGet {
		s.getManifest(w, r, project, job)
		return
	}
	if len(p) >= 6 && p[4] == "logs" {
		switch p[5] {
		case "chunks":
			if r.Method == http.MethodPost {
				s.appendLog(w, r, project, job)
				return
			}
		case "status":
			if r.Method == http.MethodGet {
				s.logStatus(w, r, project, job)
				return
			}
		case "content":
			if r.Method == http.MethodGet {
				s.logContent(w, r, project, job)
				return
			}
		case "stream":
			if r.Method == http.MethodGet {
				s.logSSE(w, r, project, job)
				return
			}
		case "complete":
			if r.Method == http.MethodPost {
				s.completeLog(w, r, project, job)
				return
			}
		}
	}
	writeErr(w, r, 404, "NotFound", "not found", false, nil)
}
func method(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", 405)
}
func token(r *http.Request) (string, error) {
	v := strings.Fields(r.Header.Get("Authorization"))
	if len(v) != 2 || v[0] != "Bearer" {
		return "", errors.New("missing bearer token")
	}
	return v[1], nil
}
func (s *Server) identity(r *http.Request) (Identity, error) {
	t, e := token(r)
	if e != nil {
		return Identity{}, e
	}
	return s.auth.Authenticate(r.Context(), t)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string, retry bool, d map[string]any) {
	writeJSON(w, status, APIError{Code: code, Message: msg, Retryable: retry, RequestID: r.Header.Get("X-Request-ID"), Details: d})
}
func decodeJSON(rd io.Reader, max int64, v any) error {
	d := json.NewDecoder(io.LimitReader(rd, max+1))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		return errors.New("trailing json")
	}
	return nil
}
func validHash(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, e := hex.DecodeString(v)
	return e == nil && v == strings.ToLower(v)
}
func validIdentifier(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return v != "." && v != ".."
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, p, j string) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UploadTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	id, e := s.identity(r)
	if e != nil {
		writeErr(w, r, 401, "Unauthorized", "invalid runner token", false, nil)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		writeErr(w, r, 400, "InvalidIdempotencyKey", "invalid idempotency key", false, nil)
		return
	}
	ct, params, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || ct != "multipart/form-data" || params["boundary"] == "" {
		writeErr(w, r, 400, "InvalidMultipartRequest", "invalid multipart request", false, nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxFileSize+s.cfg.MaxMetadataSize+(1<<20))
	mr := multipart.NewReader(r.Body, params["boundary"])
	mp, e := mr.NextPart()
	if e != nil || !s.validPartHeaders(mp) || mp.FormName() != "metadata" || mediaType(mp.Header.Get("Content-Type")) != "application/json" {
		writeErr(w, r, 400, "InvalidMultipartRequest", "metadata must be first", false, nil)
		return
	}
	var m UploadMetadata
	if e = decodeJSON(mp, s.cfg.MaxMetadataSize, &m); e != nil || m.FileName == "" || len(m.FileName) > 255 || m.Size < 0 || m.Size > s.cfg.MaxFileSize || !validHash(m.SHA256) || (m.Category != CategoryArtifact && m.Category != CategoryLog) {
		writeErr(w, r, 422, "InvalidArtifactMetadata", "invalid metadata", false, nil)
		return
	}
	fp, e := mr.NextPart()
	if e != nil || !s.validPartHeaders(fp) || fp.FormName() != "file" || mediaType(fp.Header.Get("Content-Type")) != "application/octet-stream" {
		writeErr(w, r, 400, "InvalidMultipartRequest", "file must be second", false, nil)
		return
	}
	a, ir, replay, e := s.store.BeginUpload(p, j, id.Name, key, m, s.cfg.MaxJobSize)
	if e != nil {
		s.mapErr(w, r, e)
		return
	}
	if replay {
		writeJSON(w, 200, map[string]any{"artifact": a})
		return
	}
	tmp := filepath.Join(s.cfg.DataDir, ".uploads", a.ID+".tmp")
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		s.store.FailUpload(a, ir, "StorageError", e.Error())
		writeErr(w, r, 503, "StorageError", "storage unavailable", true, nil)
		return
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(fp, m.Size+1))
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	extra, xe := mr.NextPart()
	if e != nil || xe != io.EOF || extra != nil || n != m.Size || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		os.Remove(tmp)
		s.store.FailUpload(a, ir, "ArtifactChecksumMismatch", "file size or digest mismatch")
		writeErr(w, r, 422, "ArtifactChecksumMismatch", "file size or digest mismatch", true, nil)
		return
	}
	if time.Now().After(id.ExpiresAt) {
		os.Remove(tmp)
		s.store.FailUpload(a, ir, "TokenExpired", "token expired")
		writeErr(w, r, 401, "Unauthorized", "token expired", false, nil)
		return
	}
	if e = s.store.CompleteUpload(a, ir, tmp); e != nil {
		if !s.store.UploadCommitted(a) {
			s.store.FailUpload(a, ir, "StorageError", e.Error())
		}
		writeErr(w, r, 503, "StorageError", "storage unavailable", true, nil)
		return
	}
	writeJSON(w, 201, map[string]any{"artifact": a})
}
func mediaType(v string) string { t, _, _ := mime.ParseMediaType(v); return t }
func (s *Server) validPartHeaders(p *multipart.Part) bool {
	if len(p.Header) > s.cfg.MaxPartHeaders {
		return false
	}
	var total int64
	for name, values := range p.Header {
		if len(values) != 1 && strings.EqualFold(name, "Content-Disposition") {
			return false
		}
		for _, value := range values {
			line := int64(len(name) + len(value) + 2)
			total += line
			if line > s.cfg.MaxHeaderLineSize || strings.ContainsAny(value, "\x00\r\n") {
				return false
			}
		}
	}
	return total <= s.cfg.MaxPartHeaderBytes
}
func (s *Server) mapErr(w http.ResponseWriter, r *http.Request, e error) {
	code := e.Error()
	status := 422
	if code == "JobQuotaExceeded" {
		status = 413
	}
	if code == "IdempotencyConflict" || code == "ArtifactPathConflict" || code == "UploadInProgress" || code == "SequenceGap" || code == "SequenceConflict" || code == "LogAlreadyFinalized" || code == "ManifestAlreadyCompleted" || strings.Contains(strings.ToLower(code), "conflict") {
		status = 409
	}
	writeErr(w, r, status, code, code, status >= 500, nil)
}
func (s *Server) completeManifest(w http.ResponseWriter, r *http.Request, p, j string) {
	id, e := s.identity(r)
	if e != nil {
		writeErr(w, r, 401, "Unauthorized", "invalid runner token", false, nil)
		return
	}
	var in CompleteManifestRequest
	if decodeJSON(r.Body, 1<<20, &in) != nil {
		writeErr(w, r, 400, "InvalidRequest", "invalid request", false, nil)
		return
	}
	m, e := s.store.CompleteManifest(p, j, id.Name, in)
	if e != nil {
		s.mapErr(w, r, e)
		return
	}
	writeJSON(w, 200, map[string]any{"jobUID": m.JobUID, "state": m.State, "artifactCount": len(m.Files), "digest": m.Digest})
}
func (s *Server) getManifest(w http.ResponseWriter, r *http.Request, p, j string) {
	m, ok := s.store.GetManifest(p, j, r.URL.Query().Get("jobUID"))
	if !ok {
		writeErr(w, r, 404, "NotFound", "manifest not found", false, nil)
		return
	}
	writeJSON(w, 200, m)
}
func (s *Server) list(w http.ResponseWriter, r *http.Request, p, j string) {
	cat := Category(r.URL.Query().Get("category"))
	items, _ := s.store.ListArtifacts(p, j, r.URL.Query().Get("jobUID"), cat)
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) download(w http.ResponseWriter, r *http.Request, id string) {
	a, ok := s.store.GetArtifact(id)
	if !ok || a.State != Completed {
		writeErr(w, r, 404, "NotFound", "artifact not found", false, nil)
		return
	}
	f, e := os.Open(s.store.artifactPath(a))
	if e != nil {
		writeErr(w, r, 404, "NotFound", "artifact content not found", false, nil)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("ETag", `"`+a.SHA256+`"`)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", a.FileName))
	w.Header().Set("Content-Type", a.ContentType)
	http.ServeContent(w, r, a.FileName, st.ModTime(), f)
}
