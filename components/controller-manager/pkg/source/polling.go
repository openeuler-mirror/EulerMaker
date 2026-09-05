package source

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type ListPage struct {
	Items                     []runtime.Object
	Continue, ResourceVersion string
}
type ListFunc func(context.Context, schema.GroupVersionResource, string, int64) (ListPage, error)

type pollingEntry struct {
	uid, resourceVersion string
	object               runtime.Object
}
type pollingDataError struct{ err error }

func (e *pollingDataError) Error() string { return e.err.Error() }
func (e *pollingDataError) Unwrap() error { return e.err }

type PollingSource struct {
	gvr           schema.GroupVersionResource
	name          string
	subscriptions subscriptions
	configMu      sync.Mutex
	period, stale time.Duration
	pageSize      int64
	list          ListFunc
	snapshotMu    sync.RWMutex
	snapshot      map[string]pollingEntry
	synced        atomic.Bool
	lastSuccess   atomic.Int64
}

func NewPollingSource(gvr schema.GroupVersionResource, period time.Duration, pageSize int64, stale time.Duration, list ListFunc) (*PollingSource, error) {
	if gvr.Empty() || period <= 0 || pageSize <= 0 || stale <= 0 || list == nil {
		return nil, fmt.Errorf("valid GVR, period, page size, stale threshold and list function are required")
	}
	if minimum := 3 * period; stale < minimum {
		stale = minimum
	}
	return &PollingSource{gvr: gvr, name: gvr.String(), period: period, stale: stale, pageSize: pageSize, list: list, snapshot: make(map[string]pollingEntry)}, nil
}
func (s *PollingSource) Name() string { return s.name }
func (s *PollingSource) AddEventHandler(handler ResourceEventHandler) error {
	return s.subscriptions.add(handler)
}
func (s *PollingSource) HasSynced() bool { return s.synced.Load() }
func (s *PollingSource) Ready() bool {
	if !s.HasSynced() {
		return false
	}
	return time.Since(time.Unix(0, s.lastSuccess.Load())) <= s.stale
}
func (s *PollingSource) shortenPeriod(period time.Duration) error {
	if period <= 0 {
		return fmt.Errorf("poll period must be positive")
	}
	s.subscriptions.mu.RLock()
	started := s.subscriptions.started
	s.subscriptions.mu.RUnlock()
	if started {
		return ErrSourceStarted
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if period < s.period {
		s.period = period
		if minimum := 3 * period; s.stale < minimum {
			s.stale = minimum
		}
	}
	return nil
}

func (s *PollingSource) Run(ctx context.Context) error {
	handlers, err := s.subscriptions.start()
	if err != nil {
		return err
	}
	s.configMu.Lock()
	period := s.period
	s.configMu.Unlock()
	backoff := time.Second
	for {
		err = s.scan(ctx, handlers)
		if err == nil {
			backoff = time.Second
			if !waitContext(ctx, period) {
				return nil
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if _, fatal := err.(*pollingDataError); fatal {
			return fmt.Errorf("poll source %s: %w", s.name, err)
		}
		if !waitContext(ctx, backoff) {
			return nil
		}
		backoff *= 2
		if backoff > period {
			backoff = period
		}
	}
}

func (s *PollingSource) scan(ctx context.Context, handlers []ResourceEventHandler) error {
	next := make(map[string]pollingEntry)
	continueToken := ""
	for {
		page, err := s.list(ctx, s.gvr, continueToken, s.pageSize)
		if err != nil {
			return err
		}
		for _, obj := range page.Items {
			if obj == nil {
				return &pollingDataError{fmt.Errorf("nil object in %s list", s.name)}
			}
			accessor, err := metav1.Accessor(obj)
			if err != nil {
				return &pollingDataError{err}
			}
			if accessor.GetName() == "" || accessor.GetUID() == "" || accessor.GetResourceVersion() == "" {
				return &pollingDataError{fmt.Errorf("object in %s list has incomplete metadata", s.name)}
			}
			key := accessor.GetName()
			if accessor.GetNamespace() != "" {
				key = accessor.GetNamespace() + "/" + key
			}
			if _, exists := next[key]; exists {
				return &pollingDataError{fmt.Errorf("duplicate key %s in %s list", key, s.name)}
			}
			next[key] = pollingEntry{uid: string(accessor.GetUID()), resourceVersion: accessor.GetResourceVersion(), object: obj.DeepCopyObject()}
		}
		if page.Continue == "" {
			break
		}
		continueToken = page.Continue
	}
	s.snapshotMu.Lock()
	old := s.snapshot
	s.snapshot = next
	s.snapshotMu.Unlock()
	s.dispatch(old, next, handlers)
	s.lastSuccess.Store(time.Now().UnixNano())
	s.synced.Store(true)
	return nil
}

func (s *PollingSource) dispatch(old, next map[string]pollingEntry, handlers []ResourceEventHandler) {
	for key, previous := range old {
		if _, exists := next[key]; !exists {
			s.notify(handlers, func(h ResourceEventHandler) { h.OnDelete(previous.object.DeepCopyObject()) })
		}
	}
	for key, current := range next {
		previous, exists := old[key]
		if !exists || previous.uid != current.uid {
			if exists {
				s.notify(handlers, func(h ResourceEventHandler) { h.OnDelete(previous.object.DeepCopyObject()) })
			}
			s.notify(handlers, func(h ResourceEventHandler) { h.OnAdd(current.object.DeepCopyObject()) })
			continue
		}
		s.notify(handlers, func(h ResourceEventHandler) {
			h.OnUpdate(previous.object.DeepCopyObject(), current.object.DeepCopyObject())
		})
	}
}
func (s *PollingSource) notify(handlers []ResourceEventHandler, fn func(ResourceEventHandler)) {
	for index, handler := range handlers {
		i, h := index, handler
		safeCall(s.name, i, func() { fn(h) })
	}
}
func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
