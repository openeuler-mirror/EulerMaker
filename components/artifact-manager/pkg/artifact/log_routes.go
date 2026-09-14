package artifact

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

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
