package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"

	"git-server/pkg/gitops"
	"git-server/pkg/storage"
)

type ManagerConfig struct {
	Workers       int
	MaxRetries    int
	RetryBase     time.Duration
	RetryMax      time.Duration
	CloneBaseURL  string
	CleanupPeriod time.Duration
}

type Manager struct {
	store  *storage.Store
	git    gitClient
	config ManagerConfig
	queue  workqueue.RateLimitingInterface

	mu      sync.Mutex
	states  map[string]*state
	ready   bool
	metrics counters
}

type gitClient interface {
	Clone(context.Context, gitops.Remote, string, string) error
	Fetch(context.Context, gitops.Remote, string, string) error
	ValidateBare(context.Context, string) error
	Origin(context.Context, string) (string, error)
}

func (m *Manager) Metrics() Metrics {
	m.mu.Lock()
	repositories := 0
	for _, value := range m.states {
		if value.Available {
			repositories++
		}
	}
	m.mu.Unlock()
	return Metrics{
		QueueDepth: m.queue.Len(), ActiveWorkers: m.metrics.active.Load(), Repositories: repositories,
		SyncSuccess: m.metrics.syncSuccess.Load(), SyncFailure: m.metrics.syncFailure.Load(),
		DeleteSuccess: m.metrics.deleteSuccess.Load(), DeleteFailure: m.metrics.deleteFailure.Load(),
		Retries:             m.metrics.retries.Load(),
		SyncDurationSeconds: float64(m.metrics.syncDurationNS.Load()) / float64(time.Second), SyncDurationCount: m.metrics.syncDurationN.Load(),
		DeleteDurationSeconds: float64(m.metrics.deleteDurationNS.Load()) / float64(time.Second), DeleteDurationCount: m.metrics.deleteDurationN.Load(),
	}
}

func NewManager(ctx context.Context, store *storage.Store, client gitClient, config ManagerConfig) (*Manager, error) {
	m := &Manager{
		store: store, git: client, config: config,
		queue:  workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(config.RetryBase, config.RetryMax)),
		states: map[string]*state{},
	}
	if err := m.recover(ctx); err != nil {
		return nil, err
	}
	m.ready = true
	return m, nil
}

func (m *Manager) recover(ctx context.Context) error {
	keys, err := m.store.RepositoryKeys()
	if err != nil {
		return fmt.Errorf("scan repositories: %w", err)
	}
	for _, key := range keys {
		path, exists, err := m.store.RepositoryPath(key)
		if err != nil || !exists {
			continue
		}
		if err := m.git.ValidateBare(ctx, path); err != nil {
			log.Printf("component=git-server operation=recover key=%q error=%q", key, err)
			continue
		}
		origin, err := m.git.Origin(ctx, path)
		if err != nil {
			log.Printf("component=git-server operation=recover key=%q error=%q", key, err)
			continue
		}
		parsed, err := ParseURL(origin)
		if err != nil || parsed.Key != key {
			log.Printf("component=git-server operation=recover key=%q error=%q", key, "origin does not match repository key")
			continue
		}
		m.states[key] = &state{Key: key, OriginURL: origin, Available: true}
	}
	return nil
}

func (m *Manager) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

func (m *Manager) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for i := 0; i < m.config.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for m.processNext(ctx) {
			}
		}()
	}
	go m.cleanupLoop(ctx)
	<-ctx.Done()
	m.mu.Lock()
	m.ready = false
	m.mu.Unlock()
	m.queue.ShutDownWithDrain()
	workers.Wait()
}

func (m *Manager) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(m.config.CleanupPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.store.CleanupOrphans(); err != nil {
				log.Printf("component=git-server operation=temp-cleanup error=%q", err)
			}
		}
	}
}

func (m *Manager) Sync(originURL string) (Response, error) {
	parsed, err := ParseURL(originURL)
	if err != nil {
		return Response{}, err
	}
	m.mu.Lock()
	s := m.states[parsed.Key]
	if s == nil {
		s = &state{Key: parsed.Key}
		m.states[parsed.Key] = s
	}
	s.Revision++
	s.DesiredAction = ActionSync
	s.OriginURL = originURL
	s.Error, s.RetryCount, s.ResetBackoff = nil, 0, true
	response := m.responseLocked(s)
	m.mu.Unlock()
	if m.queue.ShuttingDown() {
		return Response{}, fmt.Errorf("queue is closed")
	}
	m.queue.Add(parsed.Key)
	return response, nil
}

func (m *Manager) Delete(originURL string) (Response, bool, error) {
	parsed, err := ParseURL(originURL)
	if err != nil {
		return Response{}, false, err
	}
	m.mu.Lock()
	s := m.states[parsed.Key]
	if s == nil {
		m.mu.Unlock()
		_, exists, pathErr := m.store.RepositoryPath(parsed.Key)
		return Response{Key: parsed.Key}, false, pathErrOrNil(pathErr, exists)
	}
	s.Revision++
	s.DesiredAction = ActionDelete
	s.Error, s.RetryCount, s.ResetBackoff = nil, 0, true
	response := m.responseLocked(s)
	m.mu.Unlock()
	if m.queue.ShuttingDown() {
		return Response{}, false, fmt.Errorf("queue is closed")
	}
	m.queue.Add(parsed.Key)
	return response, true, nil
}

func pathErrOrNil(err error, exists bool) error {
	if err != nil {
		return err
	}
	if exists {
		return storage.ErrConflict
	}
	return nil
}

func (m *Manager) Status(originURL string) (Response, bool, error) {
	parsed, err := ParseURL(originURL)
	if err != nil {
		return Response{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[parsed.Key]
	if s == nil || s.DesiredAction == ActionDelete && !s.Available {
		return Response{Key: parsed.Key}, false, nil
	}
	return m.responseLocked(s), true, nil
}

func (m *Manager) responseLocked(s *state) Response {
	response := Response{Key: s.Key, RetryCount: s.RetryCount, Error: copyError(s.Error)}
	if s.SyncTime != nil {
		t := *s.SyncTime
		response.SyncTime = &t
		response.CloneURL = strings.TrimRight(m.config.CloneBaseURL, "/") + "/" + s.Key
	}
	return response
}

func copyError(value *RepositoryError) *RepositoryError {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

func (m *Manager) processNext(ctx context.Context) bool {
	item, shutdown := m.queue.Get()
	if shutdown {
		return false
	}
	key, ok := item.(string)
	if !ok {
		m.queue.Done(item)
		m.queue.Forget(item)
		return true
	}
	defer m.queue.Done(item)
	m.mu.Lock()
	s := m.states[key]
	if s == nil {
		m.mu.Unlock()
		m.queue.Forget(item)
		return true
	}
	snap := snapshot{action: s.DesiredAction, originURL: s.OriginURL, revision: s.Revision, reset: s.ResetBackoff}
	s.ResetBackoff = false
	m.mu.Unlock()
	if snap.reset {
		m.queue.Forget(item)
	}

	var operationErr error
	var syncTime *time.Time
	var quarantine string
	started := time.Now()
	m.metrics.active.Add(1)
	s.operationLock.Lock()
	switch snap.action {
	case ActionSync:
		syncTime, operationErr = m.sync(ctx, key, snap.originURL)
	case ActionDelete:
		quarantine, operationErr = m.quarantine(key)
	default:
		operationErr = &gitops.OperationError{Code: "InvalidAction", Message: "repository has no desired action", Retryable: false}
	}
	s.operationLock.Unlock()
	m.metrics.active.Add(-1)
	duration := uint64(time.Since(started))
	if snap.action == ActionSync {
		m.metrics.syncDurationNS.Add(duration)
		m.metrics.syncDurationN.Add(1)
		if operationErr == nil {
			m.metrics.syncSuccess.Add(1)
		} else {
			m.metrics.syncFailure.Add(1)
		}
	} else if snap.action == ActionDelete {
		m.metrics.deleteDurationNS.Add(duration)
		m.metrics.deleteDurationN.Add(1)
		if operationErr == nil {
			m.metrics.deleteSuccess.Add(1)
		} else {
			m.metrics.deleteFailure.Add(1)
		}
	}
	if quarantine != "" {
		if err := m.store.Cleanup(quarantine); err != nil {
			log.Printf("component=git-server operation=delete-cleanup key=%q error=%q", key, err)
		}
	}

	m.mu.Lock()
	current := m.states[key]
	if syncTime != nil {
		current.SyncTime = syncTime
		current.Available = true
	}
	if snap.action == ActionDelete && operationErr == nil {
		current.SyncTime = nil
		current.Available = false
	}
	if current.Revision != snap.revision {
		m.mu.Unlock()
		m.queue.Forget(item)
		return true
	}
	if operationErr == nil {
		current.Error, current.RetryCount = nil, 0
		m.mu.Unlock()
		m.queue.Forget(item)
		return true
	}
	opErr := gitops.AsOperationError(operationErr)
	current.Error = &RepositoryError{Code: opErr.Code, Message: opErr.Message, Retryable: opErr.Retryable}
	if opErr.Retryable && current.RetryCount < m.config.MaxRetries {
		current.RetryCount++
		m.metrics.retries.Add(1)
		m.mu.Unlock()
		m.queue.AddRateLimited(item)
		return true
	}
	m.mu.Unlock()
	m.queue.Forget(item)
	return true
}

func (m *Manager) sync(ctx context.Context, key, requestedOrigin string) (*time.Time, error) {
	path, exists, err := m.store.RepositoryPath(key)
	if err != nil {
		return nil, localError(err)
	}
	if exists {
		return m.fetch(ctx, key, path)
	}
	parsed, err := ParseURL(requestedOrigin)
	if err != nil || parsed.Key != key {
		return nil, &gitops.OperationError{Code: "InvalidRepositoryURL", Message: "repository URL does not match key", Retryable: false}
	}
	id, err := randomID()
	if err != nil {
		return nil, localError(err)
	}
	tempName, tempPath, err := m.store.CreateTemporary("clone", id)
	if err != nil {
		return nil, localError(err)
	}
	published := false
	defer func() {
		if !published {
			if cleanupErr := m.store.Cleanup(tempName); cleanupErr != nil {
				log.Printf("component=git-server operation=clone-cleanup key=%q error=%q", key, cleanupErr)
			}
		}
	}()
	remote := toRemote(parsed)
	if err := m.git.Clone(ctx, remote, tempPath, id); err != nil {
		return nil, err
	}
	if err := m.git.ValidateBare(ctx, tempPath); err != nil {
		return nil, err
	}
	origin, err := m.git.Origin(ctx, tempPath)
	if err != nil {
		return nil, err
	}
	storedOrigin, err := ParseURL(origin)
	if err != nil || storedOrigin.Key != key {
		return nil, &gitops.OperationError{Code: "OriginMismatch", Message: "cloned origin does not match repository key", Retryable: false}
	}
	if err := m.store.Publish(tempName, key); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			if cleanupErr := m.store.Cleanup(tempName); cleanupErr != nil {
				return nil, localError(cleanupErr)
			}
			published = true
			existingPath, exists, openErr := m.store.RepositoryPath(key)
			if openErr != nil || !exists {
				return nil, localError(firstError(openErr, storage.ErrConflict))
			}
			return m.fetch(ctx, key, existingPath)
		}
		return nil, localError(err)
	}
	published = true
	now := time.Now().UTC()
	return &now, nil
}

func (m *Manager) fetch(ctx context.Context, key, path string) (*time.Time, error) {
	if err := m.git.ValidateBare(ctx, path); err != nil {
		return nil, err
	}
	origin, err := m.git.Origin(ctx, path)
	if err != nil {
		return nil, err
	}
	parsed, err := ParseURL(origin)
	if err != nil || parsed.Key != key {
		return nil, &gitops.OperationError{Code: "OriginMismatch", Message: "stored origin does not match repository key", Retryable: false}
	}
	id, err := randomID()
	if err != nil {
		return nil, localError(err)
	}
	if err := m.git.Fetch(ctx, toRemote(parsed), path, id); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &now, nil
}

func (m *Manager) quarantine(key string) (string, error) {
	id, err := randomID()
	if err != nil {
		return "", localError(err)
	}
	name, _, err := m.store.Quarantine(key, id)
	if err != nil {
		return "", localError(err)
	}
	return name, nil
}

func toRemote(parsed ParsedURL) gitops.Remote {
	return gitops.Remote{URL: parsed.Original, Scheme: parsed.Scheme, Host: parsed.Host, RepositoryPath: parsed.Path, User: parsed.User}
}

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func localError(err error) error {
	if err == nil {
		return nil
	}
	return &gitops.OperationError{Code: "LocalStorageError", Message: err.Error(), Retryable: false}
}

func firstError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func (m *Manager) WithRepository(ctx context.Context, originURL string, fn func(string) error) error {
	parsed, err := ParseURL(originURL)
	if err != nil {
		return err
	}
	m.mu.Lock()
	s := m.states[parsed.Key]
	m.mu.Unlock()
	if s == nil {
		return fsNotFound{}
	}
	s.operationLock.RLock()
	defer s.operationLock.RUnlock()
	path, exists, err := m.store.RepositoryPath(parsed.Key)
	if err != nil {
		return err
	}
	if !exists {
		return fsNotFound{}
	}
	if err := m.git.ValidateBare(ctx, path); err != nil {
		return fsNotFound{}
	}
	return fn(path)
}

type fsNotFound struct{}

func (fsNotFound) Error() string { return "repository not found" }
func IsNotFound(err error) bool {
	var target fsNotFound
	return errors.As(err, &target)
}
