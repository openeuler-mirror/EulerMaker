package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"ebs-runner/pkg/gateway"
)

type recordedChunk struct {
	sequence int64
	data     []byte
}

func TestCompletedRemoteLogRecoveryChecksSpool(t *testing.T) {
	for _, match := range []bool{true, false} {
		root := t.TempDir()
		dir := filepath.Join(root, "logs", "project", "job")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "combined.log")
		if err := os.WriteFile(path, []byte("finished log\n"), 0600); err != nil {
			t.Fatal(err)
		}
		sum, size, err := hashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !match {
			sum = "different"
		}
		remote := &fakeLogRemote{status: &LogStatus{State: "Completed", CommittedBytes: size, FinalSize: &size, FinalSHA256: sum, NextSequence: 1}}
		factory := &LogFactory{Remote: remote, RootDir: root}
		sink, err := factory.Open(gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "job", Namespace: "project", UID: "uid"}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = sink.Complete(ctx)
		cancel()
		sink.Abort()
		if (err == nil) != match {
			t.Fatalf("match=%v err=%v", match, err)
		}
		if len(remote.chunks) != 0 {
			t.Fatal("appended to sealed log")
		}
	}
}

type fakeLogRemote struct {
	mu             sync.Mutex
	chunks         []recordedChunk
	appended       chan struct{}
	appendFailures int
	appendError    error
	completed      CompleteLogInput
	status         *LogStatus
	beforeAppend   func(int64)
}

func (f *fakeLogRemote) LogStatus(context.Context, string, string) (LogStatus, error) {
	if f.status != nil {
		return *f.status, nil
	}
	return LogStatus{State: "Open"}, nil
}

func (f *fakeLogRemote) AppendLog(_ context.Context, _, _ string, sequence int64, _ string, data []byte) (AppendLogResult, error) {
	if f.beforeAppend != nil {
		f.beforeAppend(sequence)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendFailures > 0 {
		f.appendFailures--
		if f.appendError != nil {
			return AppendLogResult{}, f.appendError
		}
		return AppendLogResult{}, io.ErrUnexpectedEOF
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

func TestArtifactRetryClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"network", &url.Error{Op: "Post", URL: "http://artifacts", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, true},
		{"lost response", io.EOF, true},
		{"truncated response", fmt.Errorf("decode response: %w", io.ErrUnexpectedEOF), true},
		{"unavailable", APIError{StatusCode: 503}, true},
		{"rate limit", APIError{StatusCode: 429}, true},
		{"upload in progress", APIError{StatusCode: 409, Code: "UploadInProgress"}, true},
		{"retryable conflict", APIError{StatusCode: 409, Code: "ManifestInProgress", Retryable: true}, true},
		{"token exchange unavailable", fmt.Errorf("get token: %w", gateway.StatusError{Code: 503}), true},
		{"token exchange rate limit", gateway.StatusError{Code: 429}, true},
		{"token rejected", gateway.StatusError{Code: 401}, false},
		{"metadata conflict", APIError{StatusCode: 409, Code: "IdempotencyConflict"}, false},
		{"path conflict", APIError{StatusCode: 409, Code: "ArtifactPathConflict"}, false},
		{"unknown conflict", APIError{StatusCode: 409}, false},
		{"unauthorized", APIError{StatusCode: 401}, false},
		{"invalid input", APIError{StatusCode: 422}, false},
		{"missing file", &os.PathError{Op: "open", Path: "rpm", Err: os.ErrNotExist}, false},
		{"file access denied", &os.PathError{Op: "open", Path: "rpm", Err: os.ErrPermission}, false},
		{"local read failure", &os.PathError{Op: "read", Path: "rpm", Err: io.ErrUnexpectedEOF}, false},
		{"invalid URL", &url.Error{Op: "Post", Err: errors.New("unsupported protocol")}, false},
		{"invalid response", &json.SyntaxError{}, false},
		{"unknown local error", errors.New("destination not pinned"), false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"no error", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableArtifactError(test.err); got != test.want {
				t.Fatalf("retry=%v, want %v for %v", got, test.want, test.err)
			}
		})
	}
	for _, err := range []error{
		APIError{StatusCode: 429, RetryAfter: time.Second},
		gateway.StatusError{Code: 429, RetryAfter: time.Second},
	} {
		if got := retryDelay(err, time.Millisecond); got != time.Second {
			t.Fatalf("Retry-After ignored: %s", got)
		}
		if got := retryDelay(err, 2*time.Second); got != 2*time.Second {
			t.Fatalf("backoff shortened: %s", got)
		}
	}
}

func TestLogAppendRetriesRateLimitWithoutRetryableFlag(t *testing.T) {
	remote := &fakeLogRemote{appendFailures: 1, appendError: APIError{StatusCode: 429}}
	sink := &artifactLogSink{remote: remote, project: "project", job: "job", retryMaxBackoff: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sink.appendWithRetry(ctx, 3, "sum", []byte("log")); err != nil {
		t.Fatal(err)
	}
	if len(remote.chunks) != 1 || remote.chunks[0].sequence != 3 || string(remote.chunks[0].data) != "log" {
		t.Fatalf("replayed chunk=%+v", remote.chunks)
	}
}

func (f *fakeLogRemote) CompleteLog(_ context.Context, _, _, _ string, input CompleteLogInput) (CompletedLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = input
	return CompletedLog{State: "Completed", ArtifactID: "log-1", Size: input.Size, SHA256: input.SHA256}, nil
}

func TestArtifactLogSinkUploadsChunksAndRemovesSpool(t *testing.T) {
	remote := &fakeLogRemote{appendFailures: 1}
	factory := &LogFactory{Remote: remote, RootDir: t.TempDir(), ChunkSize: 4, FlushInterval: 10 * time.Millisecond, SpoolLimit: 1024, RetryMaxBackoff: 10 * time.Millisecond}
	sink, err := factory.Open(gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "build", Namespace: "project", UID: "uid-1"}})
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
	if err := factory.Cleanup(gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "build", Namespace: "project", UID: "uid-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(factory.RootDir, "logs", "project", "build")); !os.IsNotExist(err) {
		t.Fatalf("completed spool was not cleaned: %v", err)
	}
}

func TestArtifactLogSinkFlushesPartialChunkBeforeCompletion(t *testing.T) {
	remote := &fakeLogRemote{appended: make(chan struct{}, 1)}
	factory := &LogFactory{
		Remote: remote, RootDir: t.TempDir(), ChunkSize: 256, FlushInterval: 10 * time.Millisecond,
		SpoolLimit: 1024,
	}
	sink, err := factory.Open(gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "build", Namespace: "project"}})
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
	factory := &LogFactory{Remote: &fakeLogRemote{}, RootDir: t.TempDir(), ChunkSize: 4, FlushInterval: time.Hour, SpoolLimit: 5}
	sink, err := factory.Open(gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "build", UID: "uid-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("123456")); err == nil {
		t.Fatal("expected spool limit error")
	}
	sink.Abort()
}

func TestArtifactLogFactoryUsesJobNameAcrossUIDChanges(t *testing.T) {
	factory := &LogFactory{Remote: &fakeLogRemote{}, RootDir: t.TempDir()}
	first := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "build", Namespace: "project", UID: "uid-1"}}
	sink, err := factory.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	sink.Abort()
	second := gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "build", Namespace: "project", UID: "uid-2"}}
	next, err := factory.Open(second)
	if err != nil {
		t.Fatal(err)
	}
	next.Abort()
}

type observedLogSpool struct {
	*os.File
	syncs  int
	onSync func() error
}

func (f *observedLogSpool) Sync() error {
	f.syncs++
	if f.onSync != nil {
		if err := f.onSync(); err != nil {
			return err
		}
	}
	return f.File.Sync()
}

func newTestLogSink(t *testing.T, remote LogRemote) (*artifactLogSink, *observedLogSpool) {
	t.Helper()
	dir := t.TempDir()
	file, err := os.Create(filepath.Join(dir, "combined.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	index, err := os.Create(filepath.Join(dir, "chunks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	spool := &observedLogSpool{File: file}
	return &artifactLogSink{
		remote: remote, project: "project", job: "build", dir: dir, file: spool, index: index,
		chunkSize: 4, spoolLimit: 1024, flushInterval: time.Hour,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}, spool
}

func readLogCheckpoint(t *testing.T, dir string) logUploadCheckpoint {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "upload.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint logUploadCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func TestArtifactLogSinkSyncPersistsUnuploadedTail(t *testing.T) {
	remote := &fakeLogRemote{}
	sink, spool := newTestLogSink(t, remote)
	if _, err := sink.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Sync(); err != nil {
		t.Fatal(err)
	}
	if spool.syncs != 1 || sink.durable != 4 || len(remote.chunks) != 0 {
		t.Fatalf("syncs=%d durable=%d uploaded=%d", spool.syncs, sink.durable, len(remote.chunks))
	}
	if _, err := sink.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("disk sync failed")
	spool.onSync = func() error { return syncErr }
	if err := sink.Sync(); !errors.Is(err, syncErr) || sink.durable != 4 {
		t.Fatalf("sync error=%v durable=%d", err, sink.durable)
	}
}

func TestArtifactLogSinkSyncsBeforeIndexAndUpload(t *testing.T) {
	remote := &fakeLogRemote{}
	sink, spool := newTestLogSink(t, remote)
	var events []string
	spool.onSync = func() error {
		events = append(events, "sync")
		info, err := sink.index.Stat()
		if err != nil || info.Size() != 0 {
			t.Errorf("index was written before spool sync: info=%v err=%v", info, err)
		}
		return nil
	}
	remote.beforeAppend = func(sequence int64) {
		events = append(events, "upload")
		if sink.durable != 8 {
			t.Errorf("durable boundary before upload = %d, want 8", sink.durable)
		}
		data, err := os.ReadFile(filepath.Join(sink.dir, "chunks.jsonl"))
		if err != nil {
			t.Error(err)
		}
		var record logChunkRecord
		lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
		if err := json.Unmarshal(lines[len(lines)-1], &record); err != nil || record.Sequence != sequence {
			t.Errorf("index not ready before upload: record=%+v err=%v", record, err)
		}
	}
	for _, b := range []byte("abcdefgh") {
		if _, err := sink.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if spool.syncs != 0 {
		t.Fatalf("Write synchronized the spool %d times", spool.syncs)
	}
	if _, err := os.Stat(filepath.Join(sink.dir, "upload.json")); !os.IsNotExist(err) {
		t.Fatalf("Write created a checkpoint: %v", err)
	}
	sink.closing = true
	sink.run(context.Background())
	if sink.workerErr != nil {
		t.Fatal(sink.workerErr)
	}
	if !reflect.DeepEqual(events, []string{"sync", "upload", "upload"}) {
		t.Fatalf("events = %v", events)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sink.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	checkpoint := readLogCheckpoint(t, sink.dir)
	if checkpoint.ProducedBytes != 8 || checkpoint.ConfirmedOffset != 8 || checkpoint.State != "Completed" {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
}

func TestArtifactLogSinkSyncFailureStopsBeforeIndexAndUpload(t *testing.T) {
	remote := &fakeLogRemote{}
	sink, spool := newTestLogSink(t, remote)
	syncErr := errors.New("spool sync failed")
	spool.onSync = func() error { return syncErr }
	if _, err := sink.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	sink.closing = true
	sink.run(context.Background())
	if !errors.Is(sink.workerErr, syncErr) {
		t.Fatalf("worker error = %v", sink.workerErr)
	}
	info, err := sink.index.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || len(remote.chunks) != 0 || sink.durable != 0 {
		t.Fatalf("advanced after sync failure: index=%d chunks=%d durable=%d", info.Size(), len(remote.chunks), sink.durable)
	}
	checkpoint := readLogCheckpoint(t, sink.dir)
	if checkpoint.ProducedBytes != 0 || checkpoint.ConfirmedOffset != 0 {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
}

func TestArtifactLogSinkCheckpointExcludesWritesDuringUpload(t *testing.T) {
	remote := &fakeLogRemote{}
	sink, spool := newTestLogSink(t, remote)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote.beforeAppend = func(int64) {
		written := make(chan error, 1)
		go func() {
			_, err := sink.Write([]byte("e"))
			written <- err
		}()
		if err := <-written; err != nil {
			t.Error(err)
		}
		cancel()
	}
	if _, err := sink.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	sink.run(ctx)
	checkpoint := readLogCheckpoint(t, sink.dir)
	if checkpoint.ProducedBytes != 4 || checkpoint.ConfirmedOffset != 4 || checkpoint.NextSequence != 1 {
		t.Fatalf("checkpoint included unsynced data: %+v", checkpoint)
	}
	if sink.produced != 5 || spool.syncs != 1 {
		t.Fatalf("produced=%d syncs=%d", sink.produced, spool.syncs)
	}
}

func TestArtifactLogSinkResumesExistingSpool(t *testing.T) {
	remote := &fakeLogRemote{status: &LogStatus{State: "Open", CommittedBytes: 4, NextSequence: 1}}
	root := t.TempDir()
	dir := filepath.Join(root, "logs", "project", "build")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "combined.log"), []byte("abcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	factory := &LogFactory{Remote: remote, RootDir: root, ChunkSize: 4}
	sink, err := factory.Open(gateway.JobResource{Metadata: gateway.ObjectMeta{Name: "build", Namespace: "project"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Abort()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sink.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	if len(remote.chunks) != 1 || remote.chunks[0].sequence != 1 || string(remote.chunks[0].data) != "efgh" {
		t.Fatalf("resumed chunks = %+v", remote.chunks)
	}
	checkpoint := readLogCheckpoint(t, dir)
	if checkpoint.ProducedBytes != 8 || checkpoint.ConfirmedOffset != 8 || checkpoint.NextSequence != 2 {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
}
