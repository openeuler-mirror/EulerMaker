package rpmrepo

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clocktesting "k8s.io/utils/clock/testing"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// stubPollingFactory captures the resource, period and selectors the initializer asks for.
type stubPollingFactory struct {
	resources []schema.GroupVersionResource
	selectors []string
	source    source.Source
	err       error
	gvr       schema.GroupVersionResource
	period    time.Duration
	options   metav1.ListOptions
	calls     int
}

func (f *stubPollingFactory) ForResource(gvr schema.GroupVersionResource, period time.Duration, options metav1.ListOptions) (source.Source, error) {
	f.resources = append(f.resources, gvr)
	f.selectors = append(f.selectors, options.FieldSelector)
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

func initContext(factory source.PollingSourceFactory) manager.InitContext {
	return manager.InitContext{
		Dependencies: manager.Dependencies{Client: &clientpkg.Client{}, PollingFactory: factory},
		Config: manager.ControllerConfig{
			Workers:          6,
			SlowRetryInitial: 30 * time.Second,
			SlowRetryMax:     15 * time.Minute,
			SlowRetryJitter:  0.2,
		},
	}
}

func TestInitializerBuildsThePollingSource(t *testing.T) {
	factory := &stubPollingFactory{}
	config := testConfig()

	instance, active, err := Initializer(config, NewFakeClient())(context.Background(), initContext(factory))
	if err != nil {
		t.Fatalf("Initializer: %v", err)
	}
	if !active || instance == nil || instance.Name() != Name {
		t.Fatalf("the initializer must return an active %q controller, got %v active=%t", Name, instance, active)
	}
	resources := []schema.GroupVersionResource{source.RpmReposGVR, source.BuildInfosGVR, source.RpmReposGVR}
	if factory.calls != len(resources) || !slices.Equal(factory.resources, resources) {
		t.Fatalf("unexpected polling resources %v", factory.resources)
	}
	if factory.period != config.PollPeriod {
		t.Fatalf("unexpected poll period %s", factory.period)
	}
	selectors := []string{expectedNonTerminalSelector, "metadata.deletionTimestamp!=null", "metadata.deletionTimestamp!=null"}
	if !slices.Equal(factory.selectors, selectors) {
		t.Fatalf("unexpected field selectors %v", factory.selectors)
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
			name:   "zero-retry-limit",
			config: func() Config { c := testConfig(); c.MaterializeRetryLimit = 0; return c }(),
			init:   initContext(&stubPollingFactory{}),
		},
		{
			name:   "invalid-backoff",
			config: func() Config { c := testConfig(); c.Backoff = Backoff{}; return c }(),
			init:   initContext(&stubPollingFactory{}),
		},
		{
			name:   "zero-slow-retry",
			config: testConfig(),
			init: func() manager.InitContext {
				ctx := initContext(&stubPollingFactory{})
				ctx.Config.SlowRetryInitial = 0
				return ctx
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Initializer(tc.config, NewFakeClient())(context.Background(), tc.init); err == nil {
				t.Fatalf("invalid configuration must fail initialization")
			}
		})
	}
}

func TestInitializerPropagatesPollingFactoryErrors(t *testing.T) {
	factory := &stubPollingFactory{err: errors.New("source unavailable")}
	if _, _, err := Initializer(testConfig(), NewFakeClient())(context.Background(), initContext(factory)); err == nil {
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
	if item != rpmRepoKey(testProject, testBuild) {
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
	for _, phase := range []ebsv1.RpmRepoReleasePhase{
		ebsv1.RpmRepoReleaseReady,
		ebsv1.RpmRepoReleaseFailed,
		ebsv1.RpmRepoReleaseSkipped,
	} {
		t.Run(string(phase), func(t *testing.T) {
			terminal := newRpmRepo(testBuild)
			terminal.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: phase}
			c.onAdd(terminal)
			c.onUpdate(terminal, terminal)
			if c.Queue().Len() != 0 {
				t.Fatalf("%s repository enqueued work", phase)
			}
		})
	}
	c.onAdd(&ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: testBuild, Namespace: testProject}})
	if c.Queue().Len() != 0 {
		t.Fatalf("terminal and foreign objects must not enqueue work")
	}
	c.onDelete(newRpmRepo(testBuild))
	if c.Queue().Len() != 0 {
		t.Fatalf("delete events must only be logged")
	}
}

func TestEventHandlerEnqueuesDeletingTerminalRepository(t *testing.T) {
	c := handlerController(t)
	repo := newRpmRepo(testBuild)
	now := metav1.Now()
	repo.DeletionTimestamp = &now
	repo.Finalizers = []string{ebsv1.RpmRepoCleanupFinalizer}
	repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseReady}
	repo.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionCleanupReady, Status: metav1.ConditionTrue}}
	c.onAdd(repo)
	if c.Queue().Len() != 1 {
		t.Fatal("deletion must take precedence over terminal phase and cleanup acknowledgment")
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

// expectedNonTerminalSelector excludes all release terminal phases.
const expectedNonTerminalSelector = "status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Skipped"

func TestReleaseTerminalIncludesSkipped(t *testing.T) {
	cases := []struct {
		name string
		repo *ebsv1.RpmRepo
		want bool
	}{
		{name: "no-release", repo: newRpmRepo(testBuild), want: false},
		{
			name: "ready",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseReady}
				return repo
			}(),
			want: true,
		},
		{
			name: "failed",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
				return repo
			}(),
			want: true,
		},
		{
			name: "skipped",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}
				return repo
			}(),
			want: true,
		},
		{
			name: "pending",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePending}
				return repo
			}(),
			want: false,
		},
		{
			name: "creating",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseCreating}
				return repo
			}(),
			want: false,
		},
		{
			name: "prepared",
			repo: func() *ebsv1.RpmRepo {
				repo := newRpmRepo(testBuild)
				repo.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePrepared}
				return repo
			}(),
			want: false,
		},
		{name: "nil-object", repo: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := releaseTerminal(tc.repo); got != tc.want {
				t.Fatalf("releaseTerminal = %t, want %t", got, tc.want)
			}
		})
	}
}

// failingSource refuses handler registration so the constructor error path can be exercised.
type failingSource struct{}

func (failingSource) Name() string { return "rpmrepos" }

func (failingSource) AddEventHandler(source.ResourceEventHandler) error {
	return source.ErrSourceStarted
}

func (failingSource) Run(ctx context.Context) error { <-ctx.Done(); return nil }

func (failingSource) HasSynced() bool { return true }

func (failingSource) Ready() bool { return true }

func TestNewValidatesDependenciesAndConfig(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	valid := testConfig()
	src := &stubSource{}
	client := NewFakeClient()
	artifacts := NewFakeArtifactManager()
	policy := DefaultPublishPolicy{}
	clk := clocktesting.NewFakeClock(now)

	for _, tc := range []struct {
		name string
		call func() (*Controller, error)
	}{
		{name: "nil-source", call: func() (*Controller, error) { return New(nil, client, artifacts, policy, clk, valid) }},
		{name: "nil-client", call: func() (*Controller, error) { return New(src, nil, artifacts, policy, clk, valid) }},
		{name: "nil-artifacts", call: func() (*Controller, error) { return New(src, client, nil, policy, clk, valid) }},
		{name: "nil-policy", call: func() (*Controller, error) { return New(src, client, artifacts, nil, clk, valid) }},
		{name: "nil-clock", call: func() (*Controller, error) { return New(src, client, artifacts, policy, nil, valid) }},
		{
			name: "negative-retries",
			call: func() (*Controller, error) {
				config := valid
				config.MaxRetries = -1
				return New(src, client, artifacts, policy, clk, config)
			},
		},
		{
			name: "handler-registration",
			call: func() (*Controller, error) { return New(failingSource{}, client, artifacts, policy, clk, valid) },
		},
		{
			name: "invalid-option",
			call: func() (*Controller, error) {
				return New(src, client, artifacts, policy, clk, valid, nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.call(); err == nil {
				t.Fatalf("invalid construction must fail")
			}
		})
	}
	if _, err := New(src, client, artifacts, policy, clk, valid); err != nil {
		t.Fatalf("a valid construction was rejected: %v", err)
	}
}

func TestConfigValidationBranches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "timeout", mutate: func(c *Config) { c.ArtifactManagerTimeout = 0 }},
		{name: "jobs", mutate: func(c *Config) { c.MaxJobsPerBatch = 0 }},
		{name: "retry-limit", mutate: func(c *Config) { c.MaterializeRetryLimit = 0 }},
		{name: "poll-period", mutate: func(c *Config) { c.PollPeriod = 0 }},
		{name: "backoff-initial", mutate: func(c *Config) { c.Backoff.Initial = 0 }},
		{name: "backoff-order", mutate: func(c *Config) { c.Backoff.Max = c.Backoff.Initial / 2 }},
		{name: "backoff-jitter", mutate: func(c *Config) { c.Backoff.Jitter = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := testConfig()
			tc.mutate(&config)
			if err := config.validate(); err == nil {
				t.Fatalf("invalid configuration must be rejected")
			}
		})
	}
}

func TestSplitKey(t *testing.T) {
	cases := []struct {
		key     string
		wantOK  bool
		project string
		name    string
	}{
		{key: rpmRepoKey(testProject, testBuild), wantOK: true, project: testProject, name: testBuild},
		{key: "build/project", wantOK: true, project: "build", name: "project"},
		{key: "other/project/build-a"},
		{key: "build//build-a"},
		{key: "build/project/"},
		{key: "build/project/build-a/extra"},
		{key: "project/"},
		{key: "/build-a"},
		{key: "release/project/openEuler"},
		{key: ""},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			project, name, ok := splitKey(tc.key)
			if ok != tc.wantOK {
				t.Fatalf("splitKey(%q) ok = %t, want %t", tc.key, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if project != tc.project || name != tc.name {
				t.Fatalf("unexpected split %q %q", project, name)
			}
		})
	}
}

func TestControllerStartupHelpers(t *testing.T) {
	c := handlerController(t)
	if c.Name() != Name {
		t.Fatalf("unexpected controller name %q", c.Name())
	}
	c.onDelete(&ebsv1.Build{})
	if c.Queue().Len() != 0 {
		t.Fatalf("a foreign delete event must not enqueue work")
	}
	var _ source.Source = &stubSource{}
	var _ controller.Controller = c
}

type stubSource struct {
	handler source.ResourceEventHandler
}

func (s *stubSource) Name() string { return "rpmrepos" }

func (s *stubSource) AddEventHandler(handler source.ResourceEventHandler) error {
	s.handler = handler
	return nil
}

func (s *stubSource) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (s *stubSource) HasSynced() bool { return true }

func (s *stubSource) Ready() bool { return true }
