package rpmrepo

import (
	"context"
	"errors"
	"testing"
	"time"

	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// stubPollingFactory captures the resource, period and selectors the initializer asks for.
type stubPollingFactory struct {
	source  source.Source
	err     error
	gvr     schema.GroupVersionResource
	period  time.Duration
	options metav1.ListOptions
	calls   int
}

func (f *stubPollingFactory) ForResource(gvr schema.GroupVersionResource, period time.Duration, options metav1.ListOptions) (source.Source, error) {
	f.calls++
	f.gvr, f.period, f.options = gvr, period, options
	if f.err != nil {
		return nil, f.err
	}
	if f.source == nil {
		f.source = &stubSource{}
	}
	return f.source, nil
}

func (f *stubPollingFactory) Sources() []source.Source {
	if f.source == nil {
		return nil
	}
	return []source.Source{f.source}
}

func initializedConfig() Config {
	config := testConfig()
	config.ArtifactManagerAddr = "http://artifact-manager:8080"
	return config
}

func initContext(factory source.PollingSourceFactory) manager.InitContext {
	return manager.InitContext{
		Dependencies: manager.Dependencies{Client: &stubSharedClient{}, PollingFactory: factory},
		Config: manager.ControllerConfig{
			Workers:          6,
			SlowRetryInitial: 30 * time.Second,
			SlowRetryMax:     15 * time.Minute,
			SlowRetryJitter:  0.2,
		},
	}
}

func TestInitializerStaysInactiveWithoutArtifactManager(t *testing.T) {
	factory := &stubPollingFactory{}
	config := testConfig()
	config.ArtifactManagerAddr = ""

	instance, active, err := Initializer(config)(context.Background(), initContext(factory))
	if err != nil {
		t.Fatalf("Initializer: %v", err)
	}
	if active || instance != nil {
		t.Fatalf("an unconfigured controller must stay inactive")
	}
	if factory.calls != 0 {
		t.Fatalf("an inactive controller must not build the polling source")
	}
}

func TestInitializerBuildsThePollingSource(t *testing.T) {
	factory := &stubPollingFactory{}
	config := initializedConfig()

	instance, active, err := Initializer(config)(context.Background(), initContext(factory))
	if err != nil {
		t.Fatalf("Initializer: %v", err)
	}
	if !active || instance == nil || instance.Name() != Name {
		t.Fatalf("the initializer must return an active %q controller, got %v active=%t", Name, instance, active)
	}
	if factory.calls != 1 {
		t.Fatalf("expected one polling source request, got %d", factory.calls)
	}
	if factory.gvr != source.RpmReposGVR {
		t.Fatalf("unexpected GVR %v", factory.gvr)
	}
	if factory.period != config.PollPeriod {
		t.Fatalf("unexpected poll period %s", factory.period)
	}
	if factory.options.FieldSelector != nonTerminalRpmRepoFieldSelector {
		t.Fatalf("unexpected field selector %q", factory.options.FieldSelector)
	}
	if factory.options.LabelSelector != "" {
		t.Fatalf("the polling source must not carry a label selector, got %q", factory.options.LabelSelector)
	}
}

func TestInitializerRejectsBadConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		config Config
		init   manager.InitContext
	}{
		{
			name:   "invalid-address",
			config: func() Config { c := initializedConfig(); c.ArtifactManagerAddr = "ftp://artifact-manager"; return c }(),
			init:   initContext(&stubPollingFactory{}),
		},
		{
			name:   "zero-retry-limit",
			config: func() Config { c := initializedConfig(); c.MaterializeRetryLimit = 0; return c }(),
			init:   initContext(&stubPollingFactory{}),
		},
		{
			name:   "invalid-backoff",
			config: func() Config { c := initializedConfig(); c.Backoff = Backoff{}; return c }(),
			init:   initContext(&stubPollingFactory{}),
		},
		{
			name:   "zero-slow-retry",
			config: initializedConfig(),
			init: func() manager.InitContext {
				ctx := initContext(&stubPollingFactory{})
				ctx.Config.SlowRetryInitial = 0
				return ctx
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Initializer(tc.config)(context.Background(), tc.init); err == nil {
				t.Fatalf("invalid configuration must fail initialization")
			}
		})
	}
}

func TestInitializerPropagatesPollingFactoryErrors(t *testing.T) {
	factory := &stubPollingFactory{err: errors.New("source unavailable")}
	if _, _, err := Initializer(initializedConfig())(context.Background(), initContext(factory)); err == nil {
		t.Fatalf("a failing polling factory must fail initialization")
	}
}

func handlerController(t *testing.T) *Controller {
	t.Helper()
	return newTestController(t, NewFakeClient(), NewFakeArtifactManager(), testConfig())
}

func TestEventHandlerEnqueuesTheBuildKey(t *testing.T) {
	c := handlerController(t)
	repo := newRpmRepo(testBuild)
	c.onAdd(repo)
	if c.Queue().Len() != 1 {
		t.Fatalf("Add events must enqueue the build key")
	}
	item, _ := c.Queue().Get()
	if item != buildKey(testProject, testBuild) {
		t.Fatalf("unexpected key %v", item)
	}
	c.Queue().Done(item)
	c.Queue().Forget(item)
	c.onUpdate(repo, repo)
	if c.Queue().Len() != 1 {
		t.Fatalf("Update events must enqueue the build key")
	}
}

func TestEventHandlerSkipsObjectsThatNeedNoWork(t *testing.T) {
	c := handlerController(t)
	terminal := newRpmRepo(testBuild)
	terminal.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseReady}
	deleting := newRpmRepo(testBuild)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	c.onAdd(terminal)
	c.onAdd(deleting)
	c.onAdd(&ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: testBuild, Namespace: testProject}})
	if c.Queue().Len() != 0 {
		t.Fatalf("terminal, deleting and foreign objects must not enqueue work")
	}
	c.onDelete(newRpmRepo(testBuild))
	if c.Queue().Len() != 0 {
		t.Fatalf("delete events must only be logged")
	}
}

func TestEventHandlerIgnoresMalformedObjects(t *testing.T) {
	c := handlerController(t)
	c.onAdd(&ebsv1.RpmRepo{})
	c.onAdd(runtime.Object(nil))
	if c.Queue().Len() != 0 {
		t.Fatalf("malformed events must not enqueue work")
	}
}
