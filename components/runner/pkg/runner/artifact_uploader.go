package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const artifactGeneration int64 = 1

type ArtifactRemote interface {
	LogStatus(context.Context, string, string, string) (LogStatus, error)
	UploadArtifact(context.Context, string, string, string, string, UploadArtifactInput) (ArtifactRecord, error)
	CompleteManifest(context.Context, string, string, string, CompleteManifestInput) (CompletedManifest, error)
	GetManifest(context.Context, string, string, string, int64) (CompletedManifest, error)
}

type ArtifactProcessor struct {
	Remote          ArtifactRemote
	RootDir         string
	MaxFileSize     int64
	MaxJobSize      int64
	MaxFiles        int
	Concurrency     int
	RetryMaxBackoff time.Duration
}

type artifactCandidate struct {
	Path         string
	RelativePath string
	FileName     string
	ContentType  string
	Size         int64
	SHA256       string
}

type artifactReceipt struct {
	SchemaVersion  int            `json:"schemaVersion"`
	IdempotencyKey string         `json:"idempotencyKey"`
	Artifact       ArtifactRecord `json:"artifact"`
}

func (p *ArtifactProcessor) Finalize(ctx context.Context, job JobResource, resultDir string, includeOrdinary bool) (CompletedManifest, error) {
	if p.Remote == nil {
		return CompletedManifest{}, fmt.Errorf("artifact remote is required")
	}
	if err := validateJobIdentity(job); err != nil {
		return CompletedManifest{}, err
	}

	logStatus, err := p.Remote.LogStatus(ctx, job.Metadata.Namespace, job.Metadata.Name, job.Metadata.UID)
	if err != nil {
		return CompletedManifest{}, fmt.Errorf("get completed log: %w", err)
	}
	if logStatus.State != "Completed" || logStatus.ArtifactID == "" || logStatus.FinalSize == nil || logStatus.FinalSHA256 == "" {
		return CompletedManifest{}, fmt.Errorf("container log is not completed")
	}
	files := []ManifestFile{{
		ArtifactID: logStatus.ArtifactID, RelativePath: "logs/container.log", Category: "log",
		Size: *logStatus.FinalSize, SHA256: logStatus.FinalSHA256, Required: true,
	}}

	if includeOrdinary {
		candidates, err := p.scan(resultDir)
		if err != nil {
			return CompletedManifest{}, err
		}
		artifacts, err := p.uploadAll(ctx, job, candidates)
		if err != nil {
			return CompletedManifest{}, err
		}
		for _, artifact := range artifacts {
			files = append(files, ManifestFile{
				ArtifactID: artifact.ID, RelativePath: artifact.RelativePath, Category: "artifact",
				Size: artifact.Size, SHA256: artifact.SHA256, Required: true,
			})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
	input := CompleteManifestInput{JobUID: job.Metadata.UID, Generation: artifactGeneration, Files: files}
	key := job.Metadata.UID + "-manifest-1"
	manifest, err := p.completeManifestWithRetry(ctx, job, key, input)
	if err != nil {
		return CompletedManifest{}, err
	}
	if manifest.State != "Completed" || manifest.Generation != artifactGeneration || manifest.ArtifactCount != len(files) || manifest.Digest == "" {
		return CompletedManifest{}, fmt.Errorf("artifact manifest completion response does not match request")
	}
	return manifest, nil
}

func (p *ArtifactProcessor) scan(root string) ([]artifactCandidate, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect result root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("result root is not a directory")
	}
	maxFiles := p.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 10000
	}
	maxFileSize := p.MaxFileSize
	if maxFileSize <= 0 {
		maxFileSize = 25 << 30
	}
	maxJobSize := p.MaxJobSize
	if maxJobSize <= 0 {
		maxJobSize = 100 << 30
	}

	var candidates []artifactCandidate
	var total int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported result file type: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if err := validateArtifactRelativePath(relative); err != nil {
			return err
		}
		if relative == "logs/container.log" {
			return fmt.Errorf("result path conflicts with container log: %s", relative)
		}
		if len(candidates) >= maxFiles {
			return fmt.Errorf("artifact file count exceeds %d", maxFiles)
		}
		if info.Size() > maxFileSize {
			return fmt.Errorf("artifact %s exceeds file size limit %d", relative, maxFileSize)
		}
		if info.Size() > maxJobSize-total {
			return fmt.Errorf("artifact total size exceeds %d", maxJobSize)
		}
		sum, size, err := hashFile(path)
		if err != nil {
			return fmt.Errorf("hash artifact %s: %w", relative, err)
		}
		contentType := "application/octet-stream"
		if strings.EqualFold(filepath.Ext(relative), ".rpm") {
			contentType = "application/x-rpm"
		}
		candidates = append(candidates, artifactCandidate{
			Path: path, RelativePath: relative, FileName: filepath.Base(relative), ContentType: contentType,
			Size: size, SHA256: sum,
		})
		total += size
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan result artifacts: %w", err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].RelativePath < candidates[j].RelativePath })
	return candidates, nil
}

func validateArtifactRelativePath(path string) error {
	if path == "" || path == "." || strings.HasPrefix(path, "/") || !utf8.ValidString(path) || strings.Contains(path, "\\") {
		return fmt.Errorf("invalid artifact relative path %q", path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("invalid artifact relative path %q", path)
		}
		for _, r := range segment {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("invalid artifact relative path %q", path)
			}
		}
	}
	return nil
}

func (p *ArtifactProcessor) uploadAll(ctx context.Context, job JobResource, candidates []artifactCandidate) ([]ArtifactRecord, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	concurrency := p.Concurrency
	if concurrency <= 0 || concurrency > len(candidates) {
		concurrency = len(candidates)
	}
	type uploadResult struct {
		index    int
		artifact ArtifactRecord
		err      error
	}
	jobs := make(chan int)
	results := make(chan uploadResult, len(candidates))
	var workers sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				artifact, err := p.uploadOne(ctx, job, candidates[index])
				results <- uploadResult{index: index, artifact: artifact, err: err}
			}
		}()
	}
	go func() {
		for index := range candidates {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()

	artifacts := make([]ArtifactRecord, len(candidates))
	var firstErr error
	for result := range results {
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
		artifacts[result.index] = result.artifact
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return artifacts, nil
}

func (p *ArtifactProcessor) uploadOne(ctx context.Context, job JobResource, candidate artifactCandidate) (ArtifactRecord, error) {
	keySum := sha256.Sum256([]byte(candidate.RelativePath))
	key := job.Metadata.UID + "-artifact-" + hex.EncodeToString(keySum[:])
	if receipt, ok := p.readReceipt(job, candidate, key); ok {
		return receipt.Artifact, nil
	}
	input := UploadArtifactInput{
		JobUID: job.Metadata.UID, Category: "artifact", FileName: candidate.FileName,
		RelativePath: candidate.RelativePath, ContentType: candidate.ContentType,
		Size: candidate.Size, SHA256: candidate.SHA256,
	}
	artifact, err := p.uploadWithRetry(ctx, job, key, candidate, input)
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("upload artifact %s: %w", candidate.RelativePath, err)
	}
	if err := validateUploadedArtifact(job, candidate, artifact); err != nil {
		return ArtifactRecord{}, err
	}
	receipt := artifactReceipt{SchemaVersion: 1, IdempotencyKey: key, Artifact: artifact}
	if err := p.writeReceipt(job, candidate.RelativePath, receipt); err != nil {
		return ArtifactRecord{}, fmt.Errorf("persist artifact receipt %s: %w", candidate.RelativePath, err)
	}
	return artifact, nil
}

func (p *ArtifactProcessor) uploadWithRetry(ctx context.Context, job JobResource, key string, candidate artifactCandidate, input UploadArtifactInput) (ArtifactRecord, error) {
	delay := time.Second
	for {
		artifact, uploadErr := p.Remote.UploadArtifact(ctx, job.Metadata.Namespace, job.Metadata.Name, key, candidate.Path, input)
		if uploadErr == nil {
			return artifact, nil
		}
		if !retryableArtifactError(uploadErr) {
			return ArtifactRecord{}, uploadErr
		}
		if waitErr := waitForRetry(ctx, retryDelay(uploadErr, delay)); waitErr != nil {
			return ArtifactRecord{}, errors.Join(uploadErr, waitErr)
		}
		delay = nextBackoff(delay, p.retryMaxBackoff())
	}
}

func (p *ArtifactProcessor) completeManifestWithRetry(ctx context.Context, job JobResource, key string, input CompleteManifestInput) (CompletedManifest, error) {
	delay := time.Second
	for {
		manifest, err := p.Remote.CompleteManifest(ctx, job.Metadata.Namespace, job.Metadata.Name, key, input)
		if err == nil {
			return manifest, nil
		}
		known, getErr := p.Remote.GetManifest(ctx, job.Metadata.Namespace, job.Metadata.Name, job.Metadata.UID, artifactGeneration)
		if getErr == nil && known.State == "Completed" && known.ArtifactCount == len(input.Files) && known.Digest != "" {
			return known, nil
		}
		if !retryableArtifactError(err) {
			return CompletedManifest{}, fmt.Errorf("complete artifact manifest: %w", err)
		}
		if waitErr := waitForRetry(ctx, retryDelay(err, delay)); waitErr != nil {
			return CompletedManifest{}, fmt.Errorf("complete artifact manifest: %w", errors.Join(err, waitErr))
		}
		delay = nextBackoff(delay, p.retryMaxBackoff())
	}
}

func (p *ArtifactProcessor) retryMaxBackoff() time.Duration {
	maximum := p.RetryMaxBackoff
	if maximum <= 0 {
		maximum = 30 * time.Second
	}
	return maximum
}

func validateUploadedArtifact(job JobResource, candidate artifactCandidate, artifact ArtifactRecord) error {
	if artifact.ID == "" || artifact.State != "Completed" || artifact.Project != job.Metadata.Namespace ||
		artifact.JobName != job.Metadata.Name || artifact.JobUID != job.Metadata.UID || artifact.Category != "artifact" ||
		artifact.RelativePath != candidate.RelativePath || artifact.Size != candidate.Size || artifact.SHA256 != candidate.SHA256 {
		return fmt.Errorf("artifact upload response does not match %s", candidate.RelativePath)
	}
	return nil
}

func (p *ArtifactProcessor) receiptPath(job JobResource, relative string) string {
	sum := sha256.Sum256([]byte(relative))
	return filepath.Join(p.RootDir, "uploads", job.Metadata.Namespace, job.Metadata.UID, "artifacts", hex.EncodeToString(sum[:])+".json")
}

func (p *ArtifactProcessor) writeReceipt(job JobResource, relative string, receipt artifactReceipt) error {
	path := p.receiptPath(job, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return writeAtomicFile(path, data)
}

func (p *ArtifactProcessor) readReceipt(job JobResource, candidate artifactCandidate, key string) (artifactReceipt, bool) {
	data, err := os.ReadFile(p.receiptPath(job, candidate.RelativePath))
	if err != nil {
		return artifactReceipt{}, false
	}
	var receipt artifactReceipt
	if json.Unmarshal(data, &receipt) != nil || receipt.SchemaVersion != 1 || receipt.IdempotencyKey != key ||
		validateUploadedArtifact(job, candidate, receipt.Artifact) != nil {
		return artifactReceipt{}, false
	}
	return receipt, true
}

func validateJobIdentity(job JobResource) error {
	if job.Metadata.Namespace == "" || job.Metadata.Name == "" || job.Metadata.UID == "" {
		return fmt.Errorf("job namespace, name, and UID are required for artifact upload")
	}
	for _, value := range []string{job.Metadata.Namespace, job.Metadata.UID} {
		if value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
			return fmt.Errorf("invalid job identity for local artifact path")
		}
	}
	return nil
}
