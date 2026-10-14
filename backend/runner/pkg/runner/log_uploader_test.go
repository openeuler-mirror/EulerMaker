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
	appended       chan struct{}
	appendFailures int
	completed      CompleteLogInput
}

func (f *fakeLogRemote) LogStatus(context.Context, string, string) (LogStatus, error) {
	return LogStatus{State: "Open"}, nil
}
func (f *fakeLogRemote) AppendLog(_ context.Context, _, _ string, sequence int64, _ string, data []byte) (AppendLogResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendFailures > 0 {
		f.appendFailures--
		return AppendLogResult{}, errors.New("temporary network error")
	}
	f.chunks = append(f.chunks, recordedChunk{sequence: sequence, data: append([]byte(nil), data...)})
	if f.appended != nil {
		select {
		case f.appended <- struct{}{}:
		default:
		}
	}
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
	if _, err := os.Stat(filepath.Join(factory.RootDir, "logs", "project", "build", "completed.json")); err != nil {
		t.Fatalf("completed receipt is missing: %v", err)
	}
	if err := factory.Cleanup(JobResource{Metadata: ObjectMeta{Name: "build", Namespace: "project", UID: "uid-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(factory.RootDir, "logs", "project", "build")); !os.IsNotExist(err) {
		t.Fatalf("completed spool was not cleaned: %v", err)
	}
}

func TestArtifactLogSinkFlushesPartialChunkBeforeCompletion(t *testing.T) {
	remote := &fakeLogRemote{appended: make(chan struct{}, 1)}
	factory := &ArtifactLogFactory{
		Remote: remote, RootDir: t.TempDir(), ChunkSize: 256, FlushInterval: 10 * time.Millisecond,
		SpoolLimit: 1024,
	}
	sink, err := factory.Open(JobResource{Metadata: ObjectMeta{Name: "build", Namespace: "project"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Abort()
	if _, err := sink.Write([]byte("short log")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-remote.appended:
	case <-time.After(time.Second):
		t.Fatal("partial log was not uploaded before completion")
	}
	remote.mu.Lock()
	chunks := append([]recordedChunk(nil), remote.chunks...)
	remote.mu.Unlock()
	if len(chunks) != 1 || string(chunks[0].data) != "short log" {
		t.Fatalf("uploaded chunks = %#v", chunks)
	}
	if _, err := sink.Write([]byte(" more")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-remote.appended:
	case <-time.After(time.Second):
		t.Fatal("second partial log was not uploaded before completion")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sink.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if len(remote.chunks) != 2 || remote.chunks[0].sequence != 0 || remote.chunks[1].sequence != 1 || string(remote.chunks[1].data) != " more" {
		t.Fatalf("uploaded chunks = %#v", remote.chunks)
	}
	if remote.completed.LastSequence != 1 || remote.completed.Size != int64(len("short log more")) {
		t.Fatalf("complete input = %#v", remote.completed)
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

func TestArtifactLogFactoryUsesJobNameAcrossUIDChanges(t *testing.T) {
	factory := &ArtifactLogFactory{Remote: &fakeLogRemote{}, RootDir: t.TempDir()}
	first := JobResource{Metadata: ObjectMeta{Name: "build", Namespace: "project", UID: "uid-1"}}
	sink, err := factory.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	sink.Abort()
	second := JobResource{Metadata: ObjectMeta{Name: "build", Namespace: "project", UID: "uid-2"}}
	next, err := factory.Open(second)
	if err != nil {
		t.Fatal(err)
	}
	next.Abort()
}
