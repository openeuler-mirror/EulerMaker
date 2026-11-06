package release

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
)

func writePreparedRelease(t *testing.T, m *Manager, record *ReleaseRecord) {
	t.Helper()
	path := m.Path(record)
	if err := os.MkdirAll(filepath.Join(path, "repodata"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "repodata/repomd.xml"), []byte("metadata"), 0640); err != nil {
		t.Fatal(err)
	}
	digest, err := digestReleaseDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	record.ReleaseDigest = digest
	if err := fileutil.AtomicJSON(filepath.Join(path, ".release.json"), releaseIndex{
		BuildName: record.BuildName, RequestDigest: record.RequestDigest, ReleaseDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
}

func waitReleaseState(t *testing.T, m *Manager, name string, state ReleaseState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		record, ok, err := m.Get(name)
		m.mu.RLock()
		idle := !m.running[name] && !m.queued[name]
		m.mu.RUnlock()
		if err == nil && ok && idle && record.State == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("release %s did not reach %s", name, state)
}

func TestReleaseActivationRetriesAtRuntime(t *testing.T) {
	for _, failure := range []string{"switch-current", "persist-ready"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			m := &Manager{
				root: t.TempDir(), records: map[string]*ReleaseRecord{}, ctx: ctx,
				queued: map[string]bool{}, running: map[string]bool{}, queue: make(chan string, 1),
			}
			record := &ReleaseRecord{Project: "project", TargetArch: "arch", BuildName: "build", State: ReleasePrepared}
			writePreparedRelease(t, m, record)
			if err := m.persist(record); err != nil {
				t.Fatal(err)
			}
			current := filepath.Join(m.releaseTargetDir(record), "current")
			blocked := current
			if failure == "persist-ready" {
				blocked = m.metaPath(record.BuildName)
				if err := os.Rename(blocked, blocked+"-saved"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(blocked, 0750); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				m.retryPending()
			}
			if len(m.queue) != 1 {
				t.Fatalf("activation queue was not deduplicated: %d", len(m.queue))
			}
			workerDone, retryDone := make(chan struct{}), make(chan struct{})
			go func() { defer close(workerDone); m.worker() }()
			go func() { defer close(retryDone); m.recoverQueue() }()
			t.Cleanup(func() { cancel(); <-workerDone; <-retryDone })
			waitReleaseState(t, m, record.BuildName, ReleasePrepared)
			got, _, err := m.Get(record.BuildName)
			if err != nil {
				t.Fatal(err)
			}
			response := ResponseFor(got)
			if response.State != ReleaseCreating || response.PollAfterSeconds != 5 || response.ContentURL != "" {
				t.Fatalf("activation failure response: %+v", response)
			}
			if failure == "persist-ready" {
				if target, err := os.Readlink(current); err != nil || target != "releases/build" {
					t.Fatalf("current before Ready persistence: %q, %v", target, err)
				}
			}
			if err := os.Remove(blocked); err != nil {
				t.Fatal(err)
			}
			if failure == "persist-ready" {
				if err := os.Rename(blocked+"-saved", blocked); err != nil {
					t.Fatal(err)
				}
			}
			// No POST or GET side effect is needed: the periodic scanner retries activation only.
			waitReleaseState(t, m, record.BuildName, ReleaseReady)
			if target, err := os.Readlink(current); err != nil || target != "releases/build" {
				t.Fatalf("current after retry: %q, %v", target, err)
			}
		})
	}
}

func TestReleaseActivationResumesAfterRestart(t *testing.T) {
	for _, stage := range []string{"prepared", "directory-committed", "current-switched", "superseded", "stopped"} {
		t.Run(stage, func(t *testing.T) {
			m := &Manager{root: t.TempDir(), records: map[string]*ReleaseRecord{}}
			record := &ReleaseRecord{
				Project: "project", TargetArch: "arch", BuildName: "build", State: ReleasePrepared, RequestDigest: "request",
			}
			writePreparedRelease(t, m, record)
			current := filepath.Join(m.releaseTargetDir(record), "current")
			switch stage {
			case "directory-committed":
				record.State, record.ReleaseDigest = ReleaseCreating, ""
			case "current-switched":
				if err := os.Symlink("releases/build", current); err != nil {
					t.Fatal(err)
				}
			case "superseded":
				record.State = ReleaseReady
				if err := os.Symlink("releases/newer-build", current); err != nil {
					t.Fatal(err)
				}
			case "stopped":
				if err := os.MkdirAll(filepath.Join(m.root, "repositories/project/.stopped/build"), 0750); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.persist(record); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewManager(m.root, Config{
				Workers: 1, QueueCapacity: 1, WorkTTL: time.Hour, HistoryTTL: time.Hour, HistoryCount: 2,
			}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Stop()
			if stage == "stopped" {
				waitReleaseState(t, restarted, record.BuildName, ReleasePrepared)
				if _, err := os.Lstat(current); !os.IsNotExist(err) {
					t.Fatalf("stopped release was activated: %v", err)
				}
				return
			}
			waitReleaseState(t, restarted, record.BuildName, ReleaseReady)
			want := "releases/build"
			if stage == "superseded" {
				want = "releases/newer-build"
			}
			if target, err := os.Readlink(current); err != nil || target != want {
				t.Fatalf("current after restart: %q, %v; want %q", target, err, want)
			}
		})
	}
}

func TestPreparedReleaseRecoveryRejectsInvalidContent(t *testing.T) {
	m := &Manager{root: t.TempDir(), records: map[string]*ReleaseRecord{}}
	record := &ReleaseRecord{Project: "project", TargetArch: "arch", BuildName: "build", State: ReleasePrepared}
	writePreparedRelease(t, m, record)
	if err := m.persist(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Path(record), "repodata/repomd.xml"), []byte("corrupt"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := m.recover(time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _, err := m.Get(record.BuildName)
	if err != nil || got.State != ReleaseFailed || got.Failure.Code != "ReleaseContentInvalid" {
		t.Fatalf("recovered corrupt release: %+v, %v", got, err)
	}
}
