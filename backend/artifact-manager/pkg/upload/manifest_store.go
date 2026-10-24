package upload

import (
	"context"
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

var ErrInvalidManifest = errors.New("invalid manifest metadata")

func ManifestDigest(files []ManifestFile) string {
	files = append([]ManifestFile(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
	var b strings.Builder
	b.WriteString("artifact-manifest-v1\n")
	for _, f := range files {
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%d\x00%s\x00%t\n", f.RelativePath, f.ArtifactID, f.Category, f.Size, f.SHA256, f.Required)
	}
	return "sha256:" + hashText(b.String())
}

type sealRecord struct {
	Project            string                  `json:"project"`
	JobName            string                  `json:"jobName"`
	RunnerName         string                  `json:"runnerName"`
	Files              []ManifestFile          `json:"files"`
	UploadDigest       string                  `json:"uploadDigest"`
	SigningFingerprint string                  `json:"signingFingerprint,omitempty"`
	Done               map[string]ManifestFile `json:"done,omitempty"`
	CreatedAt          time.Time               `json:"createdAt"`
}

func (s *Store) sealPath(project, job string) string {
	return filepath.Join(s.root, ".metadata/jobs", project, job, "seal.json")
}

func (s *Store) readSeal(project, job string) (*sealRecord, error) {
	if err := validateScope(project, job); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.sealPath(project, job))
	if err != nil {
		return nil, err
	}
	var record sealRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("%w: seal record", ErrInvalidManifest)
	}
	if record.Project != project || record.JobName != job || record.UploadDigest != ManifestDigest(record.Files) {
		return nil, ErrInvalidManifest
	}
	if record.Done == nil {
		record.Done = make(map[string]ManifestFile)
	}
	return &record, nil
}

// CompleteManifest freezes original uploads and publishes only the fully prepared file set.
func (s *Store) CompleteManifest(ctx context.Context, project, job, runner string, r CompleteManifestRequest) (*JobUploadManifest, error) {
	if err := validateScope(project, job); err != nil {
		return nil, err
	}
	r.Files = append([]ManifestFile(nil), r.Files...)
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].RelativePath < r.Files[j].RelativePath })
	digest := ManifestDigest(r.Files)
	s.mu.Lock()
	old, err := s.readManifest(project, job)
	if err != nil && !os.IsNotExist(err) {
		s.mu.Unlock()
		return nil, err
	}
	if old != nil {
		s.mu.Unlock()
		if old.State == ManifestCompleted && old.UploadDigest == digest {
			return old, nil
		}
		return nil, &SealError{"ManifestConflict", 409, false}
	}
	record, err := s.readSeal(project, job)
	if err != nil && !os.IsNotExist(err) {
		s.mu.Unlock()
		return nil, err
	}
	if record != nil && record.UploadDigest != digest {
		s.mu.Unlock()
		return nil, &SealError{"ManifestConflict", 409, false}
	}
	key := logKey(project, job, "")
	if s.sealing[key] {
		s.mu.Unlock()
		return nil, &SealError{"ManifestInProgress", 409, true}
	}
	artifacts, err := s.validateSealInput(project, job, r.Files)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	fingerprint := s.sealer.fingerprint()
	if record == nil {
		record = &sealRecord{
			Project:            project,
			JobName:            job,
			RunnerName:         runner,
			Files:              r.Files,
			UploadDigest:       digest,
			SigningFingerprint: fingerprint,
			CreatedAt:          time.Now().UTC(),
			Done:               make(map[string]ManifestFile),
		}
		if err := fileutil.AtomicJSON(s.sealPath(project, job), record); err != nil {
			s.mu.Unlock()
			return nil, &SealError{"StorageError", 503, true}
		}
	}
	if record.SigningFingerprint != fingerprint {
		s.mu.Unlock()
		return nil, &SealError{"SigningConfigurationChanged", 503, true}
	}
	s.sealing[key] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.sealing, key); s.mu.Unlock() }()

	final, err := s.prepareFiles(ctx, project, job, record, artifacts)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return nil, &SealError{"SigningBackendUnavailable", 503, true}
	}
	current, err := s.readSeal(project, job)
	if err != nil {
		return nil, err
	}
	if current.UploadDigest != digest || current.SigningFingerprint != fingerprint {
		return nil, &SealError{"ManifestConflict", 409, false}
	}
	now := time.Now().UTC()
	m := &JobUploadManifest{
		SchemaVersion:      1,
		Project:            project,
		JobName:            job,
		RunnerName:         record.RunnerName,
		Files:              final,
		Digest:             ManifestDigest(final),
		UploadDigest:       digest,
		SigningFingerprint: fingerprint,
		State:              ManifestCompleted,
		CreatedAt:          record.CreatedAt,
		UpdatedAt:          now,
		CompletedAt:        &now,
	}
	path := filepath.Join(s.root, ".metadata/jobs", project, job, "manifest.json")
	if err := fileutil.AtomicJSON(path, m); err != nil {
		return nil, &SealError{"StorageError", 503, true}
	}
	_ = os.Remove(s.sealPath(project, job))
	return m, nil
}

// validateSealInput runs under the Store lock and checks original upload receipts, never signed projections.
func (s *Store) validateSealInput(project, job string, files []ManifestFile) ([]Artifact, error) {
	if len(files) == 0 {
		return nil, errors.New("invalid manifest")
	}
	all, err := s.readJobArtifacts(project, job)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.State == Pending {
			return nil, &SealError{"ManifestInProgress", 409, true}
		}
	}
	seenP, seenA := map[string]bool{}, map[string]bool{}
	artifacts := make([]Artifact, 0, len(files))
	for _, f := range files {
		if seenP[f.RelativePath] || seenA[f.ArtifactID] {
			return nil, errors.New("duplicate manifest entry")
		}
		seenP[f.RelativePath], seenA[f.ArtifactID] = true, true
		a, err := s.readArtifact(project, job, f.ArtifactID)
		if os.IsNotExist(err) {
			return nil, &SealError{"ArtifactMismatch", 422, false}
		}
		if err != nil {
			return nil, err
		}
		if a.State != Completed || a.RelativePath != f.RelativePath || a.Category != f.Category || a.Size != f.Size || a.SHA256 != f.SHA256 {
			return nil, &SealError{"ArtifactMismatch", 422, false}
		}
		artifacts = append(artifacts, *a)
	}
	return artifacts, nil
}

func (s *Store) prepareFiles(ctx context.Context, project, job string, record *sealRecord, artifacts []Artifact) ([]ManifestFile, error) {
	final := append([]ManifestFile(nil), record.Files...)
	work := filepath.Join(s.root, ".signing", project, job)
	// No previous worker for this Job remains when ownership is acquired.
	if err := os.RemoveAll(work); err != nil {
		return nil, &SealError{"StorageError", 503, true}
	}
	defer os.RemoveAll(work)
	var tasks []func(context.Context) error
	var progress sync.Mutex
	for i, f := range record.Files {
		if record.SigningFingerprint == "" || !isRPM(f) {
			continue
		}
		saved, hasSaved := record.Done[f.ArtifactID]
		tasks = append(tasks, func(ctx context.Context) error {
			if hasSaved {
				path := filepath.Join(s.root, signedStorageKey(project, job, f.ArtifactID))
				if err := verifySealFile(ctx, path, saved, "SignedPackageVerificationFailed"); err != nil {
					return err
				}
			}
			prepared, err := s.sealer.prepareRPM(ctx, s.root, project, job, artifacts[i], f)
			if err != nil {
				return err
			}
			progress.Lock()
			defer progress.Unlock()
			if hasSaved && saved != prepared {
				return &SealError{"SignedPackageVerificationFailed", 422, false}
			}
			record.Done[f.ArtifactID] = prepared
			final[i] = prepared
			if err := fileutil.AtomicJSON(s.sealPath(project, job), record); err != nil {
				return &SealError{"StorageError", 503, true}
			}
			return nil
		})
	}
	if len(tasks) > 0 {
		if err := s.sealer.run(ctx, tasks); err != nil {
			return nil, err
		}
	}
	var total int64
	for i, f := range final {
		path := s.ArtifactPath(&artifacts[i])
		if record.SigningFingerprint != "" && isRPM(f) {
			path = filepath.Join(s.root, signedStorageKey(project, job, f.ArtifactID))
		}
		if err := verifySealFile(ctx, path, f, "ArtifactMismatch"); err != nil {
			return nil, err
		}
		if f.Category == CategoryArtifact && s.sealer != nil {
			if f.Size > s.sealer.config.MaxJobSize-total {
				return nil, &SealError{"ArtifactLimitExceeded", 413, false}
			}
			total += f.Size
		}
	}
	return final, nil
}

func (s *Store) GetManifest(p, j string) (*JobUploadManifest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readManifest(p, j)
}

// readManifest is called with the store lock held; only a missing file means the Job is unsealed.
func (s *Store) readManifest(project, job string) (*JobUploadManifest, error) {
	for _, part := range []string{project, job} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\x00") {
			return nil, ErrInvalidManifest
		}
	}
	path := filepath.Join(s.root, ".metadata/jobs", project, job, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest JobUploadManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if manifest.Project != project || manifest.JobName != job {
		return nil, ErrInvalidManifest
	}
	return &manifest, nil
}
