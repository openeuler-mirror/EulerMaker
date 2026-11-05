package release

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/repository"
)

func TestReleaseDeletionRetriesAtRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		root: t.TempDir(), records: map[string]*ReleaseRecord{}, ctx: ctx,
		queued: map[string]bool{}, running: map[string]bool{}, queue: make(chan string, 1),
	}
	record := &ReleaseRecord{Project: "project", TargetArch: "arch", BuildName: "build", State: ReleasePrepared}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	if _, status, err := m.Delete(record.BuildName); err != nil || status != http.StatusAccepted {
		t.Fatalf("delete: status=%d, error=%v", status, err)
	}
	// Fail metadata removal after the content has already been moved and removed.
	metaPath := m.metaPath(record.BuildName)
	if err := os.Rename(metaPath, metaPath+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metaPath, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaPath, "blocker"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.Path(record), 0750); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { defer close(stopped); m.worker() }()
	t.Cleanup(func() { cancel(); <-stopped })
	wait := func(removed bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			m.mu.RLock()
			idle := !m.queued[record.BuildName] && !m.running[record.BuildName]
			absent := m.records[record.BuildName] == nil
			m.mu.RUnlock()
			if idle && absent == removed {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("cleanup did not settle: removed=%t", removed)
	}
	wait(false)
	got, ok, err := m.Get(record.BuildName)
	if err != nil || !ok || got.State != ReleaseDeleting {
		t.Fatalf("failed removal lost deletion intent: %+v, %v", got, err)
	}
	if err := os.RemoveAll(metaPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(metaPath+"-saved", metaPath); err != nil {
		t.Fatal(err)
	}
	// A retry also finds the stable trash path after an interrupted recursive removal.
	trash := filepath.Join(m.releaseTargetDir(record), "releases/.trash", record.BuildName+"-deleting")
	if err := os.MkdirAll(trash, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trash, "rpm"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	m.retryPending()
	wait(true)
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Fatalf("retry left trash: %v", err)
	}
	if _, ok, err := m.Get(record.BuildName); err != nil || ok {
		t.Fatalf("deleted metadata remains: %t, %v", ok, err)
	}
}

func TestReleaseCleanupQueueDeduplicatesActiveWork(t *testing.T) {
	m := &Manager{
		records: map[string]*ReleaseRecord{"build": {BuildName: "build", State: ReleaseDeleting}},
		queued:  map[string]bool{}, running: map[string]bool{"build": true}, queue: make(chan string, 1),
	}
	m.retryPending()
	if len(m.queue) != 0 {
		t.Fatal("cleanup must wait for the active worker")
	}
	delete(m.running, "build")
	for range 3 {
		m.retryPending()
	}
	if len(m.queue) != 1 || !m.queued["build"] {
		t.Fatalf("cleanup queue was not deduplicated: queued=%v length=%d", m.queued, len(m.queue))
	}
}

func TestStopBuildFencesReleaseSubmissionAndActivation(t *testing.T) {
	root := t.TempDir()
	source := &repository.RepositoryRecord{
		RepositoryUID: "source", Project: "project", TargetArch: "arch", BuildName: "build",
		State: repository.RepositoryReady,
	}
	if err := fileutil.AtomicJSON(filepath.Join(root, "repositories/project/arch/history/build/.records/source.json"), source); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.AtomicJSON(filepath.Join(root, "repositories/project/arch/history/build/steps/source/.repository.json"), repository.Index{RepositoryUID: "source"}); err != nil {
		t.Fatal(err)
	}
	repositories, err := repository.NewManager(root, repository.Config{WorkTTL: time.Hour}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Stop()
	request, digest, err := normalizeReleaseRequest(CreateReleaseRequest{
		Project: "project", TargetArch: "arch", BuildName: "build", SourceRepositoryUID: "source",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		root: root, repositories: repositories, records: map[string]*ReleaseRecord{}, running: map[string]bool{},
	}
	record := &ReleaseRecord{
		Project: "project", TargetArch: "arch", BuildName: "build", SourceRepositoryUID: "source",
		State: ReleaseCreating, RequestDigest: digest,
	}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	m.running[record.BuildName] = true
	if done, err := m.StopBuild("project", "arch", "build"); done || err != nil {
		t.Fatalf("active release stop: %t, %v", done, err)
	}
	// Completion after the fence may prepare files, but must not change current.
	m.finish(record, Result{Digest: "digest"}, nil)
	got, _, err := m.Get(record.BuildName)
	if err != nil || got.State != ReleasePrepared {
		t.Fatalf("release completion after stop: %+v, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "repositories/project/arch/current")); !os.IsNotExist(err) {
		t.Fatalf("stopped release changed current: %v", err)
	}
	if done, err := m.StopBuild("project", "arch", "build"); !done || err != nil {
		t.Fatalf("drained release stop: %t, %v", done, err)
	}
	for _, state := range []ReleaseState{ReleasePrepared, ReleaseFailed} {
		record.State, record.Failure = state, &FailureInfo{Retryable: true}
		if err := m.persist(record); err != nil {
			t.Fatal(err)
		}
		_, _, err := m.Submit(request)
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "BuildStopped" {
			t.Fatalf("late %s replay: %v", state, err)
		}
	}
	record.State = ReleasePrepared
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	// Reload the persisted Prepared record and fence, without a process-local stop flag.
	m = &Manager{root: root, repositories: repositories, records: map[string]*ReleaseRecord{}}
	if err := m.recover(time.Hour); err != nil {
		t.Fatal(err)
	}
	_, _, err = m.Activate(record.BuildName)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "BuildStopped" {
		t.Fatalf("activation after restart: %v", err)
	}
	if done, err := repositories.DeleteBuild("project", "arch", "build"); err != nil || !done {
		t.Fatalf("source cleanup: %t, %v", done, err)
	}
	_, _, err = m.Submit(request)
	if !errors.As(err, &failure) || failure.Code != "BuildStopped" {
		t.Fatalf("submission after source cleanup: %v", err)
	}
}

func TestDeleteBuildRelease(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*ReleaseRecord{}, running: map[string]bool{}}
	record := &ReleaseRecord{Project: "project", TargetArch: "arch", BuildName: "build", State: ReleasePrepared}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	target := m.releaseTargetDir(record)
	for _, path := range []string{
		"releases/build/Packages/rpm", "releases/.work/build-123/partial", "releases/.trash/build-d_123/rpm",
		"releases/.records/build.json.tmp-m_123",
		"releases/other/Packages/rpm", "releases/.work/build-other-123/partial", "releases/.trash/build-other-d_123/rpm",
	} {
		if err := fileutil.AtomicJSON(filepath.Join(target, path), map[string]string{"content": "rpm"}); err != nil {
			t.Fatal(err)
		}
	}
	current := filepath.Join(target, "current")
	if err := os.Symlink("releases/build", current); err != nil {
		t.Fatal(err)
	}
	done, err := m.DeleteBuild("project", "arch", "build")
	var failure *Error
	if done || !errors.As(err, &failure) || failure.Status != http.StatusConflict || failure.Code != "ReleaseInUse" {
		t.Fatalf("current protection: %t, %v", done, err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	m.records[record.BuildName] = record
	m.running[record.BuildName] = true
	if done, err := m.DeleteBuild("project", "arch", "build"); err != nil || done {
		t.Fatalf("running release: %t, %v", done, err)
	}
	delete(m.running, record.BuildName)
	// A failed filesystem operation retains metadata and can be retried.
	if err := os.Remove(m.metaPath(record.BuildName)); err != nil {
		t.Fatal(err)
	}
	if err := fileutil.AtomicJSON(filepath.Join(m.metaPath(record.BuildName), "obstruction"), "x"); err != nil {
		t.Fatal(err)
	}
	if done, err := m.DeleteBuild("project", "arch", "build"); err == nil || done {
		t.Fatalf("partial cleanup: %t, %v", done, err)
	}
	if err := os.RemoveAll(m.metaPath(record.BuildName)); err != nil {
		t.Fatal(err)
	}
	// No cached record or existing directory is required to finish after restart.
	m = &Manager{root: m.root, records: map[string]*ReleaseRecord{}}
	for range 2 {
		if done, err := m.DeleteBuild("project", "arch", "build"); err != nil || !done {
			t.Fatalf("cleanup retry: %t, %v", done, err)
		}
	}
	for _, path := range []string{
		"releases/build", "releases/.records/build.json", "releases/.records/build.json.tmp-m_123",
		"releases/.work/build-123", "releases/.trash/build-d_123",
	} {
		if _, err := os.Stat(filepath.Join(target, path)); !os.IsNotExist(err) {
			t.Fatalf("cleanup left %s: %v", path, err)
		}
	}
	for _, path := range []string{"releases/other/Packages/rpm", "releases/.work/build-other-123/partial", "releases/.trash/build-other-d_123/rpm"} {
		if _, err := os.Stat(filepath.Join(target, path)); err != nil {
			t.Fatalf("unrelated path %s: %v", path, err)
		}
	}
}
