package source

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
)

var (
	ErrSourceStarted    = errors.New("source already started")
	ErrWatchUnsupported = errors.New("resource does not support watch")
)

type ResourceEventHandler interface {
	OnAdd(runtime.Object)
	OnUpdate(runtime.Object, runtime.Object)
	OnDelete(runtime.Object)
}

type ResourceEventHandlerFuncs struct {
	AddFunc    func(runtime.Object)
	UpdateFunc func(runtime.Object, runtime.Object)
	DeleteFunc func(runtime.Object)
}

func (f ResourceEventHandlerFuncs) OnAdd(obj runtime.Object) {
	if f.AddFunc != nil {
		f.AddFunc(obj)
	}
}
func (f ResourceEventHandlerFuncs) OnUpdate(oldObj, newObj runtime.Object) {
	if f.UpdateFunc != nil {
		f.UpdateFunc(oldObj, newObj)
	}
}
func (f ResourceEventHandlerFuncs) OnDelete(obj runtime.Object) {
	if f.DeleteFunc != nil {
		f.DeleteFunc(obj)
	}
}

type Source interface {
	Name() string
	AddEventHandler(ResourceEventHandler) error
	Run(context.Context) error
	HasSynced() bool
	Ready() bool
}

type subscriptions struct {
	mu       sync.RWMutex
	started  bool
	handlers []ResourceEventHandler
}

func (s *subscriptions) add(handler ResourceEventHandler) error {
	if handler == nil {
		return fmt.Errorf("event handler is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return ErrSourceStarted
	}
	s.handlers = append(s.handlers, handler)
	return nil
}

func (s *subscriptions) start() ([]ResourceEventHandler, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil, ErrSourceStarted
	}
	s.started = true
	return append([]ResourceEventHandler(nil), s.handlers...), nil
}

func safeCall(sourceName string, index int, fn func()) {
	defer func() {
		if value := recover(); value != nil {
			log.Printf("source=%s handler=%d panic=%v stack=%s", sourceName, index, value, debug.Stack())
		}
	}()
	fn()
}
