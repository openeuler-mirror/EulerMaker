package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeSource struct {
	name          string
	synced, ready bool
	runErr        error
}

func (f *fakeSource) Name() string                                      { return f.name }
func (f *fakeSource) AddEventHandler(source.ResourceEventHandler) error { return nil }
func (f *fakeSource) HasSynced() bool                                   { return f.synced }
func (f *fakeSource) Ready() bool                                       { return f.ready }
func (f *fakeSource) GetByKey(string) (runtime.Object, bool, error)     { return nil, false, nil }
func (f *fakeSource) ByIndex(string, string) ([]runtime.Object, error)  { return nil, nil }
func (f *fakeSource) Run(ctx context.Context) error {
	if f.runErr != nil {
		return f.runErr
	}
	<-ctx.Done()
	return nil
}

type fakeWatchFactory struct{ items []source.Source }

func (f *fakeWatchFactory) ForResource(schema.GroupVersionResource) (source.CachedSource, error) {
	return nil, errors.New("unused")
}
func (f *fakeWatchFactory) Sources() []source.Source { return f.items }

type fakePollingFactory struct{ items []source.Source }

func (f *fakePollingFactory) ForResource(schema.GroupVersionResource, time.Duration, metav1.ListOptions) (source.Source, error) {
	return nil, errors.New("unused")
}
func (f *fakePollingFactory) Sources() []source.Source { return f.items }

type fakeHealth struct {
	ready atomic.Bool
	mu    sync.Mutex
	names []string
}

func (f *fakeHealth) SetReady(value bool)           { f.ready.Store(value) }
func (f *fakeHealth) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (f *fakeHealth) AddHealthChecker(name string, _ controller.HealthChecker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	return nil
}

func TestManagerStartsAfterSourcesSyncAndStops(t *testing.T) {
	s := &fakeSource{name: "source", synced: true, ready: true}
	h := &fakeHealth{}
	started := make(chan struct{})
	var observedConfig ControllerConfig
	initializers := map[string]InitFunc{"test": func(_ context.Context, init InitContext) (controller.Controller, bool, error) {
		observedConfig = init.Config
		c, err := controller.New("test", func(context.Context, string) (controller.ReconcileResult, error) {
			return controller.ReconcileResult{}, nil
		}, 1)
		if err != nil {
			return nil, false, err
		}
		return &observedController{Controller: c, started: started}, true, nil
	}}
	m, err := New(initializers, Dependencies{Client: &clientpkg.Client{}, WatchFactory: &fakeWatchFactory{items: []source.Source{s}}, PollingFactory: &fakePollingFactory{}}, Config{Workers: 1, Controllers: "*", CacheSyncTimeout: time.Second, ShutdownTimeout: time.Second, SlowRetryInitial: time.Second, SlowRetryMax: time.Minute, SlowRetryJitter: 0.2}, h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("controller did not start")
	}
	if observedConfig.SlowRetryInitial != time.Second || observedConfig.SlowRetryMax != time.Minute || observedConfig.SlowRetryJitter != 0.2 {
		t.Fatalf("unexpected controller retry config: %+v", observedConfig)
	}
	if !h.ready.Load() {
		t.Fatal("manager not ready")
	}
	h.mu.Lock()
	if len(h.names) != 1 || h.names[0] != "test" {
		t.Fatalf("unexpected health checkers: %v", h.names)
	}
	h.mu.Unlock()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type observedController struct {
	controller.Controller
	started chan struct{}
}

func (c *observedController) Run(ctx context.Context, workers int) error {
	close(c.started)
	return c.Controller.Run(ctx, workers)
}

func TestManagerPropagatesSourceError(t *testing.T) {
	want := errors.New("fatal source")
	m, err := New(map[string]InitFunc{}, Dependencies{Client: &clientpkg.Client{}, WatchFactory: &fakeWatchFactory{items: []source.Source{&fakeSource{name: "broken", runErr: want}}}, PollingFactory: &fakePollingFactory{}}, Config{Workers: 1, Controllers: "*", CacheSyncTimeout: time.Second, ShutdownTimeout: time.Second, SlowRetryInitial: time.Second, SlowRetryMax: time.Minute, SlowRetryJitter: 0.2}, &fakeHealth{})
	if err != nil {
		t.Fatal(err)
	}
	err = m.Run(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("expected source error, got %v", err)
	}
}

func TestSelectControllers(t *testing.T) {
	noop := func(context.Context, InitContext) (controller.Controller, bool, error) { return nil, false, nil }
	selected, err := selectControllers("*,-b", map[string]InitFunc{"a": noop, "b": noop})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected["a"] == nil {
		t.Fatalf("unexpected selection: %v", selected)
	}
	if _, err := selectControllers("missing", map[string]InitFunc{"a": noop}); err == nil {
		t.Fatal("unknown controller must fail")
	}
}
