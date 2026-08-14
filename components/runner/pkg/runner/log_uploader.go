package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type LogRemote interface {
	LogStatus(context.Context, string, string, string) (LogStatus, error)
	AppendLog(context.Context, string, string, string, int64, string, []byte) (AppendLogResult, error)
	CompleteLog(context.Context, string, string, string, CompleteLogInput) (CompletedLog, error)
}

type JobLogSink interface {
	io.Writer
	Complete(context.Context) (CompletedLog, error)
	Abort()
}

type JobLogSinkFactory interface {
	Open(JobResource) (JobLogSink, error)
}

type ArtifactLogFactory struct {
	Remote          LogRemote
	RootDir         string
	ChunkSize       int64
	FlushInterval   time.Duration
	SpoolLimit      int64
	RetryMaxBackoff time.Duration
}

type logChunkRecord struct {
	Sequence int64  `json:"sequence"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type logUploadCheckpoint struct {
	SchemaVersion   int       `json:"schemaVersion"`
	Project         string    `json:"project"`
	JobName         string    `json:"jobName"`
	JobUID          string    `json:"jobUID"`
	Stream          string    `json:"stream"`
	NextSequence    int64     `json:"nextSequence"`
	ConfirmedOffset int64     `json:"confirmedOffset"`
	ProducedBytes   int64     `json:"producedBytes"`
	State           string    `json:"state"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type artifactLogSink struct {
	remote                         LogRemote
	project, job, uid              string
	chunkSize, spoolLimit          int64
	flushInterval, retryMaxBackoff time.Duration
	dir                            string

	mu           sync.Mutex
	checkpointMu sync.Mutex
	file         *os.File
	index        *os.File
	produced     int64
	uploaded     int64
	nextSequence int64
	closing      bool
	closed       bool
	wake         chan struct{}
	done         chan struct{}
	cancel       context.CancelFunc
	workerErr    error
}

func (f *ArtifactLogFactory) Open(job JobResource) (JobLogSink, error) {
	if f.Remote == nil {
		return nil, fmt.Errorf("artifact log remote is required")
	}
	if job.Metadata.UID == "" {
		return nil, fmt.Errorf("job UID is required for log upload")
	}
	project := job.Metadata.Namespace
	if project == "" {
		project = "default"
	}
	dir := filepath.Join(f.RootDir, "logs", filepath.Clean(project), filepath.Clean(job.Metadata.UID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log spool: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "combined.log"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log spool: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	index, err := os.OpenFile(filepath.Join(dir, "chunks.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("open log index: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &artifactLogSink{remote: f.Remote, project: project, job: job.Metadata.Name, uid: job.Metadata.UID,
		chunkSize: f.ChunkSize, spoolLimit: f.SpoolLimit, flushInterval: f.FlushInterval,
		retryMaxBackoff: f.RetryMaxBackoff, dir: dir, file: file, index: index, produced: info.Size(),
		wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	if s.chunkSize <= 0 {
		s.chunkSize = 256 << 10
	}
	if s.spoolLimit <= 0 {
		s.spoolLimit = 4 << 30
	}
	if s.flushInterval <= 0 {
		s.flushInterval = 500 * time.Millisecond
	}
	if s.retryMaxBackoff <= 0 {
		s.retryMaxBackoff = 30 * time.Second
	}
	go s.run(ctx)
	return s, nil
}

func (s *artifactLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.closing || s.closed {
		s.mu.Unlock()
		return 0, errors.New("log sink is closed")
	}
	if s.produced+int64(len(p)) > s.spoolLimit {
		s.mu.Unlock()
		return 0, fmt.Errorf("log spool limit %d exceeded", s.spoolLimit)
	}
	n, err := s.file.Write(p)
	s.produced += int64(n)
	if err == nil {
		err = s.file.Sync()
	}
	s.mu.Unlock()
	if err == nil {
		err = s.writeCheckpoint()
	}
	if err == nil {
		s.signal()
	}
	return n, err
}

func (s *artifactLogSink) Complete(ctx context.Context) (CompletedLog, error) {
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		s.signal()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
	case <-ctx.Done():
		s.Abort()
		return CompletedLog{}, ctx.Err()
	}
	s.mu.Lock()
	err := s.workerErr
	s.mu.Unlock()
	if err != nil {
		return CompletedLog{}, err
	}

	sum, size, err := hashFile(filepath.Join(s.dir, "combined.log"))
	if err != nil {
		return CompletedLog{}, err
	}
	s.mu.Lock()
	last := s.nextSequence - 1
	s.mu.Unlock()
	key := s.uid + "-log-complete"
	result, err := s.completeWithRetry(ctx, key, CompleteLogInput{JobUID: s.uid, Stream: "combined", LastSequence: last, Size: size, SHA256: sum})
	if err != nil {
		return CompletedLog{}, err
	}
	s.closeFiles()
	if err := s.writeCheckpointState("Completed"); err != nil {
		return result, err
	}
	receipt, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if err := writeAtomicFile(filepath.Join(s.dir, "completed.json"), receipt); err != nil {
		return result, fmt.Errorf("write completed log receipt: %w", err)
	}
	return result, nil
}

func (f *ArtifactLogFactory) Cleanup(job JobResource) error {
	if job.Metadata.UID == "" {
		return nil
	}
	project := job.Metadata.Namespace
	if project == "" {
		project = "default"
	}
	dir := filepath.Join(f.RootDir, "logs", filepath.Clean(project), filepath.Clean(job.Metadata.UID))
	if _, err := os.Stat(filepath.Join(dir, "completed.json")); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(dir)
}

func (s *artifactLogSink) Abort() {
	s.cancel()
	<-s.done
	s.closeFiles()
}

func (s *artifactLogSink) run(ctx context.Context) {
	defer close(s.done)
	status, err := s.statusWithRetry(ctx)
	if err != nil {
		s.setWorkerErr(fmt.Errorf("query log status: %w", err))
		return
	}
	s.mu.Lock()
	if status.CommittedBytes > s.produced {
		s.workerErr = fmt.Errorf("remote log offset %d exceeds local spool %d", status.CommittedBytes, s.produced)
		s.mu.Unlock()
		return
	}
	s.uploaded, s.nextSequence = status.CommittedBytes, status.NextSequence
	s.mu.Unlock()
	if err := s.writeCheckpoint(); err != nil {
		s.setWorkerErr(err)
		return
	}

	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.setWorkerErr(ctx.Err())
			return
		case <-ticker.C:
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			available := s.produced - s.uploaded
			closing := s.closing
			offset, seq := s.uploaded, s.nextSequence
			s.mu.Unlock()
			if available == 0 {
				if closing {
					return
				}
				break
			}
			if available < s.chunkSize && !closing {
				break
			}
			size := available
			if size > s.chunkSize {
				size = s.chunkSize
			}
			data := make([]byte, size)
			if _, err := s.file.ReadAt(data, offset); err != nil {
				s.setWorkerErr(fmt.Errorf("read log spool: %w", err))
				return
			}
			h := sha256.Sum256(data)
			sum := hex.EncodeToString(h[:])
			record := logChunkRecord{Sequence: seq, Offset: offset, Size: size, SHA256: sum}
			encoded, _ := json.Marshal(record)
			if _, err := s.index.Write(append(encoded, '\n')); err != nil {
				s.setWorkerErr(err)
				return
			}
			if err := s.index.Sync(); err != nil {
				s.setWorkerErr(err)
				return
			}
			if err := s.appendWithRetry(ctx, seq, sum, data); err != nil {
				s.setWorkerErr(err)
				return
			}
			s.mu.Lock()
			s.uploaded += size
			s.nextSequence++
			s.mu.Unlock()
			if err := s.writeCheckpoint(); err != nil {
				s.setWorkerErr(err)
				return
			}
		}
	}
}

func (s *artifactLogSink) appendWithRetry(ctx context.Context, seq int64, sum string, data []byte) error {
	backoff := 200 * time.Millisecond
	for {
		_, err := s.remote.AppendLog(ctx, s.project, s.job, s.uid, seq, sum, data)
		if err == nil {
			return nil
		}
		var apiErr ArtifactAPIError
		if errors.As(err, &apiErr) && !apiErr.Retryable && apiErr.StatusCode < 500 {
			return err
		}
		wait := backoff
		if errors.As(err, &apiErr) && apiErr.RetryAfter > wait {
			wait = apiErr.RetryAfter
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
		if backoff > s.retryMaxBackoff {
			backoff = s.retryMaxBackoff
		}
	}
}

func (s *artifactLogSink) statusWithRetry(ctx context.Context) (LogStatus, error) {
	backoff := 200 * time.Millisecond
	for {
		status, err := s.remote.LogStatus(ctx, s.project, s.job, s.uid)
		if err == nil {
			return status, nil
		}
		if !retryableArtifactError(err) {
			return LogStatus{}, err
		}
		if err := waitForRetry(ctx, retryDelay(err, backoff)); err != nil {
			return LogStatus{}, err
		}
		backoff = nextBackoff(backoff, s.retryMaxBackoff)
	}
}

func (s *artifactLogSink) completeWithRetry(ctx context.Context, key string, input CompleteLogInput) (CompletedLog, error) {
	backoff := 200 * time.Millisecond
	for {
		result, err := s.remote.CompleteLog(ctx, s.project, s.job, key, input)
		if err == nil {
			return result, nil
		}
		if !retryableArtifactError(err) {
			return CompletedLog{}, err
		}
		if err := waitForRetry(ctx, retryDelay(err, backoff)); err != nil {
			return CompletedLog{}, err
		}
		backoff = nextBackoff(backoff, s.retryMaxBackoff)
	}
}

func retryableArtifactError(err error) bool {
	var apiErr ArtifactAPIError
	return !errors.As(err, &apiErr) || apiErr.Retryable || apiErr.StatusCode >= 500 || apiErr.StatusCode == 429
}
func retryDelay(err error, fallback time.Duration) time.Duration {
	var apiErr ArtifactAPIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > fallback {
		return apiErr.RetryAfter
	}
	return fallback
}
func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func nextBackoff(current, maximum time.Duration) time.Duration {
	current *= 2
	if current > maximum {
		return maximum
	}
	return current
}

func (s *artifactLogSink) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *artifactLogSink) setWorkerErr(err error) {
	s.mu.Lock()
	if s.workerErr == nil {
		s.workerErr = err
	}
	s.mu.Unlock()
}
func (s *artifactLogSink) closeFiles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.file.Close()
	_ = s.index.Close()
}

func (s *artifactLogSink) writeCheckpoint() error {
	return s.writeCheckpointState("")
}

func (s *artifactLogSink) writeCheckpointState(forcedState string) error {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	s.mu.Lock()
	state := "Open"
	if s.closing {
		state = "Draining"
	}
	if s.workerErr != nil {
		state = "Failed"
	}
	if forcedState != "" {
		state = forcedState
	}
	checkpoint := logUploadCheckpoint{SchemaVersion: 1, Project: s.project, JobName: s.job, JobUID: s.uid, Stream: "combined", NextSequence: s.nextSequence, ConfirmedOffset: s.uploaded, ProducedBytes: s.produced, State: state, UpdatedAt: time.Now().UTC()}
	s.mu.Unlock()
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	final := filepath.Join(s.dir, "upload.json")
	return writeAtomicFile(final, data)
}

func writeAtomicFile(final string, data []byte) error {
	temporary := final + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temporary, final)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
