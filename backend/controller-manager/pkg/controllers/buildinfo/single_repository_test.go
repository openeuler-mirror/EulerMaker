package buildinfo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

func TestReleaseProbeResponses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		published bool
		wantError bool
		permanent bool
	}{
		{"ready", 200, testRepomd, true, false, false},
		{"unpublished", 404, `{"code":"ReleaseNotPublished"}`, false, false, false},
		{"generic missing", 404, `{"code":"ReleaseNotFound"}`, false, true, false},
		{"broken content", 503, `{"code":"ReleaseContentUnavailable"}`, false, true, false},
		{"forbidden", 403, `{"code":"Forbidden"}`, false, true, true},
		{"throttled", 429, "busy", false, true, false},
		{"truncated XML", 200, "<repomd>", false, true, false},
		{"wrong root", 200, "<html/>", false, true, false},
		{"empty metadata", 200, "<repomd/>", false, true, false},
		{"oversized metadata", 200, strings.Repeat("x", (1<<20)+1), false, true, false},
		{"oversized error", 404, strings.Repeat("x", (64<<10)+1), false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repo/repodata/repomd.xml" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			c := &Controller{releaseClient: newReleaseClient(time.Second)}
			published, err := c.checkReleasePublished(context.Background(), server.URL+"/repo/")
			var permanent controller.PermanentError
			if published != tc.published || (err != nil) != tc.wantError || errors.As(err, &permanent) != tc.permanent {
				t.Fatalf("probe = %v, %v", published, err)
			}
		})
	}
}

func TestReleaseProbeCancellationAndRedirect(t *testing.T) {
	c := &Controller{releaseClient: newReleaseClient(time.Second)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.checkReleasePublished(ctx, "http://example.invalid/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe = %v", err)
	}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cross-origin redirect was followed")
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := c.checkReleasePublished(context.Background(), redirect.URL); err == nil {
		t.Fatal("expected redirect rejection")
	}
}

func TestSingleReleaseInputs(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "bootstrap only", true: "published and bootstrap"}[published], func(t *testing.T) {
			c, client, git, _ := newTestController(t)
			bi := seedHealthyBasics(client, "single", "repo1")
			bi.Spec.BootstrapRepo = []ebsv1.BootstrapRepo{{Name: "base", URL: "https://bootstrap.example/base"}}
			client.SeedBuildInfo(bi)
			project := testProjectObj(ebsv1.ProjectActive)
			project.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}
			client.SeedProject(project)
			client.artifactEndpoints["art-01"] = "http://internal.example"
			client.InjectRead("rpmrepos", 100, errors.New("single must not read RpmRepo"))
			client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
			git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a"), "b.spec": specText("b")})
			probes := 0
			c.releaseClient.Transport = releaseTransportFunc(func(r *http.Request) (*http.Response, error) {
				probes++
				want := "http://internal.example/repositories/" + testNS + "/" + testOS + "/" + testArch + "/repodata/repomd.xml"
				if r.URL.String() != want {
					t.Errorf("probe = %s, want %s", r.URL, want)
				}
				code, body := http.StatusOK, testRepomd
				if !published {
					code, body = http.StatusNotFound, `{"code":"ReleaseNotPublished"}`
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			reconcileOnce(t, c)
			if probes != 1 || client.artifactReads != 1 || len(listJobs(t, client)) != 2 {
				t.Fatalf("probes=%d config reads=%d jobs=%d", probes, client.artifactReads, len(listJobs(t, client)))
			}
			want := []any{map[string]any{"url": "https://bootstrap.example/base/" + testArch, "priority": float64(99)}}
			if published {
				reference := strings.Replace(testReleaseReference, "default", "art-01", 1)
				want = append([]any{map[string]any{"url": reference, "priority": float64(10)}}, want...)
			}
			for _, job := range listJobs(t, client) {
				got := payloadFields(t, job.Spec.Payload)["repo"]
				if !reflect.DeepEqual(got, want) || job.Annotations[ebsv1.ProjectArtifactsAnnotation] != "art-01" {
					t.Fatalf("Job repositories=%v annotations=%v", got, job.Annotations)
				}
				job.Status.Phase = ebsv1.JobSucceeded
				client.SeedJob(&job)
			}
			// Convergence only consumes the existing Jobs; an unavailable release must not block completion.
			reconcileOnce(t, c)
			requirePhase(t, getBuildInfo(t, client), ebsv1.BuildInfoCompleted)
			if probes != 1 {
				t.Fatalf("completion triggered %d probes", probes)
			}
		})
	}
}

func TestSingleReleaseFailurePreservesJobBackfill(t *testing.T) {
	c, client, git, _ := newTestController(t)
	bi := seedHealthyBasics(client, "single", "repo1")
	client.SeedSnapshot(testSnapshotObj(repoEntry{name: "repo1", cloneURL: gitURL1, commitID: "c1", declare: true}))
	git.repo(gitURL1, "c1", map[string]string{"a.spec": specText("a"), "b.spec": specText("b")})
	seedJobAt(client, bi, "a", 1, ebsv1.JobSucceeded, testStart)
	c.releaseClient.Transport = releaseTransportFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("release temporarily unavailable")
	})
	for i := 0; i < 5; i++ {
		if _, err := c.reconcile(context.Background(), testNS+"/"+testBuild); err == nil {
			t.Fatal("expected retryable probe error")
		}
		persisted := getBuildInfo(t, client)
		if persisted.Status.SpecStatus.Entry("a").Build.Status != ebsv1.SpecBuildSucceeded ||
			persisted.Status.SpecStatus.Entry("b").Build.Status != "" {
			t.Fatalf("backfilled statuses = %+v", persisted.Status.SpecStatus)
		}
		requirePhase(t, persisted, ebsv1.BuildInfoPending)
	}
}

func TestSingleUnpublishedRequiresBootstrap(t *testing.T) {
	c, client, _, _ := newTestController(t)
	round := &reconcileRound{current: testBuildInfoObj(ebsv1.BuildInfoPending), build: testBuildObj("single")}
	round.current.Spec.BuildPayload = "repo: https://ignored.example/"
	c.releaseClient.Transport = releaseTransportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound,
			Body: io.NopCloser(strings.NewReader(`{"code":"ReleaseNotPublished"}`))}, nil
	})
	_, err := c.singleContentURL(context.Background(), round)
	var permanent controller.PermanentError
	if !errors.As(err, &permanent) || !strings.Contains(err.Error(), "BootstrapRepoRequired") || client.artifactReads != 1 {
		t.Fatalf("unpublished without bootstrap = %v", err)
	}
}

func TestSinglePreferMetadataIsScopedToReconcile(t *testing.T) {
	c, _, _, _ := newTestController(t)
	oldFetch := rpmMetaFetch
	t.Cleanup(func() { rpmMetaFetch = oldFetch })
	reads := 0
	rpmMetaFetch = func(ctx context.Context, url string) ([]byte, error) {
		reads++
		return singleMetadataFixture(ctx, url)
	}
	key := testNS + "/" + testBuild
	for i := 0; i < 2; i++ {
		round := &reconcileRound{
			key: key, current: testBuildInfoObj(ebsv1.BuildInfoPending),
			build: testBuildObj("single"), failures: c.newRoundFailures(key),
		}
		first, _, err := c.loadSinglePreferSources(context.Background(), round, "http://release.example/")
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := c.loadSinglePreferSources(context.Background(), round, "http://release.example/")
		if err != nil || first != second {
			t.Fatalf("round cache was not shared: %v", err)
		}
	}
	if reads != 4 {
		t.Fatalf("metadata reads=%d, want repomd+primary for each round", reads)
	}
}
