package repository

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"k8s.io/klog/v2"

	"artifact-manager/pkg/fileutil"
)

func TestRepositoryCompletionWaitsForDurableResult(t *testing.T) {
	for _, state := range []RepositoryState{RepositoryReady, RepositoryFailed} {
		t.Run(string(state), func(t *testing.T) {
			m := &Manager{
				root: t.TempDir(), records: map[string]*RepositoryRecord{},
				queued: map[string]bool{}, running: map[string]bool{}, queue: make(chan string, 1),
			}
			record := &RepositoryRecord{
				RepositoryUID: "repo", Project: "project", TargetArch: "arch", BuildName: "build",
				State: RepositoryCreating,
			}
			if err := m.persist(record); err != nil {
				t.Fatal(err)
			}
			m.records[record.RepositoryUID] = cloneRepository(record)
			m.running[record.RepositoryUID] = true
			if err := fileutil.AtomicJSON(filepath.Join(m.RepositoryPath(record), ".repository.json"), Index{}); err != nil {
				t.Fatal(err)
			}
			// An obstructing metadata parent makes result persistence fail even when tests run as root.
			dir := filepath.Dir(m.metaPath(record.RepositoryUID))
			if err := os.Rename(dir, dir+"-saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, nil, 0600); err != nil {
				t.Fatal(err)
			}
			var materializeErr error
			if state == RepositoryFailed {
				materializeErr = errors.New("materialization failed")
			}
			m.finish(record, Result{Digest: "digest"}, materializeErr)
			got, ok, err := m.Get(record.RepositoryUID)
			if err != nil || !ok || got.State != RepositoryCreating || got.Failure != nil || got.ContentURL != "" {
				t.Fatalf("unpersisted completion exposed: %+v, %v", got, err)
			}
			if done, err := m.StopBuild(record.Project, record.BuildName); err != nil || done {
				t.Fatalf("unpersisted completion drained: %t, %v", done, err)
			}
			m.retryPending()
			m.retryPending()
			if len(m.queue) != 0 || m.running[record.RepositoryUID] || m.completions[record.RepositoryUID] == nil {
				t.Fatalf("completion must retry persistence only: queued=%d pending=%v", len(m.queue), m.completions)
			}
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(dir+"-saved", dir); err != nil {
				t.Fatal(err)
			}
			persisted, err := m.readRecord(record.RepositoryUID)
			if err != nil || persisted.State != RepositoryCreating {
				t.Fatalf("last durable state: %+v, %v", persisted, err)
			}
			restarted := &Manager{root: m.root, records: map[string]*RepositoryRecord{}}
			if err := restarted.load(); err != nil {
				t.Fatal(err)
			}
			if done, err := restarted.StopBuild(record.Project, record.BuildName); err != nil || done {
				t.Fatalf("restart acknowledged an unfinished record: %t, %v", done, err)
			}
			m.retryPending()
			got, ok, err = m.Get(record.RepositoryUID)
			if err != nil || !ok || got.State != state {
				t.Fatalf("persisted completion: %+v, %v", got, err)
			}
			if done, err := m.StopBuild(record.Project, record.BuildName); err != nil || !done {
				t.Fatalf("durable completion still pending: %t, %v", done, err)
			}
			if len(m.completions) != 0 || len(m.queue) != 0 {
				t.Fatal("completed result retained or materialization requeued")
			}
		})
	}
}

func TestRepositoryRecoveryContentURL(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*RepositoryRecord{}}
	record := &RepositoryRecord{
		RepositoryUID: "repo", Project: "project", TargetArch: "arch", BuildName: "build",
		State: RepositoryCreating, RequestDigest: "request-digest",
	}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(m.RepositoryPath(record), ".repository.json")
	index := Index{RepositoryUID: record.RepositoryUID, RequestDigest: record.RequestDigest}
	if err := fileutil.AtomicJSON(indexPath, index); err != nil {
		t.Fatal(err)
	}
	var err error
	index.RepositoryDigest, err = digestDirectory(m.RepositoryPath(record))
	if err != nil {
		t.Fatal(err)
	}
	if err := fileutil.AtomicJSON(indexPath, index); err != nil {
		t.Fatal(err)
	}
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	if err := m.recover(time.Hour); err != nil {
		t.Fatal(err)
	}
	got, ok, err := m.Get(record.RepositoryUID)
	if err != nil || !ok || got.State != RepositoryReady || got.ContentURL != "/repositories/project/builds/build/repo/" {
		t.Fatalf("recovered repository = %+v, error = %v", got, err)
	}
}

func TestRepositoryDeletionRetriesAtRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		root: t.TempDir(), records: map[string]*RepositoryRecord{}, ctx: ctx,
		queued: map[string]bool{}, running: map[string]bool{}, queue: make(chan string, 1),
	}
	record := &RepositoryRecord{
		RepositoryUID: "repo", Project: "project", TargetArch: "arch", BuildName: "build", State: RepositoryCreating,
	}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	m.records[record.RepositoryUID] = record
	trashDir := filepath.Join(m.root, "repositories/project/arch/history/build/.trash")
	if err := os.WriteFile(trashDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, status, err := m.Delete(record.RepositoryUID); err != nil || status != 202 {
		t.Fatalf("delete: status=%d, error=%v", status, err)
	}
	stopped := make(chan struct{})
	go func() { defer close(stopped); m.worker() }()
	t.Cleanup(func() { cancel(); <-stopped })
	wait := func(removed bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			m.mu.RLock()
			idle := !m.queued[record.RepositoryUID] && !m.running[record.RepositoryUID]
			absent := m.records[record.RepositoryUID] == nil
			m.mu.RUnlock()
			if idle && absent == removed {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("cleanup did not settle: removed=%t", removed)
	}
	wait(false)
	if done, err := m.StopBuild(record.Project, record.BuildName); err != nil || done {
		t.Fatalf("failed deletion drained: %t, %v", done, err)
	}
	if err := os.Remove(trashDir); err != nil {
		t.Fatal(err)
	}
	// Resume a directory already moved to trash before an interrupted attempt.
	trash := filepath.Join(trashDir, record.RepositoryUID+"-deleting")
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
	if _, ok, err := m.Get(record.RepositoryUID); err != nil || ok {
		t.Fatalf("deleted metadata remains: %t, %v", ok, err)
	}
	if done, err := m.StopBuild(record.Project, record.BuildName); err != nil || !done {
		t.Fatalf("successful deletion not drained: %t, %v", done, err)
	}
}

func TestRepositoryCleanupWaitsForMaterialization(t *testing.T) {
	m := &Manager{
		root: t.TempDir(), records: map[string]*RepositoryRecord{},
		queued: map[string]bool{}, running: map[string]bool{}, queue: make(chan string, 1),
	}
	record := &RepositoryRecord{
		RepositoryUID: "repo", Project: "project", TargetArch: "arch", BuildName: "build", State: RepositoryCreating,
	}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	m.records[record.RepositoryUID] = record
	m.running[record.RepositoryUID] = true
	if _, _, err := m.Delete(record.RepositoryUID); err != nil {
		t.Fatal(err)
	}
	m.retryPending()
	if len(m.queue) != 0 {
		t.Fatal("cleanup must wait for materialization to exit")
	}
	m.finish(record, Result{Digest: "late-result"}, nil)
	if got, _, err := m.Get(record.RepositoryUID); err != nil || got.State != RepositoryDeleting {
		t.Fatalf("late completion replaced deletion intent: %+v, %v", got, err)
	}
	for range 3 {
		m.retryPending()
	}
	if len(m.queue) != 1 || !m.queued[record.RepositoryUID] {
		t.Fatalf("cleanup queue was not deduplicated: queued=%v length=%d", m.queued, len(m.queue))
	}
}

func TestRepositoryManagerCachesOnlyRecoverableStates(t *testing.T) {
	root := t.TempDir()
	m := &Manager{root: root, records: map[string]*RepositoryRecord{}}
	for _, state := range []RepositoryState{RepositoryCreating, RepositoryDeleting, RepositoryReady, RepositoryFailed} {
		record := &RepositoryRecord{
			RepositoryUID: string(state),
			State:         state,
			Project:       "project",
			BuildName:     "build",
			TargetArch:    "arch",
		}
		if state == RepositoryReady {
			record.RepositoryDigest = "repository-digest"
		}
		if err := m.persist(record); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, "repositories/project/arch/history/build/.records", record.RepositoryUID+".json")
		if m.metaPath(record.RepositoryUID) != want {
			t.Fatalf("metadata path = %s, want %s", m.metaPath(record.RepositoryUID), want)
		}
		if state == RepositoryReady {
			path := m.RepositoryPath(record)
			if err := os.MkdirAll(path, 0750); err != nil {
				t.Fatal(err)
			}
			if err := fileutil.AtomicJSON(filepath.Join(path, ".repository.json"), Index{
				RepositoryUID: record.RepositoryUID,
				RPMs:          map[string]RepositoryRPMMeta{},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	m.metadataPaths = nil
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	if len(m.records) != 2 || m.records[string(RepositoryCreating)] == nil || m.records[string(RepositoryDeleting)] == nil {
		t.Fatalf("cached states = %+v", m.records)
	}
	for _, state := range []RepositoryState{RepositoryReady, RepositoryFailed} {
		record, ok, err := m.Get(string(state))
		if err != nil || !ok || record.State != state {
			t.Fatalf("get %s = %+v, %v", state, record, err)
		}
		if state == RepositoryReady && record.RepositoryDigest != "repository-digest" {
			t.Fatalf("persisted repository digest = %q", record.RepositoryDigest)
		}
	}
	if len(m.records) != 2 {
		t.Fatalf("on-demand reads were cached: %+v", m.records)
	}
	ready := &RepositoryRecord{
		RepositoryUID: string(RepositoryReady),
		Project:       "project",
		BuildName:     "build",
		TargetArch:    "arch",
	}
	if err := os.Remove(filepath.Join(m.RepositoryPath(ready), ".repository.json")); err != nil {
		t.Fatal(err)
	}
	record, ok, err := m.Get(ready.RepositoryUID)
	if err != nil || !ok || record.State != RepositoryFailed || record.Failure == nil || record.Failure.Code != "RepositoryContentMissing" {
		t.Fatalf("missing Ready content = %+v, %v", record, err)
	}
}

func TestRepositoryDeletionRemovesLocationIndex(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*RepositoryRecord{}}
	record := &RepositoryRecord{RepositoryUID: "repo", Project: "project", TargetArch: "arch", BuildName: "build", State: RepositoryDeleting}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	m.records[record.RepositoryUID] = record
	path := m.metaPath(record.RepositoryUID)
	source := m.RepositoryPath(record)
	if err := os.MkdirAll(source, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".repository.json"), []byte("{}"), 0640); err != nil {
		t.Fatal(err)
	}
	m.remove(record.RepositoryUID)
	if entries, err := os.ReadDir(filepath.Join(m.root, "repositories/project/arch/history/build/.trash")); err != nil || len(entries) != 0 {
		t.Fatalf("scoped repository trash: %v, %v", entries, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("deleted repository content: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deleted metadata: %v", err)
	}
	if len(m.metadataPaths) != 0 || len(m.records) != 0 {
		t.Fatalf("deletion retained index or record: %v, %v", m.metadataPaths, m.records)
	}
	if _, ok, err := m.Get(record.RepositoryUID); err != nil || ok {
		t.Fatalf("deleted lookup: %t, %v", ok, err)
	}
}

func TestDeleteBuildRepositoriesScopesAndRecovery(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*RepositoryRecord{}}
	for _, scope := range []struct{ uid, project, arch, build string }{
		{"first", "project", "arch", "build"},
		{"second", "project", "arch", "build"},
		{"base", "project", "arch", "previous"},
		{"other-arch", "project", "other", "build"},
		{"other-project", "other", "arch", "build"},
	} {
		record := &RepositoryRecord{
			RepositoryUID: scope.uid, Project: scope.project, TargetArch: scope.arch,
			BuildName: scope.build, State: RepositoryReady,
		}
		if err := m.persist(record); err != nil {
			t.Fatal(err)
		}
		if err := fileutil.AtomicJSON(filepath.Join(m.RepositoryPath(record), ".repository.json"), Index{}); err != nil {
			t.Fatal(err)
		}
	}
	buildDir := filepath.Join(m.root, "repositories/project/arch/history/build")
	for _, path := range []string{".work/interrupted", ".trash/interrupted"} {
		if err := os.MkdirAll(filepath.Join(buildDir, path), 0750); err != nil {
			t.Fatal(err)
		}
	}
	m.records["second"] = &RepositoryRecord{
		RepositoryUID: "second", Project: "project", TargetArch: "arch", BuildName: "build", State: RepositoryCreating,
	}
	if done, err := m.DeleteBuild("project", "build"); err != nil || done {
		t.Fatalf("active cleanup = %t, %v", done, err)
	}
	delete(m.records, "second")
	m.records["other-arch"] = &RepositoryRecord{
		RepositoryUID: "other-arch", Project: "project", TargetArch: "other", BuildName: "build", State: RepositoryCreating,
	}
	if done, err := m.StopBuild("project", "build"); err != nil || done {
		t.Fatalf("active task on another architecture was not drained: %t, %v", done, err)
	}
	if done, err := m.DeleteBuild("project", "build"); err != nil || done {
		t.Fatalf("active task on another architecture was deleted: %t, %v", done, err)
	}
	delete(m.records, "other-arch")
	// Simulate a restart after some metadata was removed but content still remains.
	if err := os.Remove(m.metaPath("first")); err != nil {
		t.Fatal(err)
	}
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if done, err := m.DeleteBuild("project", "build"); err != nil || !done {
			t.Fatalf("cleanup = %t, %v", done, err)
		}
	}
	if _, err := os.Stat(buildDir); !os.IsNotExist(err) {
		t.Fatalf("build directory still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.root, "repositories/project/other/history/build")); !os.IsNotExist(err) {
		t.Fatalf("build directory on another architecture still exists: %v", err)
	}
	if err := CheckBuildActive(m.root, "project", "build"); err == nil {
		t.Fatal("cleanup removed the project-level stop marker")
	}
	for _, uid := range []string{"first", "second", "other-arch"} {
		if _, ok, err := m.Get(uid); err != nil || ok {
			t.Fatalf("removed repository %s: exists=%t, %v", uid, ok, err)
		}
	}
	for _, uid := range []string{"base", "other-project"} {
		if record, ok, err := m.Get(uid); err != nil || !ok || record.State != RepositoryReady {
			t.Fatalf("unrelated repository %s: %+v, %v", uid, record, err)
		}
	}
}

func TestBuildCleanupReleasesGlobalLock(t *testing.T) {
	m := &Manager{
		root: t.TempDir(), records: map[string]*RepositoryRecord{},
		queued: map[string]bool{}, running: map[string]bool{}, queue: make(chan string, 2),
	}
	record := &RepositoryRecord{
		RepositoryUID: "repo", Project: "project", TargetArch: "arch", BuildName: "build", State: RepositoryFailed,
	}
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	started, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	storageErr := errors.New("cleanup interrupted")
	var done bool
	var cleanupErr error
	go func() {
		defer close(finished)
		done, cleanupErr = m.withBuildCleanup("project", "build", func() error {
			close(started)
			<-resume
			return storageErr
		})
	}()
	t.Cleanup(func() { close(resume); <-finished })
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not reach filesystem work")
	}
	if !m.mu.TryLock() {
		t.Fatal("filesystem cleanup still holds the global mutex")
	}
	m.mu.Unlock()
	if done, err := m.DeleteBuild("project", "build"); done || err != nil {
		t.Fatalf("concurrent same-build deletion: %t, %v", done, err)
	}
	var conflict *Error
	if _, _, err := m.Delete(record.RepositoryUID); !errors.As(err, &conflict) || conflict.Status != 409 || len(m.queue) != 0 {
		t.Fatalf("single repository deletion raced with build cleanup: %v", err)
	}
	in := CreateRepositoryRequest{
		Project: "project", TargetArch: "arch", BuildName: "build", RepositoryName: "build",
		Manifests: []ManifestReference{{JobName: "job"}},
	}
	in.RepositoryUID = UID(in.Project, in.BuildName, "", in.Manifests)
	if _, _, err := m.Submit(in); err == nil {
		t.Fatal("submission was accepted during cleanup")
	}
	// Another build can submit, query, persist completion and delete while the first is blocked on disk.
	in.BuildName, in.RepositoryName = "other", "other"
	in.RepositoryUID = UID(in.Project, in.BuildName, "", in.Manifests)
	other, _, err := m.Submit(in)
	if err != nil {
		t.Fatal(err)
	}
	m.finish(other, Result{}, errors.New("materialization failed"))
	if got, ok, err := m.Get(other.RepositoryUID); err != nil || !ok || got.State != RepositoryFailed {
		t.Fatalf("unrelated result: %+v, %v", got, err)
	}
	if done, err := m.DeleteBuild("project", "other"); err != nil || !done {
		t.Fatalf("unrelated cleanup: %t, %v", done, err)
	}
	// Let the injected I/O failure return, then retry using the real filesystem path.
	resume <- struct{}{}
	<-finished
	if done || !errors.Is(cleanupErr, storageErr) || len(m.cleaningBuilds) != 0 || m.metaPath(record.RepositoryUID) == "" {
		t.Fatalf("failed cleanup lost recovery state: done=%t error=%v", done, cleanupErr)
	}
	if done, err := m.DeleteBuild("project", "build"); err != nil || !done {
		t.Fatalf("retry: %t, %v", done, err)
	}
	if m.metaPath(record.RepositoryUID) != "" || len(m.cleaningBuilds) != 0 {
		t.Fatal("completed cleanup retained index or reservation")
	}
}

func TestRepositorySigningFingerprintIsFixedForRequest(t *testing.T) {
	root := t.TempDir()
	m := &Manager{
		root:               root,
		signingFingerprint: "new-key",
		records:            map[string]*RepositoryRecord{},
		queued:             map[string]bool{},
		queue:              make(chan string, 1),
	}
	in := CreateRepositoryRequest{
		RepositoryName:    "build",
		Project:           "project",
		BuildName:         "build",
		TargetArch:        "arch",
		BaseRepositoryUID: "base",
		Manifests:         []ManifestReference{{JobName: "job"}},
	}
	in.RepositoryUID = UID(in.Project, in.BuildName, in.BaseRepositoryUID, in.Manifests)
	m.records["base"] = &RepositoryRecord{
		RepositoryUID:      "base",
		Project:            in.Project,
		TargetArch:         in.TargetArch,
		State:              RepositoryReady,
		SigningFingerprint: "old-key",
	}
	_, _, err := m.Submit(in)
	if typed, ok := err.(*Error); !ok || typed.Code != "BaseRepositorySigningMismatch" {
		t.Fatalf("submit = %v", err)
	}
	in.BaseRepositoryUID = ""
	in.RepositoryUID = UID(in.Project, in.BuildName, in.BaseRepositoryUID, in.Manifests)
	record, _, err := m.Submit(in)
	if err != nil || record.SigningFingerprint != "new-key" {
		t.Fatalf("new record = %+v, %v", record, err)
	}
	m.signingFingerprint = "changed-key"
	_, _, err = m.Submit(in)
	if typed, ok := err.(*Error); !ok || typed.Code != "SigningConfigurationChanged" {
		t.Fatalf("retry = %v", err)
	}
}

func TestRepositoryRejectsUnsafeTargetArch(t *testing.T) {
	request := CreateRepositoryRequest{
		RepositoryName: "build",
		Project:        "project",
		BuildName:      "build",
		TargetArch:     "../other",
		Manifests:      []ManifestReference{{JobName: "job"}},
	}
	request.RepositoryUID = UID(request.Project, request.BuildName, "", request.Manifests)
	if _, _, err := normalizeRepositoryRequest(request); err == nil {
		t.Fatal("unsafe target architecture accepted")
	}
}

func TestManagerFailureLogs(t *testing.T) {
	var output bytes.Buffer
	state := klog.CaptureState()
	t.Cleanup(func() { klog.Flush(); state.Restore() })
	flags := flag.NewFlagSet("logging-test", flag.ContinueOnError)
	klog.InitFlags(flags)
	for name, value := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL", "one_output": "true"} {
		if err := flags.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	klog.SetOutput(&output)
	record := &RepositoryRecord{RepositoryUID: "record", Project: "project", TargetArch: "arch", BuildName: "build", State: RepositoryCreating}
	m := &Manager{root: t.TempDir(), records: map[string]*RepositoryRecord{"record": record}, running: map[string]bool{}}
	m.finish(record, Result{}, &os.PathError{Op: "write", Path: "private-body", Err: syscall.ENOSPC})
	if !strings.Contains(output.String(), `operation="materialize"`) || !strings.Contains(output.String(), "no space left on device") {
		t.Fatalf("missing task failure: %s", output.String())
	}
	persisted, err := m.readRecord("record")
	if err != nil || persisted.State != RepositoryFailed {
		t.Fatalf("failure state = %+v, %v", persisted, err)
	}
	output.Reset()
	record.State = RepositoryDeleting
	m.records["record"] = record
	dir := filepath.Join(m.root, "repositories", "project", "arch", "history", "build")
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".trash"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	m.remove("record")
	if !strings.Contains(output.String(), `operation="delete"`) || !strings.Contains(output.String(), "filesystem") {
		t.Fatalf("missing delete failure: %s", output.String())
	}
	output.Reset()
	if err := os.Remove(m.metaPath("record")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.metaPath("record"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := m.persist(record); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !strings.Contains(output.String(), `operation="persist"`) || !strings.Contains(output.String(), "project=\"project\"") {
		t.Fatalf("missing persistence failure: %s", output.String())
	}
	if strings.Contains(output.String(), m.root) || strings.Contains(output.String(), "private-body") {
		t.Fatalf("sensitive data in log: %s", output.String())
	}
}
