package upload

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"artifact-manager/pkg/fileutil"
)

func (s *Store) LogPaths(p, j string) (string, string, string) {
	base := filepath.Join(s.root, ".logs", p, j)
	return filepath.Join(base, "combined.log"), filepath.Join(base, "combined.index.jsonl"), filepath.Join(s.root, ".metadata/jobs", p, j, "logs", "combined.json")
}

// loadLog requires the write lock: interrupted writes may need recovery before use.
func (s *Store) loadLog(p, j string) (*LogStream, error) {
	for _, part := range []string{p, j} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			return nil, errors.New("invalid log location")
		}
	}
	_, _, meta := s.LogPaths(p, j)
	data, err := os.ReadFile(meta)
	if err != nil {
		return nil, err
	}
	var l LogStream
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, err
	}
	if l.Project != p || l.JobName != j || l.Stream != "combined" {
		return nil, errors.New("log identity mismatch")
	}
	k := logKey(p, j, "combined")
	if _, interrupted := s.logRecovery[k]; interrupted || l.State == LogFinalizing {
		if err := s.recoverLog(&l); err != nil {
			return &l, err
		}
		delete(s.logRecovery, k)
	}
	return &l, nil
}

func (s *Store) loadOrCreateLog(p, j, runner string) (*LogStream, error) {
	l, err := s.loadLog(p, j)
	if err == nil {
		return l, nil
	}
	if l != nil || !os.IsNotExist(err) {
		return nil, err
	}
	now := time.Now().UTC()
	l = &LogStream{
		SchemaVersion: 1,
		Project:       p,
		JobName:       j,
		RunnerName:    runner,
		Stream:        "combined",
		State:         LogOpen,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	_, _, meta := s.LogPaths(p, j)
	// Persist identity before touching the body or index, including the first chunk.
	if err := fileutil.AtomicJSON(meta, l); err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Store) recoverLog(l *LogStream) error {
	body, index, meta := s.LogPaths(l.Project, l.JobName)
	if l.State == LogFinalizing && l.ArtifactID != "" {
		if a, err := s.readArtifact(l.Project, l.JobName, l.ArtifactID); err == nil && fileutil.VerifyFile(s.ArtifactPath(a), a.Size, a.SHA256) == nil {
			now := time.Now().UTC()
			a.State, a.UpdatedAt, a.CompletedAt = Completed, now, &now
			l.State, l.UpdatedAt, l.CompletedAt = LogCompleted, now, &now
			if err := s.persistArtifact(l.Project, l.JobName, a); err != nil {
				return err
			}
			return fileutil.AtomicJSON(meta, l)
		}
		if _, err := os.Stat(body); err == nil {
			l.State, l.ArtifactID, l.FinalSize, l.FinalSHA256 = LogOpen, "", nil, ""
			return fileutil.AtomicJSON(meta, l)
		}
	}
	if l.State == LogCompleted && l.ArtifactID != "" {
		if a, err := s.readArtifact(l.Project, l.JobName, l.ArtifactID); err == nil && fileutil.VerifyFile(s.ArtifactPath(a), a.Size, a.SHA256) == nil {
			return nil
		}
		l.State = LogFailed
		l.Failure = &FailureInfo{
			Code:      "Corrupted",
			Message:   "completed log artifact is missing or invalid",
			Retryable: false,
			Time:      time.Now().UTC(),
		}
		return fileutil.AtomicJSON(meta, l)
	}
	data, err := os.ReadFile(index)
	if os.IsNotExist(err) {
		data = nil
	} else if err != nil {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if cut := strings.LastIndexByte(string(data), '\n'); cut >= 0 {
			data = data[:cut+1]
		} else {
			data = nil
		}
		if err := os.WriteFile(index, data, 0640); err != nil {
			return err
		}
	}
	var offset, next int64
	for _, line := range bytesLines(data) {
		var rec LogChunkRecord
		if json.Unmarshal(line, &rec) != nil || rec.Sequence != next || rec.StartOffset != offset || rec.Size < 0 {
			l.State = LogFailed
			l.Failure = &FailureInfo{Code: "Corrupted", Message: "invalid log index", Retryable: false, Time: time.Now().UTC()}
			return fileutil.AtomicJSON(meta, l)
		}
		next++
		offset += rec.Size
	}
	st, err := os.Stat(body)
	if os.IsNotExist(err) && offset == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Size() < offset {
		l.State = LogFailed
		l.Failure = &FailureInfo{
			Code:      "Corrupted",
			Message:   "log body is shorter than its index",
			Retryable: false,
			Time:      time.Now().UTC(),
		}
		return fileutil.AtomicJSON(meta, l)
	}
	if l.State == LogOpen && st.Size() > offset {
		if err := os.Truncate(body, offset); err != nil {
			return err
		}
	}
	if l.State == LogOpen && (l.NextSequence != next || l.CommittedBytes != offset) {
		l.NextSequence, l.CommittedBytes, l.UpdatedAt = next, offset, time.Now().UTC()
		return fileutil.AtomicJSON(meta, l)
	}
	return nil
}

func bytesLines(data []byte) [][]byte {
	var lines [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		if i > 0 {
			lines = append(lines, data[:i])
		}
		data = data[i+1:]
	}
	return lines
}

func (s *Store) AppendLog(p, j, runner string, seq int64, data []byte, sum string) (*LogStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := logKey(p, j, "combined")
	l, err := s.loadOrCreateLog(p, j, runner)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if l.Project != p || l.JobName != j || l.RunnerName != runner {
		return l, errors.New("JobIdentityConflict")
	}
	if l.State != LogOpen {
		return nil, errors.New("LogAlreadyFinalized")
	}
	body, index, meta := s.LogPaths(p, j)
	if err := os.MkdirAll(filepath.Dir(body), 0750); err != nil {
		return nil, err
	}
	if seq < l.NextSequence {
		rec, e := findLogRecord(index, seq)
		if e == nil && rec.SHA256 == sum {
			return l, nil
		}
		return nil, errors.New("SequenceConflict")
	}
	if seq > l.NextSequence {
		return nil, errors.New("SequenceGap")
	}
	// Keep only a recovery marker on failure, never the LogStream metadata.
	s.logRecovery[k] = struct{}{}
	f, e := os.OpenFile(body, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if e != nil {
		return nil, e
	}
	n, e := f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	if e != nil {
		return nil, e
	}
	rec := LogChunkRecord{Sequence: seq, StartOffset: l.CommittedBytes, Size: int64(n), SHA256: sum}
	ix, e := os.OpenFile(index, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if e != nil {
		return nil, e
	}
	e = json.NewEncoder(ix).Encode(rec)
	if e == nil {
		e = ix.Sync()
	}
	ix.Close()
	if e != nil {
		return nil, e
	}
	l.NextSequence++
	l.CommittedBytes += int64(n)
	l.UpdatedAt = now
	if e = fileutil.AtomicJSON(meta, l); e != nil {
		return nil, e
	}
	delete(s.logRecovery, k)
	s.publishLocked(k, LogEvent{Sequence: seq, Data: append([]byte(nil), data...)})
	return l, nil
}

func findLogRecord(path string, seq int64) (LogChunkRecord, error) {
	f, e := os.Open(path)
	if e != nil {
		return LogChunkRecord{}, e
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r LogChunkRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Sequence == seq {
			return r, nil
		}
	}
	return LogChunkRecord{}, errors.New("not found")
}

func (s *Store) ReplayLog(p, j string, after int64, limit int) ([]LogEvent, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.loadLog(p, j)
	if err != nil {
		return nil, 0, err
	}
	first := l.NextSequence - int64(limit)
	if first < 0 {
		first = 0
	}
	if after+1 < first {
		return nil, l.NextSequence, errors.New("ReplayWindowExceeded")
	}
	body, index, _ := s.LogPaths(p, j)
	if l.State == LogCompleted && l.ArtifactID != "" {
		a, err := s.readArtifact(p, j, l.ArtifactID)
		if err != nil {
			return nil, l.NextSequence, err
		}
		body = s.ArtifactPath(a)
	}
	bf, err := os.Open(body)
	if err != nil {
		return nil, l.NextSequence, err
	}
	defer bf.Close()
	ix, err := os.Open(index)
	if err != nil {
		return nil, l.NextSequence, err
	}
	defer ix.Close()
	var out []LogEvent
	sc := bufio.NewScanner(ix)
	for sc.Scan() {
		var rec LogChunkRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			return nil, l.NextSequence, errors.New("corrupted index")
		}
		if rec.Sequence <= after {
			continue
		}
		data := make([]byte, rec.Size)
		if _, err := bf.ReadAt(data, rec.StartOffset); err != nil {
			return nil, l.NextSequence, err
		}
		out = append(out, LogEvent{Sequence: rec.Sequence, Data: data})
	}
	return out, l.NextSequence, sc.Err()
}

func (s *Store) GetLog(p, j string) (*LogStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.loadLog(p, j)
	return l, err == nil
}

func (s *Store) Subscribe(p, j string) (chan LogEvent, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := logKey(p, j, "combined")
	ch := make(chan LogEvent, 64)
	if s.subscribers[k] == nil {
		s.subscribers[k] = map[chan LogEvent]struct{}{}
	}
	s.subscribers[k][ch] = struct{}{}
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, exists := s.subscribers[k][ch]; exists {
			delete(s.subscribers[k], ch)
			close(ch)
		}
		if len(s.subscribers[k]) == 0 {
			delete(s.subscribers, k)
		}
	}
}

func (s *Store) publishLocked(k string, e LogEvent) {
	for ch := range s.subscribers[k] {
		select {
		case ch <- e:
		default:
			delete(s.subscribers[k], ch)
			close(ch)
		}
	}
	if len(s.subscribers[k]) == 0 {
		delete(s.subscribers, k)
	}
}

func (s *Store) CompleteLog(p, j, runner string, r CompleteLogRequest) (*Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := logKey(p, j, "combined")
	l, err := s.loadOrCreateLog(p, j, runner)
	if err != nil {
		return nil, err
	}
	if l.State == LogCompleted {
		if r.Stream != "combined" || l.NextSequence != r.LastSequence+1 || l.FinalSize == nil || *l.FinalSize != r.Size || l.FinalSHA256 != r.SHA256 {
			return nil, errors.New("LogCompletionConflict")
		}
		return s.readArtifact(p, j, l.ArtifactID)
	}
	if l.State != LogOpen {
		return nil, errors.New("LogAlreadyFinalized")
	}
	manifest, err := s.readManifest(p, j)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if manifest != nil {
		return nil, errors.New("ManifestAlreadyCompleted")
	}
	seal, err := s.readSeal(p, j)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if seal != nil {
		return nil, &SealError{"ManifestSealing", 409, false}
	}
	if r.Stream != "combined" || l.NextSequence != r.LastSequence+1 || l.CommittedBytes != r.Size {
		return nil, errors.New("log mismatch")
	}
	body, _, meta := s.LogPaths(p, j)
	h := sha256.New()
	f, e := os.Open(body)
	if os.IsNotExist(e) && r.Size == 0 {
		if e = os.MkdirAll(filepath.Dir(body), 0750); e == nil {
			e = os.WriteFile(body, nil, 0640)
		}
		if e == nil {
			f, e = os.Open(body)
		}
	}
	if e != nil {
		return nil, e
	}
	_, e = io.Copy(h, f)
	f.Close()
	if e != nil || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		return nil, errors.New("log digest mismatch")
	}
	artifacts, err := s.readJobArtifacts(p, j)
	if err != nil {
		return nil, err
	}
	for _, existing := range artifacts {
		if existing.RelativePath == "logs/container.log" && existing.State == Completed {
			return nil, errors.New("ArtifactPathConflict")
		}
	}
	now := time.Now().UTC()
	id := fileutil.NewID("art")
	a := &Artifact{
		SchemaVersion: 1,
		ID:            id,
		Category:      CategoryLog,
		FileName:      "container.log",
		RelativePath:  "logs/container.log",
		ContentType:   "text/plain",
		Size:          r.Size,
		SHA256:        r.SHA256,
		StorageKey:    filepath.ToSlash(filepath.Join("projects", p, "jobs", j, "logs/container.log")),
		State:         Pending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	final := s.ArtifactPath(a)
	if e = os.MkdirAll(filepath.Dir(final), 0750); e != nil {
		return nil, e
	}
	s.logRecovery[k] = struct{}{}
	l.State, l.ArtifactID, l.FinalSize, l.FinalSHA256, l.UpdatedAt = LogFinalizing, id, &r.Size, r.SHA256, now
	if e = s.persistArtifact(p, j, a); e != nil {
		return nil, e
	}
	if e = fileutil.AtomicJSON(meta, l); e != nil {
		return nil, e
	}
	if e = os.Rename(body, final); e != nil {
		return nil, e
	}
	a.State, a.CompletedAt = Completed, &now
	if e = s.persistArtifact(p, j, a); e != nil {
		return nil, e
	}
	l.State = LogCompleted
	l.ArtifactID = id
	l.FinalSize = &r.Size
	l.FinalSHA256 = r.SHA256
	l.CompletedAt = &now
	l.UpdatedAt = now
	if e = fileutil.AtomicJSON(meta, l); e != nil {
		return nil, e
	}
	delete(s.logRecovery, k)
	s.publishLocked(k, LogEvent{Complete: a})
	return a, nil
}
