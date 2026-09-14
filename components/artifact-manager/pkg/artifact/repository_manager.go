package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type repositoryManager struct {
	root         string
	timeout      time.Duration
	materializer repositoryMaterializer
	mu           sync.RWMutex
	records      map[string]*RepositoryRecord
	queued       map[string]bool
	running      map[string]bool
	queue        chan string
	ctx          context.Context
	cancel       context.CancelFunc
}

func newRepositoryManager(c Config, materializer repositoryMaterializer) (*repositoryManager, error) {
	for _, dir := range []string{"repositories", ".repository-work", ".metadata/repositories", ".repository-trash"} {
		if err := os.MkdirAll(filepath.Join(c.DataDir, dir), 0750); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &repositoryManager{root: c.DataDir, timeout: c.RepositoryTimeout, materializer: materializer, records: map[string]*RepositoryRecord{}, queued: map[string]bool{}, running: map[string]bool{}, queue: make(chan string, c.RepositoryQueueCapacity), ctx: ctx, cancel: cancel}
	if err := m.load(); err != nil {
		cancel()
		return nil, err
	}
	if err := m.recover(c.RepositoryWorkTTL); err != nil {
		cancel()
		return nil, err
	}
	for i := 0; i < c.RepositoryWorkers; i++ {
		go m.worker()
	}
	for uid, record := range m.records {
		if record.State == RepositoryCreating {
			m.enqueue(uid)
		} else if record.State == RepositoryDeleting {
			go m.remove(uid)
		}
	}
	go m.recoverQueue()
	return m, nil
}

func (m *repositoryManager) recover(workTTL time.Duration) error {
	now := time.Now().UTC()
	for uid, record := range m.records {
		final := m.repositoryPath(record)
		if record.State == RepositoryReady {
			if _, err := os.Stat(filepath.Join(final, "repository.json")); err != nil {
				record.State = RepositoryFailed
				record.Failure = &FailureInfo{Code: "RepositoryContentMissing", Message: "RepositoryContentMissing", Retryable: false, Time: now}
				record.ContentURL = ""
				record.UpdatedAt = now
				if err := m.persist(record); err != nil {
					return err
				}
			}
			continue
		}
		if record.State != RepositoryCreating {
			continue
		}
		var index repositoryIndex
		data, err := os.ReadFile(filepath.Join(final, "repository.json"))
		if err != nil || json.Unmarshal(data, &index) != nil || index.RepositoryUID != uid || index.RequestDigest != record.RequestDigest {
			continue
		}
		digest, err := digestDirectory(final)
		if err != nil || digest != index.RepositoryDigest {
			continue
		}
		record.State, record.RepositoryDigest, record.RPMs = RepositoryReady, digest, index.RPMs
		record.PackageCount, record.ContentURL, record.UpdatedAt, record.CompletedAt = len(index.RPMs), "/repositories/v1/"+uid+"/", now, &now
		if err := m.persist(record); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(m.root, ".repository-work"))
	if err != nil {
		return err
	}
	cutoff := now.Add(-workTTL)
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(m.root, ".repository-work", entry.Name()))
		}
	}
	return nil
}

func (m *repositoryManager) load() error {
	entries, err := os.ReadDir(filepath.Join(m.root, ".metadata/repositories"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var record RepositoryRecord
		data, err := os.ReadFile(filepath.Join(m.root, ".metadata/repositories", entry.Name()))
		if err != nil || json.Unmarshal(data, &record) != nil || record.RepositoryUID == "" {
			return fmt.Errorf("load repository metadata %s: invalid record", entry.Name())
		}
		m.records[record.RepositoryUID] = &record
	}
	return nil
}

func normalizeRepositoryRequest(in CreateRepositoryRequest) (CreateRepositoryRequest, string, error) {
	if !validIdentifier(in.RepositoryUID) || !validIdentifier(in.RepositoryName) || !validIdentifier(in.Project) || !validIdentifier(in.BuildName) || in.RepositoryName != in.BuildName || in.TargetOS == "" || !validIdentifier(in.TargetArch) || len(in.Manifests) == 0 || (in.BaseRepositoryUID != "" && (!validIdentifier(in.BaseRepositoryUID) || in.BaseRepositoryUID == in.RepositoryUID)) {
		return in, "", &repositoryError{code: "InvalidRepositoryRequest", status: 422}
	}
	sort.Slice(in.Manifests, func(i, j int) bool { return in.Manifests[i].JobUID < in.Manifests[j].JobUID })
	seen := map[string]bool{}
	for _, ref := range in.Manifests {
		if !validIdentifier(ref.JobName) || !validIdentifier(ref.JobUID) || seen[ref.JobUID] {
			return in, "", &repositoryError{code: "InvalidManifestReference", status: 422}
		}
		seen[ref.JobUID] = true
	}
	uid := repositoryUID(in.Project, in.BuildName, in.BaseRepositoryUID, in.Manifests)
	if in.RepositoryUID != uid {
		return in, "", &repositoryError{code: "RepositoryUIDMismatch", status: 422}
	}
	data, _ := json.Marshal(in)
	digest := sha256.Sum256(data)
	return in, hex.EncodeToString(digest[:]), nil
}

func repositoryUID(project, buildName, baseUID string, manifests []ManifestReference) string {
	h := sha256.New()
	for _, value := range []string{project, buildName, baseUID} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		h.Write(length[:])
		h.Write([]byte(value))
	}
	for _, ref := range manifests {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(ref.JobUID)))
		h.Write(length[:])
		h.Write([]byte(ref.JobUID))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (m *repositoryManager) submit(in CreateRepositoryRequest) (*RepositoryRecord, int, error) {
	in, digest, err := normalizeRepositoryRequest(in)
	if err != nil {
		return nil, 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if in.BaseRepositoryUID != "" {
		base := m.records[in.BaseRepositoryUID]
		if base == nil || base.State != RepositoryReady || base.Project != in.Project || base.TargetOS != in.TargetOS || base.TargetArch != in.TargetArch {
			return nil, 0, &repositoryError{code: "BaseRepositoryNotReady", status: 422}
		}
	}
	if old := m.records[in.RepositoryUID]; old != nil {
		if old.RequestDigest != digest {
			return nil, 0, &repositoryError{code: "RepositoryIdentityConflict", status: 409}
		}
		switch old.State {
		case RepositoryReady:
			return cloneRepository(old), 200, nil
		case RepositoryDeleting:
			return nil, 0, &repositoryError{code: "RepositoryDeleting", status: 409}
		case RepositoryCreating:
			return cloneRepository(old), 202, nil
		case RepositoryFailed:
			if old.Failure == nil || !old.Failure.Retryable {
				return cloneRepository(old), 200, nil
			}
			if len(m.queue) >= cap(m.queue) {
				return nil, 0, &repositoryError{code: "RepositoryQueueFull", status: 429, retryable: true}
			}
			old.Attempt++
			old.State, old.Failure, old.UpdatedAt = RepositoryCreating, nil, time.Now().UTC()
			if err := m.persist(old); err != nil {
				return nil, 0, &repositoryError{code: "RepositoryStorageUnavailable", status: 503, retryable: true}
			}
			m.enqueueLocked(old.RepositoryUID)
			return cloneRepository(old), 202, nil
		}
	}
	if len(m.queue) >= cap(m.queue) {
		return nil, 0, &repositoryError{code: "RepositoryQueueFull", status: 429, retryable: true}
	}
	now := time.Now().UTC()
	record := &RepositoryRecord{SchemaVersion: 1, RepositoryUID: in.RepositoryUID, RepositoryName: in.RepositoryName, Project: in.Project, BuildName: in.BuildName, TargetOS: in.TargetOS, TargetArch: in.TargetArch, BaseRepositoryUID: in.BaseRepositoryUID, Manifests: in.Manifests, RequestDigest: digest, State: RepositoryCreating, Attempt: 1, CreatedAt: now, UpdatedAt: now}
	if err := m.persist(record); err != nil {
		return nil, 0, &repositoryError{code: "RepositoryStorageUnavailable", status: 503, retryable: true}
	}
	m.records[record.RepositoryUID] = record
	m.enqueueLocked(record.RepositoryUID)
	return cloneRepository(record), 202, nil
}

func (m *repositoryManager) enqueue(uid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enqueueLocked(uid)
}

func (m *repositoryManager) enqueueLocked(uid string) {
	if m.queued[uid] {
		return
	}
	select {
	case m.queue <- uid:
		m.queued[uid] = true
	default:
	}
}

func (m *repositoryManager) worker() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case uid := <-m.queue:
			m.mu.Lock()
			delete(m.queued, uid)
			record := cloneRepository(m.records[uid])
			if record != nil && record.State == RepositoryCreating {
				m.running[uid] = true
				if base := m.records[record.BaseRepositoryUID]; base != nil {
					record.baseBuildName = base.BuildName
				}
			}
			m.mu.Unlock()
			if record == nil || record.State != RepositoryCreating {
				continue
			}
			ctx, cancel := context.WithTimeout(m.ctx, m.timeout)
			result, err := m.materializer.Materialize(ctx, *record)
			cancel()
			m.finish(record, result, err)
		}
	}
}

func (m *repositoryManager) finish(completed *RepositoryRecord, result repositoryResult, materializeErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	uid := completed.RepositoryUID
	record := m.records[uid]
	if record == nil || record.State != RepositoryCreating {
		delete(m.running, uid)
		if materializeErr == nil {
			go os.RemoveAll(m.repositoryPath(completed))
		}
		return
	}
	delete(m.running, uid)
	now := time.Now().UTC()
	record.UpdatedAt = now
	if materializeErr != nil {
		var typed *repositoryError
		if !errors.As(materializeErr, &typed) {
			typed = &repositoryError{code: "RepositoryMaterializationFailed", retryable: true}
		}
		record.State = RepositoryFailed
		record.Failure = &FailureInfo{Code: typed.code, Message: typed.code, Retryable: typed.retryable, Time: now}
	} else {
		record.State, record.RepositoryDigest, record.PackageCount, record.RPMs = RepositoryReady, result.Digest, result.Count, result.RPMs
		record.ContentURL = "/repositories/v1/" + uid + "/"
		record.CompletedAt = &now
	}
	_ = m.persist(record)
}

func (m *repositoryManager) recoverQueue() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			for uid, record := range m.records {
				if record.State == RepositoryCreating && !m.queued[uid] && !m.running[uid] {
					m.enqueueLocked(uid)
				}
			}
			m.mu.Unlock()
		}
	}
}

func (m *repositoryManager) stop() { m.cancel() }

func (m *repositoryManager) get(uid string) (*RepositoryRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record := m.records[uid]
	return cloneRepository(record), record != nil
}

func (m *repositoryManager) delete(uid string) (*RepositoryRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[uid]
	if record == nil {
		return nil, 204, nil
	}
	if record.State != RepositoryDeleting {
		record.State, record.UpdatedAt = RepositoryDeleting, time.Now().UTC()
		if err := m.persist(record); err != nil {
			return nil, 0, &repositoryError{code: "RepositoryStorageUnavailable", status: 503, retryable: true}
		}
		go m.remove(uid)
	}
	return cloneRepository(record), 202, nil
}

func (m *repositoryManager) remove(uid string) {
	m.mu.RLock()
	record := cloneRepository(m.records[uid])
	m.mu.RUnlock()
	if record == nil {
		return
	}
	source := m.repositoryPath(record)
	trashDir := filepath.Join(m.root, ".repository-trash", record.Project, record.TargetArch)
	if err := os.MkdirAll(trashDir, 0750); err != nil {
		return
	}
	trash := filepath.Join(trashDir, uid+"-"+newID("d"))
	if err := os.Rename(source, trash); err != nil && !os.IsNotExist(err) {
		return
	}
	_ = os.RemoveAll(trash)
	m.mu.Lock()
	defer m.mu.Unlock()
	if record := m.records[uid]; record != nil && record.State == RepositoryDeleting {
		_ = os.Remove(m.metaPath(uid))
		delete(m.records, uid)
	}
}

func (m *repositoryManager) persist(record *RepositoryRecord) error {
	return atomicJSON(m.metaPath(record.RepositoryUID), record)
}

func (m *repositoryManager) repositoryPath(record *RepositoryRecord) string {
	return repositoryVersionPath(m.root, record.Project, record.TargetArch, record.BuildName, record.RepositoryUID)
}

func repositoryVersionPath(root, project, arch, buildName, uid string) string {
	return filepath.Join(root, "repositories", project, arch, "history", buildName, "steps", uid)
}
func (m *repositoryManager) metaPath(uid string) string {
	return filepath.Join(m.root, ".metadata/repositories", uid+".json")
}

func cloneRepository(in *RepositoryRecord) *RepositoryRecord {
	if in == nil {
		return nil
	}
	data, _ := json.Marshal(in)
	var out RepositoryRecord
	_ = json.Unmarshal(data, &out)
	return &out
}

func repositoryResponse(record *RepositoryRecord) RepositoryResponse {
	response := RepositoryResponse{RepositoryUID: record.RepositoryUID, State: record.State, Attempt: record.Attempt, ContentURL: record.ContentURL, RepositoryDigest: record.RepositoryDigest, PackageCount: record.PackageCount, RPMs: record.RPMs, Failure: record.Failure, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, CompletedAt: record.CompletedAt}
	if record.State == RepositoryCreating {
		response.PollAfterSeconds = 5
	}
	return response
}
