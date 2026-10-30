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
	queued, running map[string]bool
	queue           chan string
	ctx             context.Context
	cancel          context.CancelFunc
}

func NewManager(root string, c Config, repositories *repository.Manager, materializer Materializer) (*Manager, error) {
	for _, dir := range []string{".release-work", ".metadata/releases", ".release-trash"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0750); err != nil {
			return nil, err
		}
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
	m.mu.Lock()
	for name, record := range m.records {
		if record.State == ReleaseCreating {
			m.enqueueLocked(name)
		} else if record.State == ReleaseDeleting {
			go m.remove(name)
		}
	}
	m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
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
			if record != nil && record.State == ReleaseCreating {
				m.running[name] = true
			}
			m.mu.Unlock()
			if record == nil || record.State != ReleaseCreating {
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
	m.mu.Unlock()
	_, _, _ = m.Activate(completed.BuildName)
}

func (m *Manager) Activate(name string) (*ReleaseRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.recordLocked(name)
	if err != nil {
		return nil, 0, releaseStorageError()
	}
	if record == nil {
		return nil, 0, &Error{Code: "ReleaseNotFound", Status: http.StatusNotFound}
	}
	if record.State != ReleasePrepared && record.State != ReleaseReady {
		return nil, 0, &Error{Code: "ReleaseNotReady", Status: http.StatusConflict}
	}
	targetDir := m.releaseTargetDir(record)
	if err := ensureStableLinks(targetDir, m.publicKey != ""); err != nil {
		return nil, 0, releaseStorageError()
	}
	target := filepath.ToSlash(filepath.Join("releases", record.BuildName))
	current := filepath.Join(targetDir, "current")
	if existing, err := os.Readlink(current); err == nil && existing == target {
		// Already activated.
	} else {
		temporary := filepath.Join(targetDir, ".current-"+fileutil.NewID("r"))
		if err := os.Symlink(target, temporary); err != nil {
			return nil, 0, releaseStorageError()
		}
		if err := os.Rename(temporary, current); err != nil {
			_ = os.Remove(temporary)
			return nil, 0, releaseStorageError()
		}
		if dir, err := os.Open(targetDir); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	now := time.Now().UTC()
	record.State, record.UpdatedAt = ReleaseReady, now
	if record.CompletedAt == nil {
		record.CompletedAt = &now
	}
	if err := m.persist(record); err != nil {
		return nil, 0, releaseStorageError()
	}
	return cloneRelease(record), http.StatusOK, nil
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
	current := filepath.Join(m.releaseTargetDir(record), "current")
	if target, err := os.Readlink(current); err == nil && target == filepath.ToSlash(filepath.Join("releases", name)) {
		return nil, 0, &Error{Code: "ReleaseInUse", Status: http.StatusConflict}
	}
	if record.State != ReleaseDeleting {
		record.State, record.UpdatedAt = ReleaseDeleting, time.Now().UTC()
		if err := m.persist(record); err != nil {
			return nil, 0, releaseStorageError()
		}
		go m.remove(name)
	}
	return cloneRelease(record), http.StatusAccepted, nil
}

func (m *Manager) remove(name string) {
	m.mu.RLock()
	record := cloneRelease(m.records[name])
	m.mu.RUnlock()
	if record == nil {
		return
	}
	source := m.Path(record)
	trashDir := filepath.Join(m.root, ".release-trash", record.Project, record.TargetArch)
	if err := os.MkdirAll(trashDir, 0750); err != nil {
		return
	}
	trash := filepath.Join(trashDir, name+"-"+fileutil.NewID("d"))
	if err := os.Rename(source, trash); err != nil && !os.IsNotExist(err) {
		return
	}
	if err := os.RemoveAll(trash); err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.records[name]; current != nil && current.State == ReleaseDeleting {
		if err := os.Remove(m.metaPath(name)); err != nil && !os.IsNotExist(err) {
			return
		}
		delete(m.records, name)
	}
}

func (m *Manager) enqueueLocked(name string) {
	if m.queued[name] {
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
			m.mu.Lock()
			for name, record := range m.records {
				if record.State == ReleaseCreating && !m.queued[name] && !m.running[name] {
					m.enqueueLocked(name)
				}
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) Stop() {
	m.cancel()
}

func (m *Manager) persist(record *ReleaseRecord) error {
	if err := fileutil.AtomicJSON(m.metaPath(record.BuildName), record); err != nil {
		return err
	}
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
		!validIdentifier(record.TargetArch) {
		return nil, errors.New("invalid release metadata")
	}
	return &record, nil
}

func (m *Manager) metaPath(name string) string {
	return filepath.Join(m.root, ".metadata/releases", name+".json")
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
		response.PollAfterSeconds = 5
	}
	return response
}
