package rpmrepo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clocktesting "k8s.io/utils/clock/testing"
)

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
		{name: "empty-address", mutate: func(c *Config) { c.ArtifactManagerAddr = "" }},
		{name: "bad-scheme", mutate: func(c *Config) { c.ArtifactManagerAddr = "ftp://artifact-manager" }},
		{name: "no-host", mutate: func(c *Config) { c.ArtifactManagerAddr = "http://" }},
		{name: "unparsable", mutate: func(c *Config) { c.ArtifactManagerAddr = "://bad" }},
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
			config := initializedConfig()
			tc.mutate(&config)
			if err := config.validate(); err == nil {
				t.Fatalf("invalid configuration must be rejected")
			}
		})
	}
}

func TestSplitKeyBranches(t *testing.T) {
	cases := []struct {
		key      string
		wantOK   bool
		wantKind string
		wantRest int
	}{
		{key: buildKey(testProject, testBuild), wantOK: true, wantKind: buildKeyPrefix, wantRest: 1},
		{key: releaseKey(testProject, testOS, testArch), wantOK: true, wantKind: releaseKeyPrefix, wantRest: 2},
		{key: "build/project"},
		{key: "other/project/build-a"},
		{key: "build//build-a"},
		{key: "build/project/"},
		{key: "build/project/build-a/extra"},
		{key: "release/project/openEuler"},
		{key: ""},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			kind, project, rest, ok := splitKey(tc.key)
			if ok != tc.wantOK {
				t.Fatalf("splitKey(%q) ok = %t, want %t", tc.key, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if kind != tc.wantKind || project != testProject || len(rest) != tc.wantRest {
				t.Fatalf("unexpected split %q %q %v", kind, project, rest)
			}
		})
	}
}

func TestAdapterReadsSucceedForMatchingObjects(t *testing.T) {
	build := newBuild(testBuild)
	info := newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	shared := &stubSharedClient{getResults: []runtime.Object{build, info}}
	client := newAPIClient(shared)
	if _, err := client.GetBuild(context.Background(), testProject, testBuild); err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if _, err := client.GetBuildInfo(context.Background(), testProject, testBuild); err != nil {
		t.Fatalf("GetBuildInfo: %v", err)
	}
}

func TestBatchHelpersHandleTiesAndInvalidInput(t *testing.T) {
	// Equal creation timestamps fall back to name and then UID.
	candidates := []candidate{
		{name: "job-b", uid: "uid-2", specName: "kernel", createdAt: 5},
		{name: "job-a", uid: "uid-3", specName: "gcc", createdAt: 5},
		{name: "job-a", uid: "uid-1", specName: "glibc", createdAt: 5},
		{name: "job-a", uid: "uid-1", specName: "extra", createdAt: 1},
	}
	sortCandidates(candidates)
	for i := 1; i < len(candidates); i++ {
		previous, current := candidates[i-1], candidates[i]
		if previous.createdAt > current.createdAt {
			t.Fatalf("candidates are not ordered by creation time: %+v", candidates)
		}
		if previous.createdAt == current.createdAt && previous.name > current.name {
			t.Fatalf("candidates are not ordered by name: %+v", candidates)
		}
	}

	if _, err := repositoryUID("", testBuild, "", []ebsv1.RepositoryInput{{JobUID: "uid-a"}}); err == nil {
		t.Fatalf("an empty project must be rejected")
	}
	if _, err := repositoryUID(testProject, "", "", []ebsv1.RepositoryInput{{JobUID: "uid-a"}}); err == nil {
		t.Fatalf("an empty build name must be rejected")
	}
	if got := unionSortedUIDs([]string{"", "uid-a"}, []ebsv1.RepositoryInput{{JobUID: ""}, {JobUID: "uid-b"}}); len(got) != 2 {
		t.Fatalf("empty UIDs must be skipped, got %v", got)
	}
}

func TestArtifactErrorFormattingIsStable(t *testing.T) {
	cause := errors.New("boom")
	failure := &artifactError{operation: "get-repository", kind: artifactRetryable, code: "RepositoryInternalError", statusCode: 500, err: cause}
	if !errors.Is(failure, cause) {
		t.Fatalf("artifactError must unwrap to its cause")
	}
	for _, want := range []string{"get-repository", "Retryable", "RepositoryInternalError", "500", "boom"} {
		if !strings.Contains(failure.Error(), want) {
			t.Fatalf("error %q must mention %q", failure.Error(), want)
		}
	}
}

func TestFakeArtifactManagerReportsMissingScripts(t *testing.T) {
	fake := NewFakeArtifactManager()
	ctx := context.Background()
	if _, err := fake.SubmitRepository(ctx, CreateRepositoryRequest{Manifests: []ManifestReference{{JobUID: "uid"}}}); err == nil {
		t.Fatalf("an unscripted SubmitRepository must fail")
	}
	if _, err := fake.GetRepository(ctx, "repo-1"); err == nil {
		t.Fatalf("an unscripted GetRepository must fail")
	}
	if _, err := fake.SubmitRelease(ctx, CreateReleaseRequest{BuildName: testBuild}); err == nil {
		t.Fatalf("an unscripted SubmitRelease must fail")
	}
	if _, err := fake.GetRelease(ctx, testBuild); err == nil {
		t.Fatalf("an unscripted GetRelease must fail")
	}
	if _, err := fake.ActivateRelease(ctx, testBuild); err == nil {
		t.Fatalf("an unscripted ActivateRelease must fail")
	}
	if _, err := fake.GetJobManifest(ctx, testProject, "job-a", "uid"); !isArtifactNotFound(err) {
		t.Fatalf("an unscripted manifest lookup must look like a missing manifest, got %v", err)
	}
}

func TestFakeClientErrorInjection(t *testing.T) {
	client := NewFakeClient()
	client.RpmRepos[key(testProject, testBuild)] = newRpmRepo(testBuild)
	client.Builds[key(testProject, testBuild)] = newBuild(testBuild)
	client.BuildInfos[key(testProject, testBuild)] = newBuildInfo(testBuild, ebsv1.BuildInfoCompleted)
	client.GetBuildErr = errors.New("build unavailable")
	client.GetBuildInfoErr = errors.New("buildinfo unavailable")
	client.GetRpmRepoErr = errors.New("rpmrepo unavailable")
	client.ListJobsErr = errors.New("jobs unavailable")
	client.ListRpmReposErr = errors.New("rpmrepos unavailable")
	client.UpdateStatusErr = errors.New("status unavailable")
	ctx := context.Background()
	if _, err := client.GetBuild(ctx, testProject, testBuild); err == nil {
		t.Fatalf("injected Build error must surface")
	}
	if _, err := client.GetBuildInfo(ctx, testProject, testBuild); err == nil {
		t.Fatalf("injected BuildInfo error must surface")
	}
	if _, err := client.GetRpmRepo(ctx, testProject, testBuild); err == nil {
		t.Fatalf("injected RpmRepo error must surface")
	}
	if _, err := client.ListJobs(ctx, testProject, metav1.ListOptions{}); err == nil {
		t.Fatalf("injected Job list error must surface")
	}
	if _, err := client.ListRpmRepos(ctx, testProject, metav1.ListOptions{}); err == nil {
		t.Fatalf("injected RpmRepo list error must surface")
	}
	if _, err := client.UpdateRpmRepoStatus(ctx, newRpmRepo(testBuild)); err == nil {
		t.Fatalf("injected status error must surface")
	}
}

func TestFakeClientGetBuildForwardsTheCallerContext(t *testing.T) {
	client := NewFakeClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var seen error
	client.GetBuildFunc = func(hookCtx context.Context, _, _ string) (*ebsv1.Build, error) {
		seen = hookCtx.Err()
		return nil, hookCtx.Err()
	}
	if _, err := client.GetBuild(ctx, testProject, testBuild); !errors.Is(err, context.Canceled) {
		t.Fatalf("the hook error must surface, got %v", err)
	}
	if !errors.Is(seen, context.Canceled) {
		t.Fatalf("the scripted lookup must see the caller context, got %v", seen)
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
