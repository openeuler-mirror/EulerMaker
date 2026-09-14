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
	idempotency map[string]*IdempotencyRecord
	subscribers map[string]map[chan logEvent]struct{}
}
type logEvent struct {
	Sequence int64
	Data     []byte
	Complete *Artifact
}

func NewStore(root string) (*Store, error) {
	s := &Store{root: root, artifacts: map[string]*Artifact{}, manifests: map[string]*JobUploadManifest{}, logs: map[string]*LogStream{}, idempotency: map[string]*IdempotencyRecord{}, subscribers: map[string]map[chan logEvent]struct{}{}}
	for _, d := range []string{".uploads", ".metadata/artifacts", ".metadata/jobs", ".metadata/idempotency", ".metadata/logs", ".logs", "projects", ".quarantine"} {
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
			s.artifacts[v.ID] = &v
		case "jobs":
			var v JobUploadManifest
			if e := json.Unmarshal(b, &v); e != nil || v.Project == "" || v.JobUID == "" {
				return fmt.Errorf("load manifest metadata %s: invalid JSON", path)
			}
			key := manifestKey(v.Project, v.JobName, v.JobUID)
			if _, exists := s.manifests[key]; exists {
				return fmt.Errorf("load manifest metadata %s: duplicate completed manifest for job", path)
			}
			s.manifests[key] = &v
		case "logs":
			var v LogStream
			if e := json.Unmarshal(b, &v); e != nil || v.Project == "" || v.JobUID == "" {
				return fmt.Errorf("load log metadata %s: invalid JSON", path)
			}
			s.logs[logKey(v.Project, v.JobName, v.JobUID, v.Stream)] = &v
		case "idempotency":
			var v IdempotencyRecord
			if e := json.Unmarshal(b, &v); e != nil || v.Scope == "" || v.Key == "" {
				return fmt.Errorf("load idempotency metadata %s: invalid JSON", path)
			}
			s.idempotency[v.Scope+"\x00"+v.Key] = &v
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
	for _, ir := range s.idempotency {
		a := s.artifacts[ir.ArtifactID]
		if a == nil {
			if ir.State == IdempotencyProcessing {
				s.failIdempotency(ir, "ArtifactMissing", "artifact metadata is missing")
			}
			continue
		}
		tmp := filepath.Join(s.root, ".uploads", a.ID+".tmp")
		final := s.artifactPath(a)
		finalOK := verifyFile(final, a.Size, a.SHA256) == nil
		if a.State == Completed && finalOK {
			if ir.State != IdempotencyCompleted {
				s.completeRecords(a, ir)
			}
			continue
		}
		if finalOK {
			s.completeRecords(a, ir)
			_ = os.Remove(tmp)
			continue
		}
		if _, err := os.Stat(final); err == nil {
			_ = os.MkdirAll(filepath.Join(s.root, ".quarantine"), 0750)
			_ = os.Rename(final, filepath.Join(s.root, ".quarantine", a.ID+".corrupted"))
		}
		_ = os.Remove(tmp)
		s.failRecords(a, ir, "Corrupted", "committed content is missing or invalid")
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

func (s *Store) completeRecords(a *Artifact, ir *IdempotencyRecord) {
	now := time.Now().UTC()
	a.State, a.UpdatedAt, a.CompletedAt, a.Failure = Completed, now, &now, nil
	ir.State, ir.UpdatedAt, ir.CompletedAt, ir.Failure = IdempotencyCompleted, now, &now, nil
	_ = atomicJSON(s.artifactMeta(a.ID), a)
	_ = atomicJSON(s.idemPath(ir.Scope, ir.Key), ir)
}

func (s *Store) failIdempotency(ir *IdempotencyRecord, code, message string) {
	now := time.Now().UTC()
	ir.State, ir.UpdatedAt = IdempotencyFailed, now
	ir.Failure = &FailureInfo{Code: code, Message: message, Retryable: true, Time: now}
	_ = atomicJSON(s.idemPath(ir.Scope, ir.Key), ir)
}

func (s *Store) failRecords(a *Artifact, ir *IdempotencyRecord, code, message string) {
	now := time.Now().UTC()
	f := &FailureInfo{Code: code, Message: message, Retryable: code != "Corrupted", Time: now}
	a.State, a.UpdatedAt, a.Failure = Failed, now, f
	ir.State, ir.UpdatedAt, ir.Failure = IdempotencyFailed, now, f
	_ = atomicJSON(s.artifactMeta(a.ID), a)
	_ = atomicJSON(s.idemPath(ir.Scope, ir.Key), ir)
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
func manifestKey(p, j, u string) string    { return p + "\x00" + j + "\x00" + u }
func logKey(p, j, u, stream string) string { return p + "\x00" + j + "\x00" + u + "\x00" + stream }
func hashText(v string) string             { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func (s *Store) idemPath(scope, key string) string {
	return filepath.Join(s.root, ".metadata/idempotency", hashText(scope), hashText(key)+".json")
}

func metadataDigest(m UploadMetadata) string {
	return "sha256:" + hashText(fmt.Sprintf("artifact-upload-v1\n%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s\n", m.JobUID, m.Category, m.Name, m.FileName, m.RelativePath, m.ContentType, m.Size, m.SHA256))
}
func (s *Store) BeginUpload(project, job, runner, key string, m UploadMetadata, maxJobSize int64) (*Artifact, *IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel, e := safeRelative(m.RelativePath)
	if e != nil {
		return nil, nil, false, e
	}
	m.RelativePath = rel
	scope := "artifact-upload/" + project + "/" + m.JobUID
	ik := scope + "\x00" + key
	digest := metadataDigest(m)
	old := s.idempotency[ik]
	if s.manifests[manifestKey(project, job, m.JobUID)] != nil {
		if old != nil && old.RequestDigest == digest {
			a := s.artifacts[old.ArtifactID]
			if old.State == IdempotencyCompleted && a != nil && a.State == Completed && verifyFile(s.artifactPath(a), a.Size, a.SHA256) == nil {
				return a, old, true, nil
			}
		}
		return nil, nil, false, errors.New("ManifestAlreadyCompleted")
	}
	if old != nil {
		if old.RequestDigest != digest {
			return nil, nil, false, fmt.Errorf("IdempotencyConflict")
		}
		a := s.artifacts[old.ArtifactID]
		if a != nil && verifyFile(s.artifactPath(a), a.Size, a.SHA256) == nil {
			s.completeRecords(a, old)
			return a, old, true, nil
		}
		if old.State == IdempotencyCompleted && a != nil && a.State == Completed {
			return a, old, true, nil
		}
		if old.State == IdempotencyProcessing {
			return nil, nil, false, fmt.Errorf("UploadInProgress")
		}
		if a != nil {
			_ = os.Remove(filepath.Join(s.root, ".uploads", a.ID+".tmp"))
			a.State = Pending
			a.Failure = nil
			a.UpdatedAt = time.Now().UTC()
			if e := atomicJSON(s.artifactMeta(a.ID), a); e != nil {
				return nil, nil, false, e
			}
			old.State = IdempotencyProcessing
			old.Failure = nil
			old.UpdatedAt = a.UpdatedAt
			if e := atomicJSON(s.idemPath(scope, key), old); e != nil {
				return nil, nil, false, e
			}
			return a, old, false, nil
		}
	}
	var used int64
	for _, artifact := range s.artifacts {
		if artifact.Project == project && artifact.JobUID == m.JobUID && (artifact.State == Pending || artifact.State == Completed) {
			if artifact.RelativePath == rel {
				return nil, nil, false, errors.New("ArtifactPathConflict")
			}
			used += artifact.Size
		}
	}
	if used+m.Size > maxJobSize {
		return nil, nil, false, errors.New("JobQuotaExceeded")
	}
	now := time.Now().UTC()
	id := newID("art")
	a := &Artifact{SchemaVersion: 1, ID: id, Project: project, JobName: job, JobUID: m.JobUID, RunnerName: runner, Category: m.Category, Name: m.Name, FileName: m.FileName, RelativePath: rel, ContentType: m.ContentType, Size: m.Size, SHA256: m.SHA256, StorageKey: filepath.ToSlash(filepath.Join("projects", project, "jobs", m.JobUID, rel)), State: Pending, CreatedAt: now, UpdatedAt: now}
	ir := &IdempotencyRecord{SchemaVersion: 1, Scope: scope, Key: key, RequestDigest: digest, ArtifactID: id, State: IdempotencyProcessing, CreatedAt: now, UpdatedAt: now}
	if e = atomicJSON(s.idemPath(scope, key), ir); e != nil {
		return nil, nil, false, e
	}
	if e = atomicJSON(s.artifactMeta(id), a); e != nil {
		return nil, nil, false, e
	}
	s.artifacts[id] = a
	s.idempotency[ik] = ir
	return a, ir, false, nil
}
func (s *Store) CompleteUpload(a *Artifact, ir *IdempotencyRecord, tmp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifests[manifestKey(a.Project, a.JobName, a.JobUID)] != nil {
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
	now := time.Now().UTC()
	a.State = Completed
	a.UpdatedAt = now
	a.CompletedAt = &now
	if err := atomicJSON(s.artifactMeta(a.ID), a); err != nil {
		return err
	}
	ir.State = IdempotencyCompleted
	ir.UpdatedAt = now
	ir.CompletedAt = &now
	return atomicJSON(s.idemPath(ir.Scope, ir.Key), ir)
}
func (s *Store) UploadCommitted(a *Artifact) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return verifyFile(s.artifactPath(a), a.Size, a.SHA256) == nil
}
func (s *Store) FailUpload(a *Artifact, ir *IdempotencyRecord, code, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	f := &FailureInfo{Code: code, Message: msg, Retryable: true, Time: now}
	a.State = Failed
	a.Failure = f
	a.UpdatedAt = now
	ir.State = IdempotencyFailed
	ir.Failure = f
	_ = atomicJSON(s.artifactMeta(a.ID), a)
	_ = atomicJSON(s.idemPath(ir.Scope, ir.Key), ir)
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
func (s *Store) ListArtifacts(project, job, uid string, cat Category) ([]Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Artifact
	for _, a := range s.artifacts {
		if a.State == Completed && a.Project == project && a.JobName == job && a.JobUID == uid && (cat == "" || a.Category == cat) {
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

func manifestDigest(files []ManifestFile) string {
	var b strings.Builder
	b.WriteString("artifact-manifest-v1\n")
	for _, f := range files {
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%d\x00%s\x00%t\n", f.RelativePath, f.ArtifactID, f.Category, f.Size, f.SHA256, f.Required)
	}
	return "sha256:" + hashText(b.String())
}
func (s *Store) CompleteManifest(project, job, runner string, r CompleteManifestRequest) (*JobUploadManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(r.Files) == 0 {
		return nil, errors.New("invalid manifest")
	}
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].RelativePath < r.Files[j].RelativePath })
	seenP := map[string]bool{}
	seenA := map[string]bool{}
	for _, f := range r.Files {
		if seenP[f.RelativePath] || seenA[f.ArtifactID] {
			return nil, errors.New("duplicate manifest entry")
		}
		seenP[f.RelativePath] = true
		seenA[f.ArtifactID] = true
		a := s.artifacts[f.ArtifactID]
		if a == nil || a.State != Completed || a.Project != project || a.JobName != job || a.JobUID != r.JobUID || a.RelativePath != f.RelativePath || a.Category != f.Category || a.Size != f.Size || a.SHA256 != f.SHA256 {
			return nil, errors.New("artifact mismatch")
		}
	}
	k := manifestKey(project, job, r.JobUID)
	digest := manifestDigest(r.Files)
	if old := s.manifests[k]; old != nil {
		if old.Digest == digest && old.State == ManifestCompleted {
			return old, nil
		}
		return nil, errors.New("manifest conflict")
	}
	now := time.Now().UTC()
	m := &JobUploadManifest{SchemaVersion: 1, Project: project, JobName: job, JobUID: r.JobUID, RunnerName: runner, Files: r.Files, Digest: digest, State: ManifestCompleted, CreatedAt: now, UpdatedAt: now, CompletedAt: &now}
	path := filepath.Join(s.root, ".metadata/jobs", project, r.JobUID, "manifest.json")
	if err := atomicJSON(path, m); err != nil {
		return nil, err
	}
	s.manifests[k] = m
	return m, nil
}
func (s *Store) GetManifest(p, j, u string) (*JobUploadManifest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.manifests[manifestKey(p, j, u)]
	if !ok {
		return nil, false
	}
	return m, true
}
