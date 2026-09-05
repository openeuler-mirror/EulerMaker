package source

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
)

type WatchSource struct {
	name          string
	subscriptions subscriptions
	informer      cache.SharedIndexInformer
	synced        atomic.Bool
}

func NewWatchSource(name string, lw cache.ListerWatcher, objectType runtime.Object, resync, _ time.Duration) (*WatchSource, error) {
	if name == "" || lw == nil || objectType == nil {
		return nil, fmt.Errorf("name, lister-watcher and object type are required")
	}
	return &WatchSource{name: name, informer: cache.NewSharedIndexInformer(lw, objectType, resync, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})}, nil
}

func (s *WatchSource) Name() string { return s.name }
func (s *WatchSource) AddEventHandler(handler ResourceEventHandler) error {
	return s.subscriptions.add(handler)
}
func (s *WatchSource) HasSynced() bool                     { return s.synced.Load() && s.informer.HasSynced() }
func (s *WatchSource) Ready() bool                         { return s.HasSynced() }
func (s *WatchSource) Informer() cache.SharedIndexInformer { return s.informer }

func (s *WatchSource) Run(ctx context.Context) error {
	handlers, err := s.subscriptions.start()
	if err != nil {
		return err
	}
	for index, handler := range handlers {
		i, h := index, handler
		_, err = s.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if value, ok := obj.(runtime.Object); ok {
					safeCall(s.name, i, func() { h.OnAdd(value) })
				}
			},
			UpdateFunc: func(oldObj, newObj any) {
				oldValue, okOld := oldObj.(runtime.Object)
				newValue, okNew := newObj.(runtime.Object)
				if okOld && okNew {
					safeCall(s.name, i, func() { h.OnUpdate(oldValue, newValue) })
				}
			},
			DeleteFunc: func(obj any) {
				value, ok := watchDeletedObject(obj)
				if ok {
					safeCall(s.name, i, func() { h.OnDelete(value) })
				}
			},
		})
		if err != nil {
			return fmt.Errorf("register handler on %s: %w", s.name, err)
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); s.informer.Run(ctx.Done()) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			<-done
			return nil
		case <-done:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("watch source %s stopped unexpectedly", s.name)
		case <-ticker.C:
			if s.informer.HasSynced() {
				s.synced.Store(true)
				ticker.Stop()
				ticker = nil
			}
		}
		if ticker == nil {
			select {
			case <-ctx.Done():
				<-done
				return nil
			case <-done:
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("watch source %s stopped unexpectedly", s.name)
			}
		}
	}
}

func watchDeletedObject(obj any) (runtime.Object, bool) {
	if value, ok := obj.(runtime.Object); ok {
		return value, true
	}
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		value, valid := tombstone.Obj.(runtime.Object)
		return value, valid
	}
	return nil, false
}
