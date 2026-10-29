package upload

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"artifact-manager/pkg/fileutil"
)

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
		s.publishLocked(k, LogEvent{Sequence: int64(i)})
	}
	// A slow subscriber is already closed by publishLocked; cleanup remains safe.
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
