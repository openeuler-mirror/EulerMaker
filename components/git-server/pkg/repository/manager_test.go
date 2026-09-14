package repository

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git-server/pkg/gitops"
	"git-server/pkg/storage"
)

type fakeGit struct {
	origin string
}

func (f *fakeGit) Clone(_ context.Context, remote gitops.Remote, destination, _ string) error {
	f.origin = remote.URL
	return os.WriteFile(filepath.Join(destination, "HEAD"), []byte("ref: refs/heads/main\n"), 0600)
}
func (f *fakeGit) Fetch(context.Context, gitops.Remote, string, string) error { return nil }
func (f *fakeGit) ValidateBare(context.Context, string) error                 { return nil }
func (f *fakeGit) Origin(context.Context, string) (string, error)             { return f.origin, nil }

func TestManagerSyncAndDelete(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeGit{}
	manager, err := NewManager(ctx, store, client, ManagerConfig{Workers: 1, MaxRetries: 1, RetryBase: time.Millisecond, RetryMax: time.Millisecond, CloneBaseURL: "git://git-server:9418", CleanupPeriod: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()

	origin := "https://example.com/team/repo.git"
	if _, err := manager.Sync(origin); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		response, found, _ := manager.Status(origin)
		return found && response.SyncTime != nil
	})
	if _, queued, err := manager.Delete(origin); err != nil || !queued {
		t.Fatalf("queued=%v err=%v", queued, err)
	}
	waitFor(t, func() bool {
		_, found, _ := manager.Status(origin)
		return !found
	})
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("manager did not stop")
	}
}

func TestManagerSyncCoalescesPendingRequest(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(context.Background(), store, &fakeGit{}, ManagerConfig{
		Workers: 1, MaxRetries: 1, RetryBase: time.Millisecond, RetryMax: time.Millisecond,
		CloneBaseURL: "git://git-server:9418", CleanupPeriod: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://example.com/team/repo.git"
	if _, err := manager.Sync(origin); err != nil {
		t.Fatal(err)
	}
	state := manager.states["example.com/team/repo.git"]
	revision := state.Revision
	state.RetryCount = 1
	state.Error = &RepositoryError{Code: "FetchFailed", Retryable: true}
	state.ResetBackoff = false

	response, err := manager.Sync(origin)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != revision || state.RetryCount != 1 || state.Error == nil || state.ResetBackoff {
		t.Fatalf("pending sync was reset: state=%+v", state)
	}
	if response.RetryCount != 1 || response.Error == nil {
		t.Fatalf("response=%+v", response)
	}
	if manager.queue.Len() != 1 {
		t.Fatalf("queue length=%d, want 1", manager.queue.Len())
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}
