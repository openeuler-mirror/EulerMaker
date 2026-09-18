package artifact

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
)

type releaseManager struct {
	root, publicKey string
	timeout         time.Duration
	historyTTL      time.Duration
	historyCount    int
	repositories    *repositoryManager
	materializer    releaseMaterializer
	mu              sync.RWMutex
	records         map[string]*ReleaseRecord
	queued, running map[string]bool
	queue           chan string
	ctx             context.Context
	cancel          context.CancelFunc
}

func newReleaseManager(c Config, repositories *repositoryManager, materializer releaseMaterializer) (*releaseManager, error) {
	for _, dir := range []string{".release-work", ".metadata/releases", ".release-trash"} {
		if err := os.MkdirAll(filepath.Join(c.DataDir, dir), 0750); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &releaseManager{root: c.DataDir, publicKey: c.ReleasePublicKey, timeout: c.ReleaseTimeout, historyTTL: c.ReleaseHistoryTTL, historyCount: c.ReleaseHistoryCount, repositories: repositories, materializer: materializer, records: map[string]*ReleaseRecord{}, queued: map[string]bool{}, running: map[string]bool{}, queue: make(chan string, c.ReleaseQueueCapacity), ctx: ctx, cancel: cancel}
	if err := m.load(); err != nil {
		cancel()
		return nil, err
	}
	if err := m.recover(c.ReleaseWorkTTL); err != nil {
		cancel()
		return nil, err
	}
	for i := 0; i < c.ReleaseWorkers; i++ {
		go m.worker()
	}
	for name, record := range m.records {
		if record.State == ReleaseCreating {
			m.enqueue(name)
		} else if record.State == ReleaseDeleting {
			go m.remove(name)
		}
	}
	go m.recoverQueue()
	go m.retentionLoop()
	m.cleanupHistory(time.Now().UTC())
	return m, nil
}

func normalizeReleaseRequest(in CreateReleaseRequest) (CreateReleaseRequest, string, error) {
	if !validIdentifier(in.BuildName) || !validIdentifier(in.Project) || !validIdentifier(in.TargetOS) || !validIdentifier(in.TargetArch) || !validIdentifier(in.SourceRepositoryUID) {
		return in, "", &releaseError{code: "InvalidReleaseRequest", status: http.StatusUnprocessableEntity}
	}
	sort.Strings(in.ExcludeSpecs)
	clean := in.ExcludeSpecs[:0]
	for _, spec := range in.ExcludeSpecs {
		if !validIdentifier(spec) {
			return in, "", &releaseError{code: "InvalidExcludeSpec", status: http.StatusUnprocessableEntity}
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

func (m *releaseManager) submit(in CreateReleaseRequest) (*ReleaseRecord, int, error) {
	in, digest, err := normalizeReleaseRequest(in)
	if err != nil {
		return nil, 0, err
	}
	source, ok := m.repositories.get(in.SourceRepositoryUID)
	if !ok || source.State != RepositoryReady || source.Project != in.Project || source.BuildName != in.BuildName || source.TargetOS != in.TargetOS || source.TargetArch != in.TargetArch {
		return nil, 0, &releaseError{code: "SourceRepositoryNotReady", status: http.StatusUnprocessableEntity}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.records[in.BuildName]; old != nil {
		if old.RequestDigest != digest {
			return nil, 0, &releaseError{code: "ReleaseIdentityConflict", status: http.StatusConflict}
		}
		switch old.State {
		case ReleaseReady:
			return cloneRelease(old), http.StatusOK, nil
		case ReleaseDeleting:
			return nil, 0, &releaseError{code: "ReleaseDeleting", status: http.StatusConflict}
		case ReleaseCreating, ReleasePrepared:
			return cloneRelease(old), http.StatusAccepted, nil
		case ReleaseFailed:
			if old.Failure == nil || !old.Failure.Retryable {
				return cloneRelease(old), http.StatusOK, nil
			}
			if len(m.queue) >= cap(m.queue) {
				return nil, 0, &releaseError{code: "ReleaseQueueFull", status: http.StatusTooManyRequests, retryable: true}
			}
			old.Attempt++
			old.State, old.Failure, old.UpdatedAt = ReleaseCreating, nil, time.Now().UTC()
			if err := m.persist(old); err != nil {
				return nil, 0, releaseStorageError()
			}
			m.enqueueLocked(old.BuildName)
			return cloneRelease(old), http.StatusAccepted, nil
		}
	}
	if len(m.queue) >= cap(m.queue) {
		return nil, 0, &releaseError{code: "ReleaseQueueFull", status: http.StatusTooManyRequests, retryable: true}
	}
	now := time.Now().UTC()
	record := &ReleaseRecord{SchemaVersion: 1, BuildName: in.BuildName, Project: in.Project, TargetOS: in.TargetOS, TargetArch: in.TargetArch, SourceRepositoryUID: in.SourceRepositoryUID, ExcludeSpecs: in.ExcludeSpecs, RequestDigest: digest, State: ReleaseCreating, Attempt: 1, CreatedAt: now, UpdatedAt: now}
	if err := m.persist(record); err != nil {
		return nil, 0, releaseStorageError()
	}
	m.records[record.BuildName] = record
	m.enqueueLocked(record.BuildName)
	return cloneRelease(record), http.StatusAccepted, nil
}

func releaseStorageError() error {
	return &releaseError{code: "ReleaseStorageUnavailable", status: http.StatusServiceUnavailable, retryable: true}
}

func (m *releaseManager) worker() {
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
			source, ok := m.repositories.get(record.SourceRepositoryUID)
			if !ok || source.State != RepositoryReady {
				m.finish(record, releaseResult{}, &releaseError{code: "SourceRepositoryNotReady", status: 422})
				continue
			}
			ctx, cancel := context.WithTimeout(m.ctx, m.timeout)
			result, err := m.materializer.Create(ctx, *record, *source)
			cancel()
			m.finish(record, result, err)
		}
	}
}

func (m *releaseManager) finish(completed *ReleaseRecord, result releaseResult, createErr error) {
	m.mu.Lock()
	record := m.records[completed.BuildName]
	delete(m.running, completed.BuildName)
	if record == nil || record.State != ReleaseCreating {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	record.UpdatedAt = now
	if createErr != nil {
		var typed *releaseError
		if !errors.As(createErr, &typed) {
			typed = &releaseError{code: "ReleaseCreationFailed", retryable: true}
		}
		record.State = ReleaseFailed
		record.Failure = &FailureInfo{Code: typed.code, Message: typed.code, Retryable: typed.retryable, Time: now}
		_ = m.persist(record)
		m.mu.Unlock()
		return
	}
	record.State, record.ReleaseDigest = ReleasePrepared, result.Digest
	_ = m.persist(record)
	m.mu.Unlock()
	_, _, _ = m.activate(completed.BuildName)
}

func (m *releaseManager) activate(name string) (*ReleaseRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[name]
	if record == nil {
		return nil, 0, &releaseError{code: "ReleaseNotFound", status: http.StatusNotFound}
	}
	if record.State != ReleasePrepared && record.State != ReleaseReady {
		return nil, 0, &releaseError{code: "ReleaseNotReady", status: http.StatusConflict}
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
		temporary := filepath.Join(targetDir, ".current-"+newID("r"))
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
	record.State, record.ContentURL, record.UpdatedAt = ReleaseReady, releaseContentURL(record), now
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

func (m *releaseManager) get(name string) (*ReleaseRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record := m.records[name]
	return cloneRelease(record), record != nil
}

func (m *releaseManager) delete(name string) (*ReleaseRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[name]
	if record == nil {
		return nil, http.StatusNoContent, nil
	}
	current := filepath.Join(m.releaseTargetDir(record), "current")
	if target, err := os.Readlink(current); err == nil && target == filepath.ToSlash(filepath.Join("releases", name)) {
		return nil, 0, &releaseError{code: "ReleaseInUse", status: http.StatusConflict}
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

func (m *releaseManager) remove(name string) {
	m.mu.RLock()
	record := cloneRelease(m.records[name])
	m.mu.RUnlock()
	if record == nil {
		return
	}
	source := m.releasePath(record)
	trashDir := filepath.Join(m.root, ".release-trash", record.Project, record.TargetOS, record.TargetArch)
	if err := os.MkdirAll(trashDir, 0750); err != nil {
		return
	}
	trash := filepath.Join(trashDir, name+"-"+newID("d"))
	if err := os.Rename(source, trash); err != nil && !os.IsNotExist(err) {
		return
	}
	_ = os.RemoveAll(trash)
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.records[name]; current != nil && current.State == ReleaseDeleting {
		_ = os.Remove(m.metaPath(name))
		delete(m.records, name)
	}
}

func (m *releaseManager) enqueue(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueueLocked(name)
}
func (m *releaseManager) enqueueLocked(name string) {
	if m.queued[name] {
		return
	}
	select {
	case m.queue <- name:
		m.queued[name] = true
	default:
	}
}

func (m *releaseManager) recoverQueue() {
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

func (m *releaseManager) stop() { m.cancel() }
func (m *releaseManager) persist(record *ReleaseRecord) error {
	return atomicJSON(m.metaPath(record.BuildName), record)
}
func (m *releaseManager) metaPath(name string) string {
	return filepath.Join(m.root, ".metadata/releases", name+".json")
}
func (m *releaseManager) releasePath(record *ReleaseRecord) string {
	return filepath.Join(m.releaseTargetDir(record), "releases", record.BuildName)
}

func (m *releaseManager) releaseTargetDir(record *ReleaseRecord) string {
	return filepath.Join(m.root, "repositories", record.Project, record.TargetOS, record.TargetArch)
}

func releaseContentURL(record *ReleaseRecord) string {
	return "/repositories/" + record.Project + "/" + record.TargetOS + "/" + record.TargetArch + "/"
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

func releaseResponse(record *ReleaseRecord) ReleaseResponse {
	response := ReleaseResponse{BuildName: record.BuildName, State: record.State, Attempt: record.Attempt, ContentURL: record.ContentURL, Failure: record.Failure, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, CompletedAt: record.CompletedAt}
	if record.State == ReleaseCreating || record.State == ReleasePrepared {
		response.PollAfterSeconds = 5
	}
	return response
}
