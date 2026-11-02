package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"artifact-manager/pkg/fileutil"
)

type Store struct {
	root        string
	mu          sync.RWMutex
	logRecovery map[string]struct{}
	subscribers map[string]map[chan LogEvent]struct{}
	sealer      *Sealer
	sealing     map[string]bool
}

type LogEvent struct {
	Sequence int64
	Data     []byte
	Complete *Artifact
}

func NewStore(root string) (*Store, error) {
	s := &Store{
		root:        root,
		logRecovery: make(map[string]struct{}),
		subscribers: make(map[string]map[chan LogEvent]struct{}),
		sealing:     make(map[string]bool),
	}
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0750); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) CleanupTemporary(ttl time.Duration) error {
	root := filepath.Join(s.root, "projects")
	cutoff := time.Now().Add(-ttl)
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if entry.IsDir() {
			if (len(parts) == 2 && parts[1] != "jobs") ||
				(len(parts) == 4 && parts[3] != ".uploads") || len(parts) > 4 {
				return filepath.SkipDir
			}
			return nil
		}
		if len(parts) != 5 || !entry.Type().IsRegular() || filepath.Ext(path) != ".tmp" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			return os.Remove(path)
		}
		return nil
	})
}

func (s *Store) load() error {
	root := filepath.Join(s.root, "projects")
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			// Only traverse Job artifact metadata and the direct logs directory.
			if (len(parts) == 2 && parts[1] != "jobs") ||
				(len(parts) == 4 && parts[3] != ".metadata" && parts[3] != "logs") ||
				(len(parts) == 5 && (parts[3] != ".metadata" || parts[4] != "artifacts")) || len(parts) > 5 {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		isArtifact := len(parts) == 6 && parts[3] == ".metadata" && parts[4] == "artifacts"
		isLog := len(parts) == 5 && parts[3] == "logs" && parts[4] == ".combined.json"
		if !isArtifact && !isLog {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		switch {
		case isArtifact:
			var v Artifact
			if e := json.Unmarshal(b, &v); e != nil || v.ID == "" || v.ID+".json" != parts[5] {
				return fmt.Errorf("load artifact metadata %s: invalid JSON", path)
			}
			return s.recoverArtifact(parts[0], parts[2], &v)
		case isLog:
			var v LogStream
			if e := json.Unmarshal(b, &v); e != nil || v.Project != parts[0] || v.JobName != parts[2] || v.Stream != "combined" {
				return fmt.Errorf("load log metadata %s: invalid JSON", path)
			}
			return s.recoverLog(&v)
		}
		return nil
	})
}

func (s *Store) recoverArtifact(project, job string, a *Artifact) error {
	if a.IdempotencyKey == "" || a.State != Pending {
		return nil
	}
	tmp := s.TemporaryUploadPath(project, job, a.ID)
	final := s.ArtifactPath(a)
	finalOK := fileutil.VerifyFile(final, a.Size, a.SHA256) == nil
	if finalOK {
		if err := s.completeArtifact(project, job, a); err != nil {
			return err
		}
		_ = os.Remove(tmp)
		return nil
	}
	if _, err := os.Stat(final); err == nil {
		quarantine := filepath.Join(s.root, "projects", project, "jobs", job, ".quarantine")
		_ = os.MkdirAll(quarantine, 0750)
		_ = os.Rename(final, filepath.Join(quarantine, a.ID+".corrupted"))
	}
	_ = os.Remove(tmp)
	if err := s.failArtifact(project, job, a, "Corrupted", "committed content is missing or invalid"); err != nil {
		return err
	}
	return nil
}

func (s *Store) completeArtifact(project, job string, a *Artifact) error {
	now := time.Now().UTC()
	a.State, a.UpdatedAt, a.CompletedAt, a.Failure = Completed, now, &now, nil
	return s.persistArtifact(project, job, a)
}

func (s *Store) failArtifact(project, job string, a *Artifact, code, message string) error {
	now := time.Now().UTC()
	f := &FailureInfo{Code: code, Message: message, Retryable: code != "Corrupted", Time: now}
	a.State, a.UpdatedAt, a.Failure = Failed, now, f
	return s.persistArtifact(project, job, a)
}

func (s *Store) ArtifactPath(a *Artifact) string {
	return filepath.Join(s.root, filepath.FromSlash(a.StorageKey))
}

// TemporaryUploadPath locates an upload body within its Job directory.
func (s *Store) TemporaryUploadPath(project, job, id string) string {
	return filepath.Join(s.root, "projects", project, "jobs", job, ".uploads", id+".tmp")
}

func (s *Store) jobMetadataDir(project, job string) string {
	return filepath.Join(s.root, "projects", project, "jobs", job, ".metadata")
}

func (s *Store) artifactMeta(project, job, id string) string {
	return filepath.Join(s.jobMetadataDir(project, job), "artifacts", id+".json")
}

func (s *Store) persistArtifact(project, job string, a *Artifact) error {
	return fileutil.AtomicJSON(s.artifactMeta(project, job, a.ID), a)
}

// readArtifact and readJobArtifacts are called with the store lock held, or during startup.
func (s *Store) readArtifact(project, job, id string) (*Artifact, error) {
	for _, part := range []string{project, job, id} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\`+"\x00") {
			return nil, errors.New("invalid artifact location")
		}
	}
	b, err := os.ReadFile(s.artifactMeta(project, job, id))
	if err != nil {
		return nil, err
	}
	var a Artifact
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	if a.ID != id {
		return nil, errors.New("artifact ID mismatch")
	}
	return &a, nil
}

func (s *Store) readJobArtifacts(project, job string) ([]Artifact, error) {
	dir := filepath.Dir(s.artifactMeta(project, job, ""))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var artifacts []Artifact
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		a, err := s.readArtifact(project, job, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, *a)
	}
	return artifacts, nil
}

func logKey(p, j, stream string) string {
	return p + "\x00" + j + "\x00" + stream
}

func hashText(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func sameUploadMetadata(a *Artifact, m UploadMetadata) bool {
	return a.Category == m.Category && a.FileName == m.FileName &&
		a.RelativePath == m.RelativePath && a.ContentType == m.ContentType && a.Size == m.Size && a.SHA256 == m.SHA256
}

func (s *Store) BeginUpload(project, job, key string, m UploadMetadata, maxJobSize int64) (*Artifact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel, e := fileutil.SafeRelative(m.RelativePath)
	if e != nil {
		return nil, false, e
	}
	if rel == ".metadata" || strings.HasPrefix(rel, ".metadata/") {
		return nil, false, errors.New("reserved metadata path")
	}
	if rel == ".uploads" || strings.HasPrefix(rel, ".uploads/") {
		return nil, false, errors.New("reserved upload path")
	}
	if rel == ".quarantine" || strings.HasPrefix(rel, ".quarantine/") {
		return nil, false, errors.New("reserved quarantine path")
	}
	if rel == ".signed" || strings.HasPrefix(rel, ".signed/") ||
		rel == ".signing" || strings.HasPrefix(rel, ".signing/") {
		return nil, false, errors.New("reserved signing path")
	}
	if rel == "logs" {
		return nil, false, errors.New("reserved log path")
	}
	for _, path := range []string{"logs/combined.log", "logs/container.log", "logs/.combined.json", "logs/.combined.index.jsonl"} {
		if rel == path || strings.HasPrefix(rel, path+"/") || strings.HasPrefix(rel, path+".tmp-") {
			return nil, false, errors.New("reserved log path")
		}
	}
	m.RelativePath = rel
	artifacts, err := s.readJobArtifacts(project, job)
	if err != nil {
		return nil, false, err
	}
	var old *Artifact
	for i := range artifacts {
		if artifacts[i].IdempotencyKey == key && key != "" {
			if old != nil {
				return nil, false, errors.New("duplicate idempotency key")
			}
			old = &artifacts[i]
		}
	}
	manifest, err := s.readManifest(project, job)
	if err != nil && !os.IsNotExist(err) {
		return nil, false, err
	}
	seal, err := s.readSeal(project, job)
	if err != nil && !os.IsNotExist(err) {
		return nil, false, err
	}
	if manifest != nil || seal != nil {
		if old != nil && sameUploadMetadata(old, m) && old.State == Completed && fileutil.VerifyFile(s.ArtifactPath(old), old.Size, old.SHA256) == nil {
			return old, true, nil
		}
		if manifest != nil {
			return nil, false, errors.New("ManifestAlreadyCompleted")
		}
		return nil, false, &SealError{"ManifestSealing", 409, false}
	}
	if err := os.MkdirAll(filepath.Dir(s.TemporaryUploadPath(project, job, "")), 0750); err != nil {
		return nil, false, err
	}
	if old != nil {
		if !sameUploadMetadata(old, m) {
			return nil, false, fmt.Errorf("IdempotencyConflict")
		}
		if fileutil.VerifyFile(s.ArtifactPath(old), old.Size, old.SHA256) == nil {
			if err := s.completeArtifact(project, job, old); err != nil {
				return nil, false, err
			}
			return old, true, nil
		}
		if old.State == Pending {
			return nil, false, fmt.Errorf("UploadInProgress")
		}
		for _, other := range artifacts {
			if other.ID != old.ID && other.RelativePath == rel && (other.State == Pending || other.State == Completed) {
				return nil, false, errors.New("ArtifactPathConflict")
			}
		}
		_ = os.Remove(s.TemporaryUploadPath(project, job, old.ID))
		old.State = Pending
		old.Failure = nil
		old.CompletedAt = nil
		old.UpdatedAt = time.Now().UTC()
		if e := s.persistArtifact(project, job, old); e != nil {
			return nil, false, e
		}
		return old, false, nil
	}
	var used int64
	for _, artifact := range artifacts {
		if artifact.State == Pending || artifact.State == Completed {
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
	id := fileutil.NewID("art")
	a := &Artifact{
		SchemaVersion:  1,
		ID:             id,
		Category:       m.Category,
		FileName:       m.FileName,
		RelativePath:   rel,
		ContentType:    m.ContentType,
		Size:           m.Size,
		SHA256:         m.SHA256,
		StorageKey:     filepath.ToSlash(filepath.Join("projects", project, "jobs", job, rel)),
		IdempotencyKey: key,
		State:          Pending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if e = s.persistArtifact(project, job, a); e != nil {
		return nil, false, e
	}
	return a, false, nil
}

func (s *Store) CompleteUpload(project, job string, a *Artifact, tmp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	manifest, err := s.readManifest(project, job)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if manifest != nil {
		return errors.New("ManifestAlreadyCompleted")
	}
	seal, err := s.readSeal(project, job)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if seal != nil {
		return &SealError{"ManifestSealing", 409, false}
	}
	final := s.ArtifactPath(a)
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
	return s.completeArtifact(project, job, a)
}

func (s *Store) UploadCommitted(a *Artifact) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fileutil.VerifyFile(s.ArtifactPath(a), a.Size, a.SHA256) == nil
}

func (s *Store) FailUpload(project, job string, a *Artifact, code, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.failArtifact(project, job, a, code, msg)
}

// GetJobArtifact returns an artifact only when it belongs to the requested Job.
func (s *Store) GetJobArtifact(project, job, id string) (*Artifact, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, err := s.readArtifact(project, job, id)
	if err != nil {
		return nil, false
	}
	manifest, err := s.readManifest(project, job)
	if err != nil && !os.IsNotExist(err) {
		return nil, false
	}
	if err := projectArtifact(a, project, job, manifest); err != nil {
		return nil, false
	}
	return a, true
}

func (s *Store) ListArtifacts(project, job string, cat Category) ([]Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Artifact
	artifacts, err := s.readJobArtifacts(project, job)
	if err != nil {
		return nil, err
	}
	manifest, err := s.readManifest(project, job)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, a := range artifacts {
		if err := projectArtifact(&a, project, job, manifest); err != nil {
			return nil, err
		}
		if a.State == Completed && (cat == "" || a.Category == cat) {
			out = append(out, a)
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

// projectArtifact selects immutable sealed content without mutating original upload metadata.
func projectArtifact(a *Artifact, project, job string, manifest *JobUploadManifest) error {
	if manifest == nil {
		return nil
	}
	if manifest.State != ManifestCompleted || manifest.Digest != ManifestDigest(manifest.Files) {
		return ErrInvalidManifest
	}
	for _, f := range manifest.Files {
		if f.ArtifactID != a.ID {
			continue
		}
		if f.RelativePath != a.RelativePath || f.Category != a.Category {
			return ErrInvalidManifest
		}
		a.Size, a.SHA256 = f.Size, f.SHA256
		if manifest.SigningFingerprint != "" && isRPM(f) {
			a.StorageKey = signedStorageKey(project, job, a.ID)
		}
		return nil
	}
	return nil
}
