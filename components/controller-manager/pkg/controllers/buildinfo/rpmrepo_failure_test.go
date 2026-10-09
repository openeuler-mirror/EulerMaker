package buildinfo

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"controller-manager/pkg/controllers/buildinfo/rpmver"
	"controller-manager/pkg/controllers/buildinfo/specparse"
	ebsv1 "ebs-api/ebs/v1"
)

func seedRpmRepoMetadataRound(t *testing.T, c *Controller, client *fakeClient) string {
	t.Helper()
	bi := testBuildInfoObj(ebsv1.BuildInfoProcessing)
	bi.Status.Dcg = map[string]ebsv1.DcgNodeState{"a": {Version: "1.0-1"}}
	bi.Status.SpecStatus = ebsv1.NewSpecStatusGroup(map[string]ebsv1.SpecStatus{
		"a": {Build: ebsv1.SpecBuildStatus{Status: SpecBuildRunning}, DispatchCount: 1},
	})
	seeded := seedProcessingRound(client, c, bi, map[string]specparse.SpecDepend{"a": dependEntry("a")}, &rpmver.RpmMetaSources{})
	seedJobAt(client, seeded, "a", 1, ebsv1.JobRunning, testStart)
	return testNS + "/" + testBuild
}

func TestRpmRepoDownloadFailureRetriesUntilRecovery(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := seedRpmRepoMetadataRound(t, c, client)
	oldFetch := rpmMetaFetch
	t.Cleanup(func() { rpmMetaFetch = oldFetch })
	rpmMetaFetch = func(context.Context, string) ([]byte, error) { return nil, errors.New("temporary download failure") }

	for i := 0; i < c.config.RpmRepoReadyRetryLimit+2; i++ {
		reconcileOnce(t, c)
		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
		requireNoCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable)
	}
	if got := c.counters.Count(counterRpmRepo, key); got != 0 {
		t.Fatalf("parse failure count = %d, want 0 after download failures", got)
	}
	c.rpmMetaSources.Set(key, testSources())
	reconcileOnce(t, c)
	requireNoCondition(t, getBuildInfo(t, client).Status.Conditions, ConditionRpmRepoUnavailable)
}

func TestRpmRepoPersistentParseFailureStopsWithReason(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seedRpmRepoMetadataRound(t, c, client)
	oldFetch := rpmMetaFetch
	t.Cleanup(func() { rpmMetaFetch = oldFetch })
	rpmMetaFetch = func(context.Context, string) ([]byte, error) { return []byte("<invalid"), nil }

	for i := 1; i <= c.config.RpmRepoReadyRetryLimit; i++ {
		reconcileOnce(t, c)
		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoProcessing)
		if i < c.config.RpmRepoReadyRetryLimit {
			requireNoCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable)
		} else {
			requireCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoXMLParseFailed)
			if got := findCondition(persisted.Status.Conditions, ConditionRpmRepoUnavailable).Message; !strings.Contains(got, "repomd.xml") || !strings.Contains(got, "consecutive failures: 3") {
				t.Fatalf("condition message = %q, want source and failure count", got)
			}
		}
	}
}

func TestRpmRepoDownloadFailureBreaksParseFailureStreak(t *testing.T) {
	c, client, _, _ := newTestController(t)
	key := seedRpmRepoMetadataRound(t, c, client)
	oldFetch := rpmMetaFetch
	t.Cleanup(func() { rpmMetaFetch = oldFetch })
	parseFailure := true
	rpmMetaFetch = func(context.Context, string) ([]byte, error) {
		if parseFailure {
			return []byte("<invalid"), nil
		}
		return nil, errors.New("temporary download failure")
	}

	for i := 0; i < c.config.RpmRepoReadyRetryLimit-1; i++ {
		reconcileOnce(t, c)
	}
	if got := c.counters.Count(counterRpmRepo, key); got != c.config.RpmRepoReadyRetryLimit-1 {
		t.Fatalf("parse failure count = %d, want %d", got, c.config.RpmRepoReadyRetryLimit-1)
	}
	parseFailure = false
	reconcileOnce(t, c)
	if got := c.counters.Count(counterRpmRepo, key); got != 0 {
		t.Fatalf("parse failure count = %d, want 0 after download failure", got)
	}
	parseFailure = true
	reconcileOnce(t, c)
	requireNoCondition(t, getBuildInfo(t, client).Status.Conditions, ConditionRpmRepoUnavailable)
}

func TestRpmRepoInvalidURLStopsImmediately(t *testing.T) {
	c, client, _, _ := newTestController(t)
	seedRpmRepoMetadataRound(t, c, client)
	client.SeedRpmRepo(testRpmRepoObj("ftp://invalid/repository"))
	reconcileOnce(t, c)
	persisted := getBuildInfo(t, client)
	requireCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoConfigInvalid)
	if got := findCondition(persisted.Status.Conditions, ConditionRpmRepoUnavailable).Message; !strings.Contains(got, "unsupported repository URL scheme") {
		t.Fatalf("condition message = %q, want configuration error", got)
	}
}

func TestBootstrapRepoFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		name       string
		repoURL    string
		wantReason string
	}{
		{"invalid URL", "ftp://invalid/bootstrap", ReasonBootstrapRepoConfigInvalid},
		{"persistent XML parse failure", "https://example.test/bootstrap", ReasonBootstrapRepoXMLParseFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			seedRpmRepoMetadataRound(t, c, client)
			bi := getBuildInfo(t, client)
			bi.Spec.BootstrapRepo = []ebsv1.BootstrapRepo{{Repo: tc.repoURL}}
			client.SeedBuildInfo(bi)
			client.SeedRpmRepo(testRpmRepoObj(""))
			oldFetch := rpmMetaFetch
			t.Cleanup(func() { rpmMetaFetch = oldFetch })
			rpmMetaFetch = func(context.Context, string) ([]byte, error) { return []byte("<invalid"), nil }
			attempts := 1
			if tc.wantReason == ReasonBootstrapRepoXMLParseFailed {
				attempts = c.config.RpmRepoReadyRetryLimit
			}
			for i := 0; i < attempts; i++ {
				reconcileOnce(t, c)
			}
			requireCondition(t, getBuildInfo(t, client).Status.Conditions, ConditionRpmRepoUnavailable, tc.wantReason)
		})
	}
}

func TestRpmRepoQueryFailureClassification(t *testing.T) {
	t.Run("temporary network failure", func(t *testing.T) {
		c, client, _, _ := newTestController(t)
		key := seedRpmRepoMetadataRound(t, c, client)
		for i := 0; i < c.config.RpmRepoReadyRetryLimit+2; i++ {
			client.InjectRead("rpmrepos", 1, errors.New("network unavailable"))
			if _, err := c.reconcile(context.Background(), key); err == nil {
				t.Fatal("temporary query failure must retry")
			}
			requireNoCondition(t, getBuildInfo(t, client).Status.Conditions, ConditionRpmRepoUnavailable)
		}
		if got := c.counters.Count(counterRpmRepo, key); got != 0 {
			t.Fatalf("parse failure count = %d, want 0 after query failures", got)
		}
	})
	t.Run("forbidden query", func(t *testing.T) {
		c, client, _, _ := newTestController(t)
		key := seedRpmRepoMetadataRound(t, c, client)
		client.InjectRead("rpmrepos", 1, apierrors.NewForbidden(schema.GroupResource{Resource: "rpmrepos"}, testBuild, errors.New("permission denied")))
		if _, err := c.reconcile(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		requireCondition(t, getBuildInfo(t, client).Status.Conditions, ConditionRpmRepoUnavailable, ReasonRpmRepoQueryRejected)
	})
}

func TestRpmRepoMetadataHTTPStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		stops  bool
	}{
		{http.StatusNotFound, false},
		{http.StatusTooManyRequests, false},
		{http.StatusForbidden, true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			c, client, _, _ := newTestController(t)
			seedRpmRepoMetadataRound(t, c, client)
			oldFetch := rpmMetaFetch
			t.Cleanup(func() { rpmMetaFetch = oldFetch })
			rpmMetaFetch = func(_ context.Context, url string) ([]byte, error) {
				return nil, &rpmver.HTTPStatusError{URL: url, StatusCode: tc.status}
			}
			reconcileOnce(t, c)
			condition := findCondition(getBuildInfo(t, client).Status.Conditions, ConditionRpmRepoUnavailable)
			if tc.stops {
				if condition == nil || condition.Reason != ReasonRpmRepoConfigInvalid {
					t.Fatalf("condition = %+v, want configuration failure", condition)
				}
			} else if condition != nil {
				t.Fatalf("condition = %+v, want retry", condition)
			}
		})
	}
}

func TestSinglePreferMetadataDownloadWaitsBeforeDispatch(t *testing.T) {
	c, client, git, _ := newTestController(t)
	client.SeedProject(testProjectObj(ebsv1.ProjectActive))
	client.SeedBuild(testBuildObj("single", "repo1"))
	client.SeedBuildResourceRules(testBuildResourceRules())
	client.SetBuildTargetContent(testBuildTargetContent())
	bi := testBuildInfoObj(ebsv1.BuildInfoPending)
	bi.Spec.BuildPayload = "prefer:\n- rpm-a\n"
	client.SeedBuildInfo(bi)
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	client.SeedRpmRepo(testRpmRepoObj(testRepoURL))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a")})
	oldFetch := rpmMetaFetch
	t.Cleanup(func() { rpmMetaFetch = oldFetch })
	rpmMetaFetch = func(context.Context, string) ([]byte, error) { return nil, errors.New("temporary download failure") }

	for i := 0; i < c.config.RpmRepoReadyRetryLimit+2; i++ {
		result, err := c.reconcile(context.Background(), testNS+"/"+testBuild)
		if err != nil || !result.Requeue {
			t.Fatalf("reconcile = %+v, %v, want retry", result, err)
		}
		persisted := getBuildInfo(t, client)
		requirePhase(t, persisted, ebsv1.BuildInfoPending)
		requireNoCondition(t, persisted.Status.Conditions, ConditionRpmRepoUnavailable)
		if jobs := listJobs(t, client); len(jobs) != 0 {
			t.Fatalf("Jobs = %d, want none before metadata recovers", len(jobs))
		}
	}
	c.rpmMetaSources.Set(testNS+"/"+testBuild, testSources())
	reconcileOnce(t, c)
	requirePhase(t, getBuildInfo(t, client), ebsv1.BuildInfoProcessing)
	if jobs := listJobs(t, client); len(jobs) != 1 {
		t.Fatalf("Jobs = %d, want one after metadata recovers", len(jobs))
	}
}
