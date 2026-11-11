package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"artifact-manager/pkg/fileutil"
)

func TestLogOperationsUseJobLock(t *testing.T) {
	for _, operation := range []string{"append", "query", "content", "replay", "complete", "manifest", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendLog("project", "job", "runner", 0, []byte("log"), hashText("log")); err != nil {
				t.Fatal(err)
			}
			var manifest CompleteManifestRequest
			if operation == "manifest" {
				a, err := s.CompleteLog("project", "job", "runner", CompleteLogRequest{
					Stream: "combined", LastSequence: 0, Size: 3, SHA256: hashText("log"),
				})
				if err != nil {
					t.Fatal(err)
				}
				manifest.Files = []ManifestFile{{
					ArtifactID: a.ID, RelativePath: a.RelativePath, Category: a.Category, Size: a.Size, SHA256: a.SHA256,
				}}
			}
			var once sync.Once
			unlock := s.lockJob("project", "job")
			release := func() { once.Do(unlock) }
			defer release()
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "append":
					_, err = s.AppendLog("project", "job", "runner", 1, []byte("more"), hashText("more"))
				case "query":
					if _, ok := s.GetLog("project", "job"); !ok {
						err = fmt.Errorf("log missing")
					}
				case "content":
					var f *os.File
					f, _, err = s.OpenLog("project", "job")
					if f != nil {
						f.Close()
					}
				case "replay":
					_, _, err = s.ReplayLog("project", "job", -1, 10)
				case "complete":
					_, err = s.CompleteLog("project", "job", "runner", CompleteLogRequest{
						Stream: "combined", LastSequence: 0, Size: 3, SHA256: hashText("log"),
					})
				case "manifest":
					_, err = s.CompleteManifest(context.Background(), "project", "job", "runner", manifest)
				case "delete":
					err = s.DeleteJob("project", "job")
				}
				done <- err
			}()
			waitJobLockRefs(t, s, "project", "job", 2)
			select {
			case err := <-done:
				t.Fatalf("operation bypassed Job lock: %v", err)
			default:
			}
			// The same Job name in a different Project must remain independent.
			other := make(chan error, 1)
			go func() {
				_, err := s.AppendLog("other-project", "job", "runner", 0, []byte("log"), hashText("log"))
				if err == nil {
					_, err = s.CompleteLog("other-project", "job", "runner", CompleteLogRequest{
						Stream: "combined", LastSequence: 0, Size: 3, SHA256: hashText("log"),
					})
				}
				other <- err
			}()
			waitLogOperation(t, other)
			release()
			waitLogOperation(t, done)
			s.mu.RLock()
			locks := len(s.jobs)
			s.mu.RUnlock()
			if locks != 0 {
				t.Fatalf("idle Job locks retained: %d", locks)
			}
		})
	}
}

func TestOpenLogSnapshotSurvivesAppendCompletionAndDeletion(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendLog("project", "job", "runner", 0, []byte("first"), hashText("first")); err != nil {
		t.Fatal(err)
	}
	f, snapshot, err := s.OpenLog("project", "job")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := s.AppendLog("project", "job", "runner", 1, []byte("next"), hashText("next")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteLog("project", "job", "runner", CompleteLogRequest{
		Stream: "combined", LastSequence: 1, Size: 9, SHA256: hashText("firstnext"),
	}); err != nil {
		t.Fatal(err)
	}
	completed, state, err := s.OpenLog("project", "job")
	if err != nil {
		t.Fatal(err)
	}
	defer completed.Close()
	if err := s.DeleteJob("project", "job"); err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(io.NewSectionReader(f, 0, snapshot.CommittedBytes))
	if err != nil || string(content) != "first" || snapshot.NextSequence != 1 {
		t.Fatalf("open snapshot: %q, %+v, %v", content, snapshot, err)
	}
	content, err = io.ReadAll(io.NewSectionReader(completed, 0, state.CommittedBytes))
	if err != nil || string(content) != "firstnext" || state.State != LogCompleted {
		t.Fatalf("completed snapshot: %q, %+v, %v", content, state, err)
	}
}

func TestReplayLogDuringAppendAndCompletion(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendLog("project", "job", "runner", 0, []byte("log"), hashText("log")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for seq := int64(1); seq < 16; seq++ {
			if _, err := s.AppendLog("project", "job", "runner", seq, []byte("log"), hashText("log")); err != nil {
				done <- err
				return
			}
		}
		_, err := s.CompleteLog("project", "job", "runner", CompleteLogRequest{
			Stream: "combined", LastSequence: 15, Size: 48, SHA256: hashText(string(bytes.Repeat([]byte("log"), 16))),
		})
		done <- err
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		events, next, err := s.ReplayLog("project", "job", -1, 32)
		if err != nil || int64(len(events)) != next {
			t.Fatalf("replay crossed snapshot boundary: events=%d next=%d err=%v", len(events), next, err)
		}
		for i, event := range events {
			if event.Sequence != int64(i) || string(event.Data) != "log" {
				t.Fatalf("invalid committed event: %+v", event)
			}
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-deadline.C:
			t.Fatal("append or completion blocked by replay")
		default:
		}
	}
}

func waitJobLockRefs(t *testing.T, s *Store, project, job string, count int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.RLock()
		lock := s.jobs[logKey(project, job, "")]
		ready := lock != nil && lock.refs == count
		s.mu.RUnlock()
		if ready {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("operation did not acquire a Job lock reference")
		case <-tick.C:
		}
	}
}

func waitLogOperation(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Job operation blocked")
	}
}

func TestConcurrentLogRetriesAndSubscriptions(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const jobs = 8
	done := make(chan error, jobs)
	for i := range jobs {
		go func() {
			job := fmt.Sprintf("job-%d", i)
			ch, unsubscribe := s.Subscribe("project", job)
			defer unsubscribe()
			for seq := range int64(3) {
				// Concurrent retries must commit exactly one body/index entry per sequence.
				attempts := make(chan error, 4)
				for range 4 {
					go func() {
						_, err := s.AppendLog("project", job, "runner", seq, []byte("log"), hashText("log"))
						attempts <- err
					}()
				}
				for range 4 {
					if err := <-attempts; err != nil {
						done <- err
						return
					}
				}
			}
			_, err := s.CompleteLog("project", job, "runner", CompleteLogRequest{
				Stream: "combined", LastSequence: 2, Size: 9, SHA256: hashText("logloglog"),
			})
			if err == nil {
				events, next, replayErr := s.ReplayLog("project", job, -1, 10)
				if replayErr != nil || next != 3 || len(events) != 3 || len(ch) != 4 {
					err = fmt.Errorf("replay/events for %s: next=%d events=%d live=%d err=%v", job, next, len(events), len(ch), replayErr)
				}
			}
			unsubscribe()
			if err == nil {
				err = s.DeleteJob("project", job)
			}
			done <- err
		}()
	}
	for range jobs {
		waitLogOperation(t, done)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.jobs) != 0 || len(s.logRecovery) != 0 || len(s.subscribers) != 0 {
		t.Fatalf("retained state: locks=%d recovery=%d subscribers=%d", len(s.jobs), len(s.logRecovery), len(s.subscribers))
	}
}

func TestJobLogLayoutSurvivesRestartAndCompletion(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello")
	if _, err := s.AppendLog("project", "job", "runner", 0, data, hashText(string(data))); err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(root, "artifacts/project/jobs/job")
	for _, relative := range []string{"logs/combined.log", "logs/.combined.index.jsonl", "logs/.combined.json"} {
		if _, err := os.Stat(filepath.Join(jobDir, relative)); err != nil {
			t.Fatal(err)
		}
	}
	// Startup recovery must discard a body tail that has no committed index entry.
	body := filepath.Join(jobDir, "logs/combined.log")
	if err := os.WriteFile(body, []byte("hellouncommitted"), 0640); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(body); err != nil || !bytes.Equal(content, data) {
		t.Fatalf("recovered log body: %q, %v", content, err)
	}
	if _, err := s.AppendLog("project", "job", "runner", 1, data, hashText(string(data))); err != nil {
		t.Fatal(err)
	}
	a, err := s.CompleteLog("project", "job", "runner", CompleteLogRequest{
		Stream: "combined", LastSequence: 1, Size: 10, SHA256: hashText("hellohello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.ArtifactPath(a) != filepath.Join(jobDir, "logs/container.log") {
		t.Fatalf("completed log path: %s", s.ArtifactPath(a))
	}
	if _, err := os.Stat(filepath.Join(jobDir, "logs/combined.log")); !os.IsNotExist(err) {
		t.Fatalf("active log was not moved: %v", err)
	}
	s, err = NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := s.ReplayLog("project", "job", -1, 10)
	if err != nil || len(events) != 2 || !bytes.Equal(events[0].Data, data) || !bytes.Equal(events[1].Data, data) {
		t.Fatalf("replay completed log after restart: %+v, %v", events, err)
	}
	artifacts, err := s.ListArtifacts("project", "job", "")
	if err != nil || len(artifacts) != 1 || artifacts[0].RelativePath != "logs/container.log" {
		t.Fatalf("downloadable log artifacts: %+v, %v", artifacts, err)
	}
}

func TestUploadReservesManagedLogPaths(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"logs", "logs/combined.log", "logs/container.log", "logs/combined.log/child",
		"logs/container.log/child", "packages/../logs/combined.log",
		"logs/.combined.json", "logs/.combined.index.jsonl", "logs/.combined.json/child",
		"logs/.combined.index.jsonl/child", "packages/../logs/.combined.json", "logs/.combined.json.tmp-test",
	} {
		t.Run(path, func(t *testing.T) {
			_, _, err := s.BeginUpload("project", "job", "key", UploadMetadata{
				Category: CategoryLog, FileName: filepath.Base(path), RelativePath: path,
			}, 1024)
			if err == nil || err.Error() != "reserved log path" {
				t.Fatalf("upload into managed log path: %v", err)
			}
		})
	}
	if _, _, err := s.BeginUpload("project", "job", "custom", UploadMetadata{
		Category: CategoryLog, FileName: "custom.log", RelativePath: "logs/custom.log",
	}, 1024); err != nil {
		t.Fatalf("upload custom log: %v", err)
	}
}

func TestLogMetadataReadOnDemand(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte("hello")
	l, err := s.AppendLog("project", "job", "runner", 0, chunk, hashText(string(chunk)))
	if err != nil {
		t.Fatal(err)
	}
	_, _, meta := s.LogPaths("project", "job")
	l.RunnerName = "replacement-runner"
	if err := fileutil.AtomicJSON(meta, l); err != nil {
		t.Fatal(err)
	}
	loaded, ok := s.GetLog("project", "job")
	if !ok || loaded.RunnerName != l.RunnerName {
		t.Fatalf("disk metadata: %+v", loaded)
	}
	if _, err := s.AppendLog("project", "job", l.RunnerName, 1, chunk, hashText(string(chunk))); err != nil {
		t.Fatal(err)
	}
	complete := CompleteLogRequest{Stream: "combined", LastSequence: 1, Size: 10, SHA256: hashText("hellohello")}
	a, err := s.CompleteLog("project", "job", l.RunnerName, complete)
	if err != nil {
		t.Fatal(err)
	}
	finalizing, ok := s.GetLog("project", "job")
	if !ok {
		t.Fatal("completed log metadata missing")
	}
	finalizing.State, finalizing.CompletedAt = LogFinalizing, nil
	if err := fileutil.AtomicJSON(meta, finalizing); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.CompleteLog("project", "job", l.RunnerName, complete)
	if err != nil || replayed.ID != a.ID {
		t.Fatalf("completion replay: %+v, %v", replayed, err)
	}
	if len(s.logRecovery) != 0 {
		t.Fatalf("successful operations retained recovery markers: %v", s.logRecovery)
	}
}

func TestRestartWithMissingCompletedLogArtifact(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("completed log")
	if _, err := store.AppendLog("project", "job", "runner", 0, data, hashText(string(data))); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CompleteLog("project", "job", "runner", CompleteLogRequest{
		Stream: "combined", LastSequence: 0, Size: int64(len(data)), SHA256: hashText(string(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.ArtifactPath(artifact)); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		store, err = NewStore(root)
		if err != nil {
			t.Fatalf("restart %d: %v", attempt+1, err)
		}
		log, ok := store.GetLog("project", "job")
		if !ok || log.State != LogFailed || log.Failure == nil || log.Failure.Code != "Corrupted" {
			t.Fatalf("restart %d log: %+v", attempt+1, log)
		}
	}
}

func TestRestartWithMissingOpenLogBody(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("open log")
	if _, err := store.AppendLog("project", "job", "runner", 0, data, hashText(string(data))); err != nil {
		t.Fatal(err)
	}
	body, _, _ := store.LogPaths("project", "job")
	if err := os.Remove(body); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		store, err = NewStore(root)
		if err != nil {
			t.Fatalf("restart %d: %v", attempt+1, err)
		}
		log, ok := store.GetLog("project", "job")
		if !ok || log.State != LogFailed || log.Failure == nil || log.Failure.Code != "Corrupted" {
			t.Fatalf("restart %d log: %+v", attempt+1, log)
		}
	}
}

func TestLogAppendRecoversInterruptedWrite(t *testing.T) {
	for _, stage := range []string{"body", "partial-index", "index", "empty-index"} {
		t.Run(stage, func(t *testing.T) {
			s, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			first, next := []byte("first"), []byte("next")
			if stage == "empty-index" {
				next = nil
			}
			l, err := s.AppendLog("project", "job", "runner", 0, first, hashText(string(first)))
			if err != nil {
				t.Fatal(err)
			}
			body, index, _ := s.LogPaths("project", "job")
			appendTestFile(t, body, next)
			record, err := json.Marshal(LogChunkRecord{
				Sequence:    1,
				StartOffset: int64(len(first)),
				Size:        int64(len(next)),
				SHA256:      hashText(string(next)),
			})
			if err != nil {
				t.Fatal(err)
			}
			switch stage {
			case "partial-index":
				appendTestFile(t, index, record[:len(record)/2])
			case "index", "empty-index":
				appendTestFile(t, index, append(record, '\n'))
			}
			// The request failed after writing the body/index but before committing metadata.
			s.logRecovery[logKey("project", "job", "combined")] = struct{}{}
			l, err = s.AppendLog("project", "job", "runner", 1, next, hashText(string(next)))
			if err != nil || l.NextSequence != 2 || l.CommittedBytes != int64(len(first)+len(next)) {
				t.Fatalf("retry: %+v, %v", l, err)
			}
			content, err := os.ReadFile(body)
			if err != nil || !bytes.Equal(content, append(first, next...)) {
				t.Fatalf("body after retry: %q, %v", content, err)
			}
			events, _, err := s.ReplayLog("project", "job", -1, 10)
			if err != nil || len(events) != 2 || !bytes.Equal(events[1].Data, next) {
				t.Fatalf("replay: %+v, %v", events, err)
			}
		})
	}
}

func TestLogAppendRetriesAfterIndexWriteFailure(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, index, _ := s.LogPaths("project", "job")
	if err := os.MkdirAll(index, 0750); err != nil {
		t.Fatal(err)
	}
	data := []byte("first chunk")
	if _, err := s.AppendLog("project", "job", "runner", 0, data, hashText(string(data))); err == nil {
		t.Fatal("expected index write failure")
	}
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	l, err := s.AppendLog("project", "job", "runner", 0, data, hashText(string(data)))
	if err != nil || l.NextSequence != 1 {
		t.Fatalf("retry first chunk: %+v, %v", l, err)
	}
	content, err := os.ReadFile(body)
	if err != nil || !bytes.Equal(content, data) {
		t.Fatalf("body after retry: %q, %v", content, err)
	}
}

func TestLogSubscriberCleanup(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ch, unsubscribe := s.Subscribe("project", "job")
	k := logKey("project", "job", "combined")
	for i := 0; i <= cap(ch); i++ {
		s.publish(k, LogEvent{Sequence: int64(i)})
	}
	// A slow subscriber is already closed by publish; cleanup remains safe.
	unsubscribe()
	unsubscribe()
	if len(s.subscribers) != 0 {
		t.Fatalf("subscriber entries retained: %d", len(s.subscribers))
	}
	_, unsubscribe = s.Subscribe("project", "job")
	unsubscribe()
	if len(s.subscribers) != 0 {
		t.Fatalf("subscriber entries retained after disconnect: %d", len(s.subscribers))
	}
}

func appendTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append test file: write=%v close=%v", writeErr, closeErr)
	}
}
