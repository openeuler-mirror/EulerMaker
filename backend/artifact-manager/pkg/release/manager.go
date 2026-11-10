package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"artifact-manager/pkg/diagnostics"
	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/repository"
)

type Manager struct {
	root, publicKey string
	timeout         time.Duration
	historyTTL      time.Duration
	historyCount    int
	repositories    *repository.Manager
	materializer    Materializer
	mu              sync.RWMutex
	records         map[string]*ReleaseRecord
	cleaningBuilds  map[string]bool
	metadataPaths   map[string]string
	queued, running map[string]bool
	queue           chan string
	ctx             context.Context
	cancel          context.CancelFunc
}

func NewManager(root string, c Config, repositories *repository.Manager, materializer Materializer) (*Manager, error) {
	if err := os.MkdirAll(filepath.Join(root, "repositories"), 0750); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		root:         root,
		publicKey:    c.PublicKey,
		timeout:      c.Timeout,
		historyTTL:   c.HistoryTTL,
		historyCount: c.HistoryCount,
		repositories: repositories,
		materializer: materializer,
		records:      map[string]*ReleaseRecord{},
		queued:       map[string]bool{},
		running:      map[string]bool{},
		queue:        make(chan string, c.QueueCapacity),
		ctx:          ctx,
		cancel:       cancel,
	}
	if err := m.recover(c.WorkTTL); err != nil {
		cancel()
		return nil, err
	}
	m.retryPending()
	for i := 0; i < c.Workers; i++ {
		go m.worker()
	}
	go m.recoverQueue()
	go m.retentionLoop()
	m.cleanupHistory(time.Now().UTC())
	return m, nil
}

func normalizeReleaseRequest(in CreateReleaseRequest) (CreateReleaseRequest, string, error) {
	if !validIdentifier(in.BuildName) ||
		!validIdentifier(in.Project) ||
		!validIdentifier(in.TargetArch) ||
		!validIdentifier(in.SourceRepositoryUID) {
		return in, "", &Error{Code: "InvalidReleaseRequest", Status: http.StatusUnprocessableEntity}
	}
	sort.Strings(in.ExcludeSpecs)
	clean := in.ExcludeSpecs[:0]
	for _, spec := range in.ExcludeSpecs {
		if !validIdentifier(spec) {
			return in, "", &Error{Code: "InvalidExcludeSpec", Status: http.StatusUnprocessableEntity}
		}
		if len(clean) == 0 || clean[len(clean)-1] != spec {
			clean = append(clean, spec)
		}
	}
	in.ExcludeSpecs = clean
	data, _ := json.Marshal(in)
	digest := sha256.Sum256(data)
	return in, hex.EncodeToString(digest[:]), nil
}

func (m *Manager) Submit(in CreateReleaseRequest) (*ReleaseRecord, int, error) {
	in, digest, err := normalizeReleaseRequest(in)
	if err != nil {
		return nil, 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cleaningBuilds[in.BuildName] {
		return nil, 0, &Error{Code: "ReleaseDeleting", Status: http.StatusConflict}
	}
	if err := repository.CheckBuildActive(m.root, in.Project, in.BuildName); err != nil {
		return nil, 0, releaseStopError(err)
	}
	source, ok, sourceErr := m.repositories.Get(in.SourceRepositoryUID)
	if sourceErr != nil {
		return nil, 0, releaseStorageError()
	}
	if !ok ||
		source.State != repository.RepositoryReady ||
		source.Project != in.Project ||
		source.BuildName != in.BuildName ||
		source.TargetArch != in.TargetArch {
		return nil, 0, &Error{Code: "SourceRepositoryNotReady", Status: http.StatusUnprocessableEntity}
	}
	old, err := m.recordLocked(in.BuildName)
	if err != nil {
		return nil, 0, releaseStorageError()
	}
	if old != nil {
		if old.RequestDigest != digest {
			return nil, 0, &Error{Code: "ReleaseIdentityConflict", Status: http.StatusConflict}
		}
		switch old.State {
		case ReleaseReady:
			return cloneRelease(old), http.StatusOK, nil
		case ReleaseDeleting:
			return nil, 0, &Error{Code: "ReleaseDeleting", Status: http.StatusConflict}
		case ReleaseCreating, ReleasePrepared:
			return cloneRelease(old), http.StatusAccepted, nil
		case ReleaseFailed:
			if old.Failure == nil || !old.Failure.Retryable {
				return cloneRelease(old), http.StatusOK, nil
			}
			if len(m.queue) >= cap(m.queue) {
				return nil, 0, &Error{Code: "ReleaseQueueFull", Status: http.StatusTooManyRequests, Retryable: true}
			}
			old.State, old.Failure, old.UpdatedAt = ReleaseCreating, nil, time.Now().UTC()
			if err := m.persist(old); err != nil {
				return nil, 0, releaseStorageError()
			}
			m.enqueueLocked(old.BuildName)
			return cloneRelease(old), http.StatusAccepted, nil
		}
	}
	if len(m.queue) >= cap(m.queue) {
		return nil, 0, &Error{Code: "ReleaseQueueFull", Status: http.StatusTooManyRequests, Retryable: true}
	}
	now := time.Now().UTC()
	record := &ReleaseRecord{
		SchemaVersion:       1,
		BuildName:           in.BuildName,
		Project:             in.Project,
		TargetArch:          in.TargetArch,
		SourceRepositoryUID: in.SourceRepositoryUID,
		ExcludeSpecs:        in.ExcludeSpecs,
		RequestDigest:       digest,
		State:               ReleaseCreating,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := m.persist(record); err != nil {
		return nil, 0, releaseStorageError()
	}
	m.enqueueLocked(record.BuildName)
	return cloneRelease(record), http.StatusAccepted, nil
}

func releaseStorageError() error {
	return &Error{Code: "ReleaseStorageUnavailable", Status: http.StatusServiceUnavailable, Retryable: true}
}

func (m *Manager) worker() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case name := <-m.queue:
			m.mu.Lock()
			delete(m.queued, name)
			record := cloneRelease(m.records[name])
			if record == nil || m.running[name] ||
				(record.State != ReleaseCreating && record.State != ReleasePrepared && record.State != ReleaseDeleting) {
				m.mu.Unlock()
				continue
			}
			m.running[name] = true
			m.mu.Unlock()
			if record.State == ReleasePrepared {
				m.finishActivation(name)
				continue
			}
			if record.State == ReleaseDeleting {
				m.remove(name)
				m.mu.Lock()
				delete(m.running, name)
				m.mu.Unlock()
				continue
			}
			source, ok, sourceErr := m.repositories.Get(record.SourceRepositoryUID)
			if sourceErr != nil {
				m.finish(record, Result{}, releaseStorageError())
				continue
			}
			if !ok || source.State != repository.RepositoryReady {
				m.finish(record, Result{}, &Error{Code: "SourceRepositoryNotReady", Status: 422})
				continue
			}
			// Repository records stay lightweight in memory. Load the immutable RPM
			// index only for the release currently being materialized.
			index, err := repository.LoadIndex(m.repositories.RepositoryPath(source), source.RepositoryUID)
			if err != nil {
				var repositoryErr *repository.Error
				if errors.As(err, &repositoryErr) && !repositoryErr.Retryable {
					err = &Error{Code: "SourceRepositoryInvalid", Status: 422}
				}
				m.finish(record, Result{}, err)
				continue
			}
			if index.SigningFingerprint != source.SigningFingerprint {
				m.finish(record, Result{}, &Error{Code: "SourceRepositoryInvalid", Status: 422})
				continue
			}
			source.RPMs = index.RPMs
			ctx, cancel := context.WithTimeout(m.ctx, m.timeout)
			result, err := m.materializer.Create(ctx, *record, *source)
			cancel()
			m.finish(record, result, err)
		}
	}
}

func (m *Manager) finish(completed *ReleaseRecord, result Result, createErr error) {
	m.mu.Lock()
	record := cloneRelease(m.records[completed.BuildName])
	delete(m.running, completed.BuildName)
	if record == nil || record.State != ReleaseCreating {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	record.UpdatedAt = now
	if createErr != nil {
		var typed *Error
		if !errors.As(createErr, &typed) {
			typed = &Error{Code: "ReleaseCreationFailed", Retryable: true}
		}
		klog.ErrorS(nil, "release create failed",
			"component", "release", "operation", "create", "project", record.Project, "build", record.BuildName,
			"code", diagnostics.Code(typed.Code), "cause", diagnostics.Cause(createErr))
		record.State = ReleaseFailed
		record.Failure = &FailureInfo{Code: typed.Code, Message: typed.Code, Retryable: typed.Retryable, Time: now}
		_ = m.persist(record)
		m.mu.Unlock()
		return
	}
	record.State, record.ReleaseDigest = ReleasePrepared, result.Digest
	if err := m.persist(record); err != nil {
		m.mu.Unlock()
		return
	}
	m.running[completed.BuildName] = true
	m.mu.Unlock()
	m.finishActivation(completed.BuildName)
}

func (m *Manager) finishActivation(name string) {
	if err := m.activate(name); err != nil {
		klog.ErrorS(nil, "release activate failed",
			"component", "release", "operation", "activate", "build", name, "cause", diagnostics.Cause(err))
	}
	m.mu.Lock()
	delete(m.running, name)
	m.mu.Unlock()
}

// activate commits the prepared directory to the stable entry point. Failures leave the checkpoint retryable.
func (m *Manager) activate(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cleaningBuilds[name] {
		return &Error{Code: "ReleaseDeleting", Status: http.StatusConflict}
	}
	record, err := m.recordLocked(name)
	if err != nil {
		return releaseStorageError()
	}
	if record == nil {
		return &Error{Code: "ReleaseNotFound", Status: http.StatusNotFound}
	}
	if record.State == ReleaseReady {
		return nil
	}
	if err := repository.CheckBuildActive(m.root, record.Project, record.BuildName); err != nil {
		return releaseStopError(err)
	}
	if record.State != ReleasePrepared {
		return &Error{Code: "ReleaseNotReady", Status: http.StatusConflict}
	}
	targetDir := m.releaseTargetDir(record)
	if err := ensureStableLinks(targetDir, m.publicKey != ""); err != nil {
		return releaseStorageError()
	}
	target := filepath.ToSlash(filepath.Join("releases", record.BuildName))
	current := filepath.Join(targetDir, "current")
	if existing, err := os.Readlink(current); err == nil && existing == target {
		// Already activated.
	} else {
		temporary := filepath.Join(targetDir, ".current-"+fileutil.NewID("r"))
		if err := os.Symlink(target, temporary); err != nil {
			return releaseStorageError()
		}
		if err := os.Rename(temporary, current); err != nil {
			_ = os.Remove(temporary)
			return releaseStorageError()
		}
	}
	dir, err := os.Open(targetDir)
	if err != nil {
		return releaseStorageError()
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return releaseStorageError()
	}
	now := time.Now().UTC()
	record.State, record.UpdatedAt = ReleaseReady, now
	if record.CompletedAt == nil {
		record.CompletedAt = &now
	}
	if err := m.persist(record); err != nil {
		return releaseStorageError()
	}
	return nil
}

func ensureStableLinks(targetDir string, publicKey bool) error {
	if err := os.MkdirAll(filepath.Join(targetDir, "releases"), 0750); err != nil {
		return err
	}
	links := map[string]string{"Packages": "current/Packages", "repodata": "current/repodata"}
	if publicKey {
		links["RPM-GPG-KEY-openEuler"] = "current/RPM-GPG-KEY-openEuler"
	}
	for name, target := range links {
		path := filepath.Join(targetDir, name)
		if existing, err := os.Readlink(path); err == nil {
			if existing != target {
				return fmt.Errorf("unexpected stable link")
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(target, path); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func (m *Manager) Get(name string) (*ReleaseRecord, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, err := m.recordLocked(name)
	if err != nil {
		return nil, false, releaseStorageError()
	}
	return record, record != nil, nil
}

func (m *Manager) Delete(name string) (*ReleaseRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.recordLocked(name)
	if err != nil {
		return nil, 0, releaseStorageError()
	}
	if record == nil {
		return nil, http.StatusNoContent, nil
	}
	if m.cleaningBuilds[name] {
		return nil, 0, &Error{Code: "ReleaseDeleting", Status: http.StatusConflict}
	}
	current := filepath.Join(m.releaseTargetDir(record), "current")
	if target, err := os.Readlink(current); err == nil && target == filepath.ToSlash(filepath.Join("releases", name)) {
		return nil, 0, &Error{Code: "ReleaseInUse", Status: http.StatusConflict}
	}
	if record.State != ReleaseDeleting {
		record.State, record.UpdatedAt = ReleaseDeleting, time.Now().UTC()
		if err := m.persist(record); err != nil {
			return nil, 0, releaseStorageError()
		}
	}
	m.enqueueLocked(name)
	return cloneRelease(record), http.StatusAccepted, nil
}

func (m *Manager) remove(name string) {
	m.mu.RLock()
	record := cloneRelease(m.records[name])
	m.mu.RUnlock()
	if record == nil || record.State != ReleaseDeleting {
		return
	}
	source := m.Path(record)
	trashDir := filepath.Join(m.releaseTargetDir(record), "releases", ".trash")
	if err := os.MkdirAll(trashDir, 0750); err != nil {
		klog.ErrorS(nil, "release delete failed",
			"component", "release", "operation", "delete", "project", record.Project, "build", record.BuildName,
			"record", record.BuildName, "cause", diagnostics.Cause(err))
		return
	}
	// Reuse the same destination after a failed removal or restart.
	trash := filepath.Join(trashDir, name+"-deleting")
	if err := os.Rename(source, trash); err != nil && !os.IsNotExist(err) {
		klog.ErrorS(nil, "release delete failed",
			"component", "release", "operation", "delete", "project", record.Project, "build", record.BuildName,
			"record", record.BuildName, "cause", diagnostics.Cause(err))
		return
	}
	if err := os.RemoveAll(trash); err != nil {
		klog.ErrorS(nil, "release delete failed",
			"component", "release", "operation", "delete", "project", record.Project, "build", record.BuildName,
			"record", record.BuildName, "cause", diagnostics.Cause(err))
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.records[name]; current != nil && current.State == ReleaseDeleting {
		if err := os.Remove(m.metaPath(name)); err != nil && !os.IsNotExist(err) {
			klog.ErrorS(nil, "release delete failed",
				"component", "release", "operation", "delete", "project", record.Project, "build", record.BuildName,
				"record", record.BuildName, "cause", diagnostics.Cause(err))
			return
		}
		delete(m.records, name)
		delete(m.metadataPaths, name)
	}
}

func (m *Manager) enqueueLocked(name string) {
	if m.queued[name] || m.running[name] {
		return
	}
	select {
	case m.queue <- name:
		m.queued[name] = true
	default:
	}
}

func (m *Manager) recoverQueue() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.retryPending()
		}
	}
}

// retryPending requeues unfinished work without duplicating queued or running tasks.
func (m *Manager) retryPending() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, record := range m.records {
		if record.State == ReleasePrepared {
			// A persisted stop marker also fences recovered and queued activation attempts.
			if err := repository.CheckBuildActive(m.root, record.Project, name); err != nil {
				continue
			}
			m.enqueueLocked(name)
		}
		if record.State == ReleaseCreating || record.State == ReleaseDeleting {
			m.enqueueLocked(name)
		}
	}
}

func (m *Manager) Stop() {
	m.cancel()
}

func (m *Manager) persist(record *ReleaseRecord) (err error) {
	defer func() {
		if err != nil {
			klog.ErrorS(nil, "release persist failed",
				"component", "release", "operation", "persist", "project", record.Project, "build", record.BuildName,
				"record", record.BuildName, "cause", diagnostics.Cause(err))
		}
	}()
	path := m.recordMetaPath(record)
	if previous := m.metaPath(record.BuildName); previous != "" && previous != path {
		return errors.New("release metadata scope changed")
	}
	if err := fileutil.AtomicJSON(path, record); err != nil {
		return err
	}
	if m.metadataPaths == nil {
		m.metadataPaths = make(map[string]string)
	}
	m.metadataPaths[record.BuildName] = path
	m.cacheActive(record)
	return nil
}

// cacheActive is called with the write lock held, or before workers start.
func (m *Manager) cacheActive(record *ReleaseRecord) {
	switch record.State {
	case ReleaseCreating, ReleasePrepared, ReleaseDeleting:
		m.records[record.BuildName] = cloneRelease(record)
	default:
		delete(m.records, record.BuildName)
	}
}

func (m *Manager) recordLocked(name string) (*ReleaseRecord, error) {
	if record := m.records[name]; record != nil {
		return cloneRelease(record), nil
	}
	return m.readRecord(name)
}

func (m *Manager) readRecord(name string) (*ReleaseRecord, error) {
	if !validIdentifier(name) {
		return nil, errors.New("invalid release name")
	}
	if m.metaPath(name) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(m.metaPath(name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record ReleaseRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if record.BuildName != name ||
		!validIdentifier(record.Project) ||
		!validIdentifier(record.TargetArch) || m.metaPath(name) != m.recordMetaPath(&record) {
		return nil, errors.New("invalid release metadata")
	}
	return &record, nil
}

func (m *Manager) metaPath(name string) string {
	return m.metadataPaths[name]
}

func (m *Manager) recordMetaPath(record *ReleaseRecord) string {
	return filepath.Join(m.releaseTargetDir(record), "releases", ".records", record.BuildName+".json")
}

func (m *Manager) Path(record *ReleaseRecord) string {
	return filepath.Join(m.releaseTargetDir(record), "releases", record.BuildName)
}

func (m *Manager) releaseTargetDir(record *ReleaseRecord) string {
	return filepath.Join(m.root, "repositories", record.Project, record.TargetArch)
}

func releaseContentURL(record *ReleaseRecord) string {
	return "/repositories/" + record.Project + "/" + record.TargetArch + "/"
}

func cloneRelease(in *ReleaseRecord) *ReleaseRecord {
	if in == nil {
		return nil
	}
	data, _ := json.Marshal(in)
	var out ReleaseRecord
	_ = json.Unmarshal(data, &out)
	return &out
}

func ResponseFor(record *ReleaseRecord) ReleaseResponse {
	response := ReleaseResponse{
		BuildName:   record.BuildName,
		State:       record.State,
		Failure:     record.Failure,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
		CompletedAt: record.CompletedAt,
	}
	if record.State == ReleaseReady {
		response.ContentURL = releaseContentURL(record)
	}
	if record.State == ReleaseCreating || record.State == ReleasePrepared {
		response.State = ReleaseCreating
		response.PollAfterSeconds = 5
	}
	return response
}
