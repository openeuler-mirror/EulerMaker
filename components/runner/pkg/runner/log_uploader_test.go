package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordedChunk struct {
	sequence int64
	data     []byte
}
type fakeLogRemote struct {
	mu             sync.Mutex
	chunks         []recordedChunk
	appendFailures int
	completed      CompleteLogInput
}

func (f *fakeLogRemote) LogStatus(context.Context, string, string, string) (LogStatus, error) {
	return LogStatus{State: "Open"}, nil
}
func (f *fakeLogRemote) AppendLog(_ context.Context, _, _, _ string, sequence int64, _ string, data []byte) (AppendLogResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendFailures > 0 {
		f.appendFailures--
		return AppendLogResult{}, errors.New("temporary network error")
	}
	f.chunks = append(f.chunks, recordedChunk{sequence: sequence, data: append([]byte(nil), data...)})
	return AppendLogResult{AcceptedSequence: sequence, NextSequence: sequence + 1}, nil
}
func (f *fakeLogRemote) CompleteLog(_ context.Context, _, _, _ string, input CompleteLogInput) (CompletedLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = input
	return CompletedLog{State: "Completed", ArtifactID: "log-1", Size: input.Size, SHA256: input.SHA256}, nil
}

func TestArtifactLogSinkUploadsChunksAndRemovesSpool(t *testing.T) {
	remote := &fakeLogRemote{appendFailures: 1}
	factory := &ArtifactLogFactory{Remote: remote, RootDir: t.TempDir(), ChunkSize: 4, FlushInterval: 10 * time.Millisecond, SpoolLimit: 1024, RetryMaxBackoff: 10 * time.Millisecond}
	sink, err := factory.Open(JobResource{Metadata: ObjectMeta{Name: "build", Namespace: "project", UID: "uid-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("abcdefghij")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	completed, err := sink.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != "Completed" {
		t.Fatalf("completed = %#v", completed)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if len(remote.chunks) != 3 {
		t.Fatalf("chunks = %#v", remote.chunks)
	}
	if string(remote.chunks[0].data)+string(remote.chunks[1].data)+string(remote.chunks[2].data) != "abcdefghij" {
		t.Fatalf("unexpected chunks: %#v", remote.chunks)
	}
	for i, chunk := range remote.chunks {
		if chunk.sequence != int64(i) {
			t.Fatalf("sequence %d = %d", i, chunk.sequence)
		}
	}
	if remote.completed.LastSequence != 2 || remote.completed.Size != 10 {
		t.Fatalf("complete input = %#v", remote.completed)
	}
	if _, err := os.Stat(filepath.Join(factory.RootDir, "logs", "project", "uid-1", "completed.json")); err != nil {
		t.Fatalf("completed receipt is missing: %v", err)
	}
	if err := factory.Cleanup(JobResource{Metadata: ObjectMeta{Namespace: "project", UID: "uid-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(factory.RootDir, "logs", "project", "uid-1")); !os.IsNotExist(err) {
		t.Fatalf("completed spool was not cleaned: %v", err)
	}
}

func TestArtifactLogSinkEnforcesSpoolLimit(t *testing.T) {
	factory := &ArtifactLogFactory{Remote: &fakeLogRemote{}, RootDir: t.TempDir(), ChunkSize: 4, FlushInterval: time.Hour, SpoolLimit: 5}
	sink, err := factory.Open(JobResource{Metadata: ObjectMeta{Name: "build", UID: "uid-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("123456")); err == nil {
		t.Fatal("expected spool limit error")
	}
	sink.Abort()
}
