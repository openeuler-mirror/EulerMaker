package upload_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/upload"
)

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestManifestReadOnDemandAndSealingProtection(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetManifest("project", "job"); !os.IsNotExist(err) {
		t.Fatalf("missing manifest: %v", err)
	}
	data := []byte("rpm")
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: int64(len(data)), SHA256: sum(data)}
	a, _, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, ".uploads", a.ID+".tmp")
	if err := os.WriteFile(tmp, data, 0640); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload("project", "job", a, tmp); err != nil {
		t.Fatal(err)
	}
	request := upload.CompleteManifestRequest{Files: []upload.ManifestFile{{
		ArtifactID: a.ID, RelativePath: a.RelativePath, Category: a.Category, Size: a.Size, SHA256: a.SHA256,
	}}}
	manifest, err := store.CompleteManifest("project", "job", "runner", request)
	if err != nil {
		t.Fatal(err)
	}
	manifest.RunnerName = "updated-runner"
	path := filepath.Join(root, ".metadata/jobs/project/job/manifest.json")
	if err := fileutil.AtomicJSON(path, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetManifest("project", "job")
	if err != nil || loaded.RunnerName != manifest.RunnerName {
		t.Fatalf("disk manifest: %+v, %v", loaded, err)
	}
	replayed, err := store.CompleteManifest("project", "job", "runner", request)
	if err != nil || replayed.RunnerName != manifest.RunnerName || replayed.Digest != manifest.Digest {
		t.Fatalf("seal replay from disk: %+v, %v", replayed, err)
	}
	if _, _, err := store.BeginUpload("project", "job", "new-key", metadata, 1024); err == nil || err.Error() != "ManifestAlreadyCompleted" {
		t.Fatalf("sealed Job upload: %v", err)
	}
	// Corrupted metadata must block writes instead of being treated as an unsealed Job.
	corrupted := []byte("{")
	if err := os.WriteFile(path, corrupted, 0600); err != nil {
		t.Fatal(err)
	}
	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatalf("startup should not load historical manifests: %v", err)
	}
	if _, err := store.GetManifest("project", "job"); !errors.Is(err, upload.ErrInvalidManifest) {
		t.Fatalf("read corrupted manifest: %v", err)
	}
	if _, _, err := store.BeginUpload("project", "job", "key", metadata, 1024); !errors.Is(err, upload.ErrInvalidManifest) {
		t.Fatalf("reserve upload with corrupted manifest: %v", err)
	}
	if err := store.CompleteUpload("project", "job", a, tmp); !errors.Is(err, upload.ErrInvalidManifest) {
		t.Fatalf("commit upload with corrupted manifest: %v", err)
	}
	if _, err := store.CompleteManifest("project", "job", "runner", request); !errors.Is(err, upload.ErrInvalidManifest) {
		t.Fatalf("seal corrupted manifest: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(content, corrupted) {
		t.Fatalf("manifest was overwritten: %q, %v", content, err)
	}
}

func TestArtifactMetadataReadOnDemand(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: 3, SHA256: sum([]byte("rpm"))}
	a, _, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, ".uploads", a.ID+".tmp")
	if err := os.WriteFile(tmp, []byte("rpm"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload("project", "job", a, tmp); err != nil {
		t.Fatal(err)
	}
	// Disk metadata is authoritative without restarting the Store.
	a.ContentType = "application/x-rpm"
	path := filepath.Join(root, ".metadata/jobs/project/job/artifacts", a.ID+".json")
	if err := fileutil.AtomicJSON(path, a); err != nil {
		t.Fatal(err)
	}
	loaded, ok := store.GetJobArtifact("project", "job", a.ID)
	if !ok || loaded.ContentType != a.ContentType {
		t.Fatalf("read disk metadata: %+v", loaded)
	}
	items, err := store.ListArtifacts("project", "job", upload.CategoryArtifact)
	if err != nil || len(items) != 1 || items[0].ContentType != a.ContentType {
		t.Fatalf("list disk metadata: %+v, %v", items, err)
	}
	metadata.ContentType = a.ContentType
	replayed, replay, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil || !replay || replayed.ID != a.ID {
		t.Fatalf("replay from disk: %+v, %t, %v", replayed, replay, err)
	}
}

func TestConcurrentUploadReservationAndRetry(t *testing.T) {
	store, err := upload.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: 3, SHA256: sum([]byte("rpm"))}
	const requests = 16
	accepted := make(chan *upload.Artifact, requests)
	var workers sync.WaitGroup
	for i := 0; i < requests; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			a, replay, err := store.BeginUpload("project", "job", "key", metadata, 1024)
			if err != nil {
				if err.Error() != "UploadInProgress" {
					t.Errorf("reserve upload: %v", err)
				}
				return
			}
			if replay {
				t.Error("unfinished upload returned as replay")
			}
			accepted <- a
		}()
	}
	workers.Wait()
	close(accepted)
	if len(accepted) != 1 {
		t.Fatalf("accepted %d uploads, want 1", len(accepted))
	}
	a := <-accepted
	store.FailUpload("project", "job", a, "StorageError", "interrupted")
	retried, replay, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil || replay || retried.ID != a.ID || retried.State != upload.Pending {
		t.Fatalf("retry persisted failure: %+v, %t, %v", retried, replay, err)
	}
}

func TestFinalizingLogLoadsArtifactFromDisk(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("log")
	if _, err := store.AppendLog("project", "job", "runner", 0, data, sum(data)); err != nil {
		t.Fatal(err)
	}
	request := upload.CompleteLogRequest{Stream: "combined", LastSequence: 0, Size: int64(len(data)), SHA256: sum(data)}
	a, err := store.CompleteLog("project", "job", "runner", request)
	if err != nil {
		t.Fatal(err)
	}
	a.State, a.CompletedAt = upload.Pending, nil
	if err := fileutil.AtomicJSON(filepath.Join(root, ".metadata/jobs/project/job/artifacts", a.ID+".json"), a); err != nil {
		t.Fatal(err)
	}
	l, _ := store.GetLog("project", "job")
	l.State, l.CompletedAt = upload.LogFinalizing, nil
	_, _, meta := store.LogPaths("project", "job")
	if err := fileutil.AtomicJSON(meta, l); err != nil {
		t.Fatal(err)
	}
	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.CompleteLog("project", "job", "runner", request)
	if err != nil || recovered.ID != a.ID || recovered.State != upload.Completed {
		t.Fatalf("recover log artifact: %+v, %v", recovered, err)
	}
	events, _, err := store.ReplayLog("project", "job", -1, 10)
	if err != nil || len(events) != 1 || !bytes.Equal(events[0].Data, data) {
		t.Fatalf("replay recovered log: %+v, %v", events, err)
	}
}

func TestJobMetadataLayoutSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("rpm content")
	metadata := upload.UploadMetadata{
		Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm",
		Size: int64(len(data)), SHA256: sum(data),
	}
	type jobArtifacts struct {
		project, job  string
		artifact, log *upload.Artifact
	}
	jobs := []jobArtifacts{{project: "project-a", job: "job"}, {project: "project-b", job: "job"}}
	for i := range jobs {
		job := &jobs[i]
		job.artifact, _, err = store.BeginUpload(job.project, job.job, "key", metadata, 1024)
		if err != nil {
			t.Fatal(err)
		}
		temporary := filepath.Join(root, ".uploads", job.artifact.ID+".tmp")
		if err := os.WriteFile(temporary, data, 0640); err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteUpload(job.project, job.job, job.artifact, temporary); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendLog(job.project, job.job, "runner", 0, data, sum(data)); err != nil {
			t.Fatal(err)
		}
		body, index, meta := store.LogPaths(job.project, job.job)
		wantMeta := filepath.Join(root, ".metadata/jobs", job.project, job.job, "logs", "combined.json")
		if meta != wantMeta || body != filepath.Join(root, ".logs", job.project, job.job, "combined.log") ||
			index != filepath.Join(root, ".logs", job.project, job.job, "combined.index.jsonl") {
			t.Fatalf("log paths: body=%s index=%s meta=%s", body, index, meta)
		}
		checkLogMetadata := func(state upload.LogState) {
			t.Helper()
			encoded, err := os.ReadFile(wantMeta)
			if err != nil {
				t.Fatal(err)
			}
			var persisted upload.LogStream
			if err := json.Unmarshal(encoded, &persisted); err != nil || persisted.State != state ||
				persisted.Project != job.project || persisted.JobName != job.job {
				t.Fatalf("persisted log: %+v, error: %v", persisted, err)
			}
		}
		checkLogMetadata(upload.LogOpen)
		job.log, err = store.CompleteLog(job.project, job.job, "runner", upload.CompleteLogRequest{
			Stream: "combined", LastSequence: 0, Size: int64(len(data)), SHA256: sum(data),
		})
		if err != nil {
			t.Fatal(err)
		}
		var files []upload.ManifestFile
		checkLogMetadata(upload.LogCompleted)
		for _, artifact := range []*upload.Artifact{job.artifact, job.log} {
			path := filepath.Join(root, ".metadata/jobs", job.project, job.job, "artifacts", artifact.ID+".json")
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var persisted upload.Artifact
			if err := json.Unmarshal(encoded, &persisted); err != nil || persisted.ID != artifact.ID || persisted.State != upload.Completed {
				t.Fatalf("persisted artifact: %+v, error: %v", persisted, err)
			}
			files = append(files, upload.ManifestFile{
				ArtifactID: artifact.ID, RelativePath: artifact.RelativePath, Category: artifact.Category,
				Size: artifact.Size, SHA256: artifact.SHA256, Required: true,
			})
		}
		if _, err := store.CompleteManifest(job.project, job.job, "runner", upload.CompleteManifestRequest{Files: files}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, ".metadata/jobs", job.project, job.job, "manifest.json")); err != nil {
			t.Fatal(err)
		}
	}
	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		artifact, ok := store.GetJobArtifact(job.project, job.job, job.artifact.ID)
		if !ok || artifact.ID != job.artifact.ID || artifact.State != upload.Completed {
			t.Fatalf("loaded Job artifact: %+v", artifact)
		}
		manifest, err := store.GetManifest(job.project, job.job)
		if err != nil || manifest.State != upload.ManifestCompleted || len(manifest.Files) != 2 {
			t.Fatalf("loaded manifest: %+v, %v", manifest, err)
		}
		items, err := store.ListArtifacts(job.project, job.job, "")
		if err != nil || len(items) != 2 {
			t.Fatalf("loaded artifacts: %+v, error: %v", items, err)
		}
		replayed, replay, err := store.BeginUpload(job.project, job.job, "key", metadata, 1024)
		if err != nil || !replay || replayed.ID != job.artifact.ID {
			t.Fatalf("replay: %+v, replay=%t, error: %v", replayed, replay, err)
		}
		log, ok := store.GetLog(job.project, job.job)
		if !ok || log.State != upload.LogCompleted || log.ArtifactID != job.log.ID {
			t.Fatalf("loaded log: %+v", log)
		}
	}
}

func TestArtifactOwnershipSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("rpm")
	metadata := upload.UploadMetadata{
		Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm",
		Size: int64(len(data)), SHA256: sum(data),
	}
	a, _, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(root, ".uploads", a.ID+".tmp")
	if err := os.WriteFile(temporary, data, 0640); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload("project", "job", a, temporary); err != nil {
		t.Fatal(err)
	}
	request := upload.CompleteManifestRequest{Files: []upload.ManifestFile{{
		ArtifactID: a.ID, RelativePath: a.RelativePath, Category: a.Category, Size: a.Size, SHA256: a.SHA256,
	}}}
	for _, restart := range []bool{false, true} {
		if restart {
			store, err = upload.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, scope := range [][2]string{{"another-project", "job"}, {"project", "another-job"}} {
			if _, ok := store.GetJobArtifact(scope[0], scope[1], a.ID); ok {
				t.Fatalf("artifact visible to %v after restart=%t", scope, restart)
			}
			if _, err := store.CompleteManifest(scope[0], scope[1], "runner", request); err == nil || err.Error() != "artifact mismatch" {
				t.Fatalf("manifest ownership check for %v after restart=%t: %v", scope, restart, err)
			}
		}
		if _, err := store.CompleteManifest("project", "job", "runner", request); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreRecoversPendingCommittedUploadAndLogTail(t *testing.T) {
	root := t.TempDir()
	s, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	m := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "x", RelativePath: "x", Size: 3, SHA256: sum([]byte("abc"))}
	a, _, err := s.BeginUpload("project", "job", "key", m, 1024)
	if err != nil {
		t.Fatal(err)
	}
	final := s.ArtifactPath(a)
	if err := os.MkdirAll(filepath.Dir(final), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("abc"), 0640); err != nil {
		t.Fatal(err)
	}
	s, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := s.GetJobArtifact("project", "job", a.ID)
	if !ok || recovered.State != upload.Completed {
		t.Fatalf("artifact was not recovered: %#v", recovered)
	}

	chunk := []byte("committed")
	if _, err := s.AppendLog("project", "another-job", "runner", 0, chunk, sum(chunk)); err != nil {
		t.Fatal(err)
	}
	body, _, _ := s.LogPaths("project", "another-job")
	f, err := os.OpenFile(body, os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, "uncommitted-tail")
	_ = f.Close()
	s, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := s.GetLog("project", "another-job")
	if !ok || l.CommittedBytes != int64(len(chunk)) {
		t.Fatalf("log was not recovered: %#v", l)
	}
	content, _ := os.ReadFile(body)
	if !bytes.Equal(content, chunk) {
		t.Fatalf("uncommitted tail was not truncated: %q", content)
	}
}

func TestStoreRestartOnlyVerifiesProcessingUploads(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: 3, SHA256: sum([]byte("abc"))}
	artifact, _, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(root, ".uploads", artifact.ID+".tmp")
	if err := os.WriteFile(temporary, []byte("abc"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload("project", "job", artifact, temporary); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.ArtifactPath(artifact), []byte("bad"), 0640); err != nil {
		t.Fatal(err)
	}

	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := store.GetJobArtifact("project", "job", artifact.ID)
	if !ok || loaded.State != upload.Completed {
		t.Fatalf("completed upload changed during startup: %+v", loaded)
	}
	_, replay, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil || replay {
		t.Fatalf("corrupted completed upload replayed: replay=%t, error=%v", replay, err)
	}
}

func TestStoreUploadIdempotencySurvivesRestart(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: 3, SHA256: sum([]byte("abc"))}
	artifact, replay, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil || replay {
		t.Fatalf("begin upload: replay=%t, error=%v", replay, err)
	}
	temporary := filepath.Join(root, ".uploads", artifact.ID+".tmp")
	if err := os.WriteFile(temporary, []byte("abc"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload("project", "job", artifact, temporary); err != nil {
		t.Fatal(err)
	}
	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := store.GetJobArtifact("project", "job", artifact.ID)
	if !ok || loaded.IdempotencyKey != "key" {
		t.Fatalf("upload identity was not persisted: %+v", loaded)
	}
	replayed, replay, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil || !replay || replayed.ID != artifact.ID {
		t.Fatalf("replay after restart: artifact=%+v, replay=%t, error=%v", replayed, replay, err)
	}
	metadata.ContentType = "application/x-rpm"
	if _, _, err := store.BeginUpload("project", "job", "key", metadata, 1024); err == nil || err.Error() != "IdempotencyConflict" {
		t.Fatalf("changed request reused key: %v", err)
	}
}

func TestStoreRetryFailedUploadAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, err := upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata := upload.UploadMetadata{Category: upload.CategoryArtifact, FileName: "package.rpm", RelativePath: "packages/package.rpm", Size: 3, SHA256: sum([]byte("abc"))}
	artifact, _, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil {
		t.Fatal(err)
	}
	store, err = upload.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	retried, replay, err := store.BeginUpload("project", "job", "key", metadata, 1024)
	if err != nil || replay || retried.ID != artifact.ID || retried.State != upload.Pending {
		t.Fatalf("retry after restart: artifact=%+v, replay=%t, error=%v", retried, replay, err)
	}
}
