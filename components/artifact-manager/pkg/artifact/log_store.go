package artifact

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
)

func (s *Store) logPaths(p, u string) (string, string, string) {
	base := filepath.Join(s.root, ".logs", p, u)
	return filepath.Join(base, "combined.log"), filepath.Join(base, "combined.index.jsonl"), filepath.Join(s.root, ".metadata/logs", p, u, "combined.json")
}
func (s *Store) recoverLog(l *LogStream) error {
	body, index, meta := s.logPaths(l.Project, l.JobUID)
	if l.State == LogFinalizing && l.ArtifactID != "" {
		if a := s.artifacts[l.ArtifactID]; a != nil && verifyFile(s.artifactPath(a), a.Size, a.SHA256) == nil {
			now := time.Now().UTC()
			a.State, a.UpdatedAt, a.CompletedAt = Completed, now, &now
			l.State, l.UpdatedAt, l.CompletedAt = LogCompleted, now, &now
			if err := atomicJSON(s.artifactMeta(a.ID), a); err != nil {
				return err
			}
			return atomicJSON(meta, l)
		}
		if _, err := os.Stat(body); err == nil {
			l.State, l.ArtifactID, l.FinalSize, l.FinalSHA256 = LogOpen, "", nil, ""
			return atomicJSON(meta, l)
		}
	}
	if l.State == LogCompleted && l.ArtifactID != "" {
		if a := s.artifacts[l.ArtifactID]; a != nil && verifyFile(s.artifactPath(a), a.Size, a.SHA256) == nil {
			return nil
		}
		l.State = LogFailed
		l.Failure = &FailureInfo{Code: "Corrupted", Message: "completed log artifact is missing or invalid", Retryable: false, Time: time.Now().UTC()}
		return atomicJSON(meta, l)
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
			return atomicJSON(meta, l)
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
		l.Failure = &FailureInfo{Code: "Corrupted", Message: "log body is shorter than its index", Retryable: false, Time: time.Now().UTC()}
		return atomicJSON(meta, l)
	}
	if l.State == LogOpen && st.Size() > offset {
		if err := os.Truncate(body, offset); err != nil {
			return err
		}
	}
	if l.State == LogOpen && (l.NextSequence != next || l.CommittedBytes != offset) {
		l.NextSequence, l.CommittedBytes, l.UpdatedAt = next, offset, time.Now().UTC()
		return atomicJSON(meta, l)
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
func (s *Store) AppendLog(p, j, u, runner string, seq int64, data []byte, sum string) (*LogStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := logKey(p, j, u, "combined")
	l := s.logs[k]
	now := time.Now().UTC()
	if l == nil {
		l = &LogStream{SchemaVersion: 1, Project: p, JobName: j, JobUID: u, RunnerName: runner, Stream: "combined", State: LogOpen, CreatedAt: now, UpdatedAt: now}
		s.logs[k] = l
	}
	if l.Project != p || l.JobName != j || l.JobUID != u || l.RunnerName != runner {
		return l, errors.New("JobIdentityConflict")
	}
	if l.State != LogOpen {
		return nil, errors.New("LogAlreadyFinalized")
	}
	body, index, meta := s.logPaths(p, u)
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
	if e = atomicJSON(meta, l); e != nil {
		return nil, e
	}
	s.publishLocked(k, logEvent{Sequence: seq, Data: append([]byte(nil), data...)})
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
func (s *Store) ReplayLog(p, j, u string, after int64, limit int) ([]logEvent, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l := s.logs[logKey(p, j, u, "combined")]
	if l == nil {
		return nil, 0, errors.New("not found")
	}
	first := l.NextSequence - int64(limit)
	if first < 0 {
		first = 0
	}
	if after+1 < first {
		return nil, l.NextSequence, errors.New("ReplayWindowExceeded")
	}
	body, index, _ := s.logPaths(p, u)
	if l.State == LogCompleted && l.ArtifactID != "" {
		if a := s.artifacts[l.ArtifactID]; a != nil {
			body = s.artifactPath(a)
		}
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
	var out []logEvent
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
		out = append(out, logEvent{Sequence: rec.Sequence, Data: data})
	}
	return out, l.NextSequence, sc.Err()
}
func (s *Store) GetLog(p, j, u string) (*LogStream, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.logs[logKey(p, j, u, "combined")]
	if !ok {
		return nil, false
	}
	cp := *l
	return &cp, true
}
func (s *Store) Subscribe(p, j, u string) (chan logEvent, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := logKey(p, j, u, "combined")
	ch := make(chan logEvent, 64)
	if s.subscribers[k] == nil {
		s.subscribers[k] = map[chan logEvent]struct{}{}
	}
	s.subscribers[k][ch] = struct{}{}
	return ch, func() { s.mu.Lock(); defer s.mu.Unlock(); delete(s.subscribers[k], ch); close(ch) }
}
func (s *Store) publishLocked(k string, e logEvent) {
	for ch := range s.subscribers[k] {
		select {
		case ch <- e:
		default:
			delete(s.subscribers[k], ch)
			close(ch)
		}
	}
}
func (s *Store) CompleteLog(p, j, u, runner string, r CompleteLogRequest) (*Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := logKey(p, j, u, "combined")
	l := s.logs[k]
	if l == nil {
		now := time.Now().UTC()
		l = &LogStream{SchemaVersion: 1, Project: p, JobName: j, JobUID: u, RunnerName: runner, Stream: "combined", State: LogOpen, CreatedAt: now, UpdatedAt: now}
		s.logs[k] = l
	}
	if l.State == LogCompleted {
		if r.Stream != "combined" || l.NextSequence != r.LastSequence+1 || l.FinalSize == nil || *l.FinalSize != r.Size || l.FinalSHA256 != r.SHA256 {
			return nil, errors.New("LogCompletionConflict")
		}
		return s.artifacts[l.ArtifactID], nil
	}
	if r.Stream != "combined" || l.NextSequence != r.LastSequence+1 || l.CommittedBytes != r.Size {
		return nil, errors.New("log mismatch")
	}
	body, _, meta := s.logPaths(p, u)
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
	for _, existing := range s.artifacts {
		if existing.Project == p && existing.JobUID == u && existing.RelativePath == "logs/container.log" && existing.State == Completed {
			return nil, errors.New("ArtifactPathConflict")
		}
	}
	now := time.Now().UTC()
	id := newID("art")
	a := &Artifact{SchemaVersion: 1, ID: id, Project: p, JobName: j, JobUID: u, RunnerName: runner, Category: CategoryLog, FileName: "container.log", RelativePath: "logs/container.log", ContentType: "text/plain", Size: r.Size, SHA256: r.SHA256, StorageKey: filepath.ToSlash(filepath.Join("projects", p, "jobs", u, "logs/container.log")), State: Pending, CreatedAt: now, UpdatedAt: now}
	final := s.artifactPath(a)
	if e = os.MkdirAll(filepath.Dir(final), 0750); e != nil {
		return nil, e
	}
	l.State, l.ArtifactID, l.FinalSize, l.FinalSHA256, l.UpdatedAt = LogFinalizing, id, &r.Size, r.SHA256, now
	s.artifacts[id] = a
	if e = atomicJSON(s.artifactMeta(id), a); e != nil {
		return nil, e
	}
	if e = atomicJSON(meta, l); e != nil {
		return nil, e
	}
	if e = os.Rename(body, final); e != nil {
		return nil, e
	}
	a.State, a.CompletedAt = Completed, &now
	if e = atomicJSON(s.artifactMeta(id), a); e != nil {
		return nil, e
	}
	l.State = LogCompleted
	l.ArtifactID = id
	l.FinalSize = &r.Size
	l.FinalSHA256 = r.SHA256
	l.CompletedAt = &now
	l.UpdatedAt = now
	if e = atomicJSON(meta, l); e != nil {
		return nil, e
	}
	s.publishLocked(k, logEvent{Complete: a})
	return a, nil
}
