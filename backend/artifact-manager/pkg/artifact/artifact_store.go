package artifact

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	root        string
	mu          sync.RWMutex
	artifacts   map[string]*Artifact
	manifests   map[string]*JobUploadManifest
	logs        map[string]*LogStream
	uploads     map[string]*Artifact
	subscribers map[string]map[chan logEvent]struct{}
}

type logEvent struct {
	Sequence int64
	Data     []byte
	Complete *Artifact
}

func NewStore(root string) (*Store, error) {
	s := &Store{root: root, artifacts: map[string]*Artifact{}, manifests: map[string]*JobUploadManifest{}, logs: map[string]*LogStream{}, uploads: map[string]*Artifact{}, subscribers: map[string]map[chan logEvent]struct{}{}}
	for _, d := range []string{".uploads", ".metadata/artifacts", ".metadata/jobs", ".metadata/logs", ".logs", "projects", ".quarantine"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0750); err != nil {
			return nil, err
		}
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) CleanupTemporary(ttl time.Duration) error {
	entries, err := os.ReadDir(filepath.Join(s.root, ".uploads"))
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-ttl)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(s.root, ".uploads", entry.Name())
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
	return nil
}

func (s *Store) load() error {
	return filepath.WalkDir(filepath.Join(s.root, ".metadata"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(filepath.Join(s.root, ".metadata"), path)
		switch strings.Split(filepath.ToSlash(rel), "/")[0] {
		case "artifacts":
			var v Artifact
			if e := json.Unmarshal(b, &v); e != nil || v.ID == "" {
				return fmt.Errorf("load artifact metadata %s: invalid JSON", path)
			}
			if v.IdempotencyKey != "" {
				key := uploadKey(v.Project, v.JobName, v.IdempotencyKey)
				if _, exists := s.uploads[key]; exists {
					return fmt.Errorf("load artifact metadata %s: duplicate idempotency key", path)
				}
				s.uploads[key] = &v
			}
			s.artifacts[v.ID] = &v
		case "jobs":
			var v JobUploadManifest
			if e := json.Unmarshal(b, &v); e != nil || v.Project == "" || v.JobName == "" {
				return fmt.Errorf("load manifest metadata %s: invalid JSON", path)
			}
			key := manifestKey(v.Project, v.JobName)
			if _, exists := s.manifests[key]; exists {
				return fmt.Errorf("load manifest metadata %s: duplicate completed manifest for job", path)
			}
			s.manifests[key] = &v
		case "logs":
			var v LogStream
			if e := json.Unmarshal(b, &v); e != nil || v.Project == "" || v.JobName == "" {
				return fmt.Errorf("load log metadata %s: invalid JSON", path)
			}
			s.logs[logKey(v.Project, v.JobName, v.Stream)] = &v
		}
		return nil
	})
}

func (s *Store) recover() error {
	for _, l := range s.logs {
		if err := s.recoverLog(l); err != nil {
			return err
		}
	}
	for _, a := range s.artifacts {
		if a.IdempotencyKey == "" || (a.State != Pending && a.State != Uploading) {
			continue
		}
		tmp := filepath.Join(s.root, ".uploads", a.ID+".tmp")
		final := s.artifactPath(a)
		finalOK := verifyFile(final, a.Size, a.SHA256) == nil
		if finalOK {
			if err := s.completeArtifact(a); err != nil {
				return err
			}
			_ = os.Remove(tmp)
			continue
		}
		if _, err := os.Stat(final); err == nil {
			_ = os.MkdirAll(filepath.Join(s.root, ".quarantine"), 0750)
			_ = os.Rename(final, filepath.Join(s.root, ".quarantine", a.ID+".corrupted"))
		}
		_ = os.Remove(tmp)
		if err := s.failArtifact(a, "Corrupted", "committed content is missing or invalid"); err != nil {
			return err
		}
	}
	return nil
}

func verifyFile(path string, size int64, sum string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != sum {
		return errors.New("content mismatch")
	}
	return nil
}

func (s *Store) completeArtifact(a *Artifact) error {
	now := time.Now().UTC()
	a.State, a.UpdatedAt, a.CompletedAt, a.Failure = Completed, now, &now, nil
	return atomicJSON(s.artifactMeta(a.ID), a)
}

func (s *Store) failArtifact(a *Artifact, code, message string) error {
	now := time.Now().UTC()
	f := &FailureInfo{Code: code, Message: message, Retryable: code != "Corrupted", Time: now}
	a.State, a.UpdatedAt, a.Failure = Failed, now, f
	return atomicJSON(s.artifactMeta(a.ID), a)
}

func newID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func atomicJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	tmp := path + ".tmp-" + newID("m")
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if e = enc.Encode(v); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(tmp)
		return e
	}
	if e = os.Rename(tmp, path); e != nil {
		os.Remove(tmp)
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e == nil {
		e = d.Sync()
		d.Close()
	}
	return e
}

func safeRelative(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) || strings.Contains(p, "\\") || strings.ContainsAny(p, "\x00\r\n") {
		return "", errors.New("invalid relative path")
	}
	c := filepath.ToSlash(filepath.Clean(p))
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || len(c) > 1024 {
		return "", errors.New("invalid relative path")
	}
	for _, x := range strings.Split(c, "/") {
		if x == "" || x == "." || x == ".." {
			return "", errors.New("invalid relative path")
		}
	}
	return c, nil
}

func (s *Store) artifactPath(a *Artifact) string {
	return filepath.Join(s.root, filepath.FromSlash(a.StorageKey))
}

func (s *Store) artifactMeta(id string) string {
	return filepath.Join(s.root, ".metadata/artifacts", id+".json")
}

func manifestKey(p, j string) string {
	return p + "\x00" + j
}

func logKey(p, j, stream string) string {
	return p + "\x00" + j + "\x00" + stream
}

func hashText(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func uploadKey(project, job, key string) string {
	return project + "\x00" + job + "\x00" + key
}

func sameUploadMetadata(a *Artifact, m UploadMetadata) bool {
	return a.Category == m.Category && a.FileName == m.FileName &&
		a.RelativePath == m.RelativePath && a.ContentType == m.ContentType && a.Size == m.Size && a.SHA256 == m.SHA256
}

func (s *Store) BeginUpload(project, job, runner, key string, m UploadMetadata, maxJobSize int64) (*Artifact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel, e := safeRelative(m.RelativePath)
	if e != nil {
		return nil, false, e
	}
	m.RelativePath = rel
	ik := uploadKey(project, job, key)
	old := s.uploads[ik]
	if s.manifests[manifestKey(project, job)] != nil {
		if old != nil && sameUploadMetadata(old, m) && old.State == Completed && verifyFile(s.artifactPath(old), old.Size, old.SHA256) == nil {
			return old, true, nil
		}
		return nil, false, errors.New("ManifestAlreadyCompleted")
	}
	if old != nil {
		if !sameUploadMetadata(old, m) {
			return nil, false, fmt.Errorf("IdempotencyConflict")
		}
		if verifyFile(s.artifactPath(old), old.Size, old.SHA256) == nil {
			if err := s.completeArtifact(old); err != nil {
				return nil, false, err
			}
			return old, true, nil
		}
		if old.State == Pending || old.State == Uploading {
			return nil, false, fmt.Errorf("UploadInProgress")
		}
		for _, other := range s.artifacts {
			if other != old && other.Project == project && other.JobName == job && other.RelativePath == rel && (other.State == Pending || other.State == Uploading || other.State == Completed) {
				return nil, false, errors.New("ArtifactPathConflict")
			}
		}
		_ = os.Remove(filepath.Join(s.root, ".uploads", old.ID+".tmp"))
		old.State = Pending
		old.Failure = nil
		old.CompletedAt = nil
		old.UpdatedAt = time.Now().UTC()
		if e := atomicJSON(s.artifactMeta(old.ID), old); e != nil {
			return nil, false, e
		}
		return old, false, nil
	}
	var used int64
	for _, artifact := range s.artifacts {
		if artifact.Project == project && artifact.JobName == job && (artifact.State == Pending || artifact.State == Completed) {
			if artifact.RelativePath == rel {
				return nil, false, errors.New("ArtifactPathConflict")
			}
			used += artifact.Size
		}
	}
	if used+m.Size > maxJobSize {
		return nil, false, errors.New("JobQuotaExceeded")
	}
	now := time.Now().UTC()
	id := newID("art")
	a := &Artifact{SchemaVersion: 1, ID: id, Project: project, JobName: job, RunnerName: runner, Category: m.Category, FileName: m.FileName, RelativePath: rel, ContentType: m.ContentType, Size: m.Size, SHA256: m.SHA256, StorageKey: filepath.ToSlash(filepath.Join("projects", project, "jobs", job, rel)), IdempotencyKey: key, State: Pending, CreatedAt: now, UpdatedAt: now}
	if e = atomicJSON(s.artifactMeta(id), a); e != nil {
		return nil, false, e
	}
	s.artifacts[id] = a
	s.uploads[ik] = a
	return a, false, nil
}

func (s *Store) CompleteUpload(a *Artifact, tmp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifests[manifestKey(a.Project, a.JobName)] != nil {
		return errors.New("ManifestAlreadyCompleted")
	}
	final := s.artifactPath(a)
	if err := os.MkdirAll(filepath.Dir(final), 0750); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	if d, e := os.Open(filepath.Dir(final)); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return s.completeArtifact(a)
}

func (s *Store) UploadCommitted(a *Artifact) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return verifyFile(s.artifactPath(a), a.Size, a.SHA256) == nil
}

func (s *Store) FailUpload(a *Artifact, code, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.failArtifact(a, code, msg)
}

func (s *Store) GetArtifact(id string) (*Artifact, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.artifacts[id]
	if !ok {
		return nil, false
	}
	cp := *a
	return &cp, true
}

func (s *Store) ListArtifacts(project, job string, cat Category) ([]Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Artifact
	for _, a := range s.artifacts {
		if a.State == Completed && a.Project == project && a.JobName == job && (cat == "" || a.Category == cat) {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CompletedAt == nil {
			return true
		}
		if out[j].CompletedAt == nil {
			return false
		}
		if out[i].CompletedAt.Equal(*out[j].CompletedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CompletedAt.Before(*out[j].CompletedAt)
	})
	return out, nil
}
