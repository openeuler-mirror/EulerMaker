package artifact

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg   Config
	store *Store
	auth  Authorizer
}

func NewServer(c Config, a Authorizer) (*http.Server, error) {
	st, e := NewStore(c.DataDir)
	if e != nil {
		return nil, e
	}
	if e = st.CleanupTemporary(c.TemporaryUploadTTL); e != nil {
		return nil, e
	}
	s := &Server{cfg: c, store: st, auth: a}
	return &http.Server{Addr: c.Listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}, nil
}
func NewHandler(c Config, a Authorizer) (http.Handler, error) {
	st, e := NewStore(c.DataDir)
	if e != nil {
		return nil, e
	}
	if e = st.CleanupTemporary(c.TemporaryUploadTTL); e != nil {
		return nil, e
	}
	return &Server{cfg: c, store: st, auth: a}, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		w.WriteHeader(200)
		w.Write([]byte("ok\n"))
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "artifacts" && parts[1] == "v1" {
		s.route(w, r, parts[2:])
		return
	}
	writeErr(w, r, 404, "NotFound", "not found", false, nil)
}
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
	if e = decodeJSON(mp, s.cfg.MaxMetadataSize, &m); e != nil || !validIdentifier(m.JobUID) || m.FileName == "" || len(m.FileName) > 255 || m.Size < 0 || m.Size > s.cfg.MaxFileSize || !validHash(m.SHA256) || (m.Category != CategoryArtifact && m.Category != CategoryLog) {
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
	if code == "IdempotencyConflict" || code == "ArtifactPathConflict" || code == "UploadInProgress" || code == "SequenceGap" || code == "SequenceConflict" || code == "LogAlreadyFinalized" || code == "JobIdentityConflict" || strings.Contains(strings.ToLower(code), "conflict") {
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
	if k := r.Header.Get("Idempotency-Key"); k == "" || len(k) > 128 {
		writeErr(w, r, 400, "InvalidIdempotencyKey", "invalid idempotency key", false, nil)
		return
	}
	var in CompleteManifestRequest
	if decodeJSON(r.Body, 1<<20, &in) != nil {
		writeErr(w, r, 400, "InvalidRequest", "invalid request", false, nil)
		return
	}
	m, e := s.store.CompleteManifest(p, j, id.Name, r.Header.Get("Idempotency-Key"), in)
	if e != nil {
		s.mapErr(w, r, e)
		return
	}
	writeJSON(w, 200, map[string]any{"jobUID": m.JobUID, "generation": m.Generation, "state": m.State, "artifactCount": len(m.Files), "digest": m.Digest})
}
func (s *Server) getManifest(w http.ResponseWriter, r *http.Request, p, j string) {
	g, e := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
	if e != nil {
		writeErr(w, r, 400, "InvalidRequest", "invalid generation", false, nil)
		return
	}
	m, ok := s.store.GetManifest(p, j, r.URL.Query().Get("jobUID"), g)
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

func (s *Server) appendLog(w http.ResponseWriter, r *http.Request, p, j string) {
	id, e := s.identity(r)
	if e != nil {
		writeErr(w, r, 401, "Unauthorized", "invalid runner token", false, nil)
		return
	}
	u := r.Header.Get("X-Job-UID")
	seq, e := strconv.ParseInt(r.Header.Get("X-Log-Sequence"), 10, 64)
	sum := r.Header.Get("X-Content-SHA256")
	if e != nil || u == "" || r.Header.Get("X-Log-Stream") != "combined" || !validHash(sum) {
		writeErr(w, r, 400, "InvalidLogChunk", "invalid log chunk", false, nil)
		return
	}
	if mediaType(r.Header.Get("Content-Type")) != "application/octet-stream" {
		writeErr(w, r, 415, "UnsupportedMediaType", "log chunks require application/octet-stream", false, nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.LogChunkSize*2+(64<<10))
	var rd io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, er := gzip.NewReader(r.Body)
		if er != nil {
			writeErr(w, r, 400, "InvalidLogChunk", "invalid gzip", false, nil)
			return
		}
		defer gz.Close()
		rd = gz
	} else if v := r.Header.Get("Content-Encoding"); v != "" && v != "identity" {
		writeErr(w, r, 415, "UnsupportedEncoding", "unsupported encoding", false, nil)
		return
	}
	data, e := io.ReadAll(io.LimitReader(rd, s.cfg.LogChunkSize+1))
	if e != nil || int64(len(data)) > s.cfg.LogChunkSize || hex.EncodeToString(sha256sum(data)) != sum {
		writeErr(w, r, 422, "LogChunkMismatch", "log chunk mismatch", true, nil)
		return
	}
	if current, ok := s.store.GetLog(p, j, u); ok && seq < current.NextSequence-int64(s.cfg.LogDedupeWindow) {
		writeErr(w, r, 409, "SequenceConflict", "sequence is outside the deduplication window", false, map[string]any{"nextSequence": current.NextSequence})
		return
	}
	if current, ok := s.store.GetLog(p, j, u); ok && current.CommittedBytes+int64(len(data)) > s.cfg.MaxLogSize {
		writeErr(w, r, 413, "LogQuotaExceeded", "log size limit exceeded", false, nil)
		return
	}
	l, e := s.store.AppendLog(p, j, u, id.Name, seq, data, sum)
	if e != nil {
		details := map[string]any{}
		if l != nil {
			details["nextSequence"] = l.NextSequence
		}
		status := 422
		if e.Error() == "SequenceGap" || e.Error() == "SequenceConflict" || e.Error() == "LogAlreadyFinalized" || e.Error() == "JobIdentityConflict" {
			status = 409
		}
		writeErr(w, r, status, e.Error(), e.Error(), false, details)
		return
	}
	writeJSON(w, 200, map[string]any{"stream": "combined", "acceptedSequence": seq, "nextSequence": l.NextSequence, "committedBytes": l.CommittedBytes})
}
func sha256sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }
func (s *Server) logStatus(w http.ResponseWriter, r *http.Request, p, j string) {
	if _, e := s.identity(r); e != nil {
		writeErr(w, r, 401, "Unauthorized", "invalid runner token", false, nil)
		return
	}
	l, ok := s.store.GetLog(p, j, r.URL.Query().Get("jobUID"))
	if !ok {
		writeJSON(w, 200, map[string]any{"stream": "combined", "state": LogOpen, "nextSequence": 0, "committedBytes": 0})
		return
	}
	writeJSON(w, 200, l)
}
func (s *Server) logContent(w http.ResponseWriter, r *http.Request, p, j string) {
	u := r.URL.Query().Get("jobUID")
	l, ok := s.store.GetLog(p, j, u)
	if !ok {
		writeErr(w, r, 404, "NotFound", "log not found", false, nil)
		return
	}
	body, _, _ := s.store.logPaths(p, u)
	if l.State == LogCompleted {
		if a, yes := s.store.GetArtifact(l.ArtifactID); yes {
			body = s.store.artifactPath(a)
		}
	}
	f, e := os.Open(body)
	if e != nil {
		writeErr(w, r, 404, "NotFound", "log not found", false, nil)
		return
	}
	defer f.Close()
	w.Header().Set("X-Log-State", string(l.State))
	w.Header().Set("X-Log-Next-Sequence", strconv.FormatInt(l.NextSequence, 10))
	w.Header().Set("X-Committed-Bytes", strconv.FormatInt(l.CommittedBytes, 10))
	etag := fmt.Sprintf(`"log-%d-%d"`, l.NextSequence, l.CommittedBytes)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if l.ArtifactID != "" {
		w.Header().Set("X-Artifact-ID", l.ArtifactID)
	}
	http.ServeContent(w, r, "container.log", l.UpdatedAt, io.NewSectionReader(f, 0, l.CommittedBytes))
}
func (s *Server) logSSE(w http.ResponseWriter, r *http.Request, p, j string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, r, 500, "StreamingUnsupported", "streaming unsupported", false, nil)
		return
	}
	u := r.URL.Query().Get("jobUID")
	after, supplied, e := recoverySequence(r)
	if e != nil {
		writeErr(w, r, 400, "InvalidRequest", "invalid recovery sequence", false, nil)
		return
	}
	ch, done := s.store.Subscribe(p, j, u)
	defer done()
	var replay []logEvent
	if supplied {
		replay, _, e = s.store.ReplayLog(p, j, u, after, s.cfg.LogReplayWindow)
		if e != nil {
			if e.Error() == "ReplayWindowExceeded" {
				writeErr(w, r, 409, e.Error(), e.Error(), false, nil)
			} else {
				writeErr(w, r, 404, "NotFound", "log not found", false, nil)
			}
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 2000\n\n")
	lastSent := after
	for _, e := range replay {
		writeSSELog(w, e)
		lastSent = e.Sequence
	}
	fl.Flush()
	tick := time.NewTicker(s.cfg.SSEHeartbeat)
	defer tick.Stop()
	for {
		select {
		case e, open := <-ch:
			if !open {
				return
			}
			if e.Complete != nil {
				b, _ := json.Marshal(map[string]any{"artifactID": e.Complete.ID, "size": e.Complete.Size, "sha256": e.Complete.SHA256})
				fmt.Fprintf(w, "event: complete\ndata: %s\n\n", b)
				fl.Flush()
				return
			}
			if supplied && e.Sequence <= lastSent {
				continue
			}
			writeSSELog(w, e)
			lastSent = e.Sequence
			fl.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
func recoverySequence(r *http.Request) (int64, bool, error) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("afterSequence")
	}
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, true, err
}
func writeSSELog(w io.Writer, e logEvent) {
	b, _ := json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(e.Data)})
	fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", e.Sequence, b)
}
func (s *Server) completeLog(w http.ResponseWriter, r *http.Request, p, j string) {
	id, e := s.identity(r)
	if e != nil {
		writeErr(w, r, 401, "Unauthorized", "invalid runner token", false, nil)
		return
	}
	var in CompleteLogRequest
	if decodeJSON(r.Body, 1<<20, &in) != nil || !validHash(in.SHA256) {
		writeErr(w, r, 400, "InvalidRequest", "invalid request", false, nil)
		return
	}
	a, e := s.store.CompleteLog(p, j, in.JobUID, id.Name, in)
	if e != nil {
		s.mapErr(w, r, e)
		return
	}
	writeJSON(w, 200, map[string]any{"state": LogCompleted, "artifactID": a.ID, "relativePath": a.RelativePath, "size": a.Size, "sha256": a.SHA256})
}
