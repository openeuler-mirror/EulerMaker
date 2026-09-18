package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const seededRepositoryUID = "DB3A8CE2-00CD-4C89-9C20-ADA417C83155"

func withBaseBuildRefName(build *ebsv1.Build, name string) *ebsv1.Build {
	build.Status.BaseBuildRef = &ebsv1.BaseBuildRef{Name: name}
	return build
}

func historicalRpmRepo(project, name, repositoryUID, contentURL string) *ebsv1.RpmRepo {
	return &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
		Status: ebsv1.RpmRepoStatus{Repository: &ebsv1.RpmRepoRepositoryStatus{
			Phase: ebsv1.RpmRepoReady, RepositoryUID: repositoryUID, ContentURL: contentURL,
		}},
	}
}

func repositoryContentURL(repositoryUID string) string {
	return "https://artifact-manager/repositories/v1/" + repositoryUID
}

func TestPendingSeedsRpmRepoBaseFromHistory(t *testing.T) {
	for _, buildType := range []string{"full", "incremental", "specified"} {
		t.Run(buildType, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", buildType, []string{"gcc"}), "build-prev")
			api.storeSnapshot(activeSnapshot("project-a", "build-a"))
			api.storeRpmRepo(historicalRpmRepo("project-a", "build-prev", seededRepositoryUID, repositoryContentURL(seededRepositoryUID)))
			c := newTestController(t, api, newTestClock())

			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if got := api.CallCount("GetRpmRepo project-a/build-prev"); got != 1 {
				t.Fatalf("historical reads = %d", got)
			}
			created := api.rpmRepo("project-a", "build-a")
			if created == nil {
				t.Fatal("RpmRepo was not created")
			}
			if created.Spec != (ebsv1.RpmRepoSpec{}) {
				t.Fatalf("spec = %+v", created.Spec)
			}
			if created.Status.Repository == nil {
				t.Fatal("repository status is missing")
			}
			if created.Status.Repository.Phase != ebsv1.RpmRepoPending {
				t.Fatalf("phase = %q", created.Status.Repository.Phase)
			}
			if created.Status.Repository.RepositoryUID != seededRepositoryUID || created.Status.Repository.ContentURL != repositoryContentURL(seededRepositoryUID) {
				t.Fatalf("seeded base = %+v", created.Status.Repository)
			}
			if created.Status.Release != nil || len(created.Status.Conditions) != 0 {
				t.Fatalf("only the seeded base must survive creation: %+v", created.Status)
			}
			if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared {
				t.Fatalf("phase = %q", stored.Status.Phase)
			}
		})
	}
}

func TestPendingSkipsRpmRepoBaseWithoutHistory(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "incremental", []string{"gcc"}))
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("GetRpmRepo project-a/build-a") != 1 || api.CallCount("GetRpmRepo") != 1 {
		t.Fatalf("RpmRepo reads = %d", api.CallCount("GetRpmRepo"))
	}
	created := api.rpmRepo("project-a", "build-a")
	if created == nil || created.Status.Repository == nil {
		t.Fatal("RpmRepo status is missing")
	}
	if created.Status.Repository.RepositoryUID != "" || created.Status.Repository.ContentURL != "" {
		t.Fatalf("seeded base = %+v", created.Status.Repository)
	}
	if created.Status.Repository.Phase != ebsv1.RpmRepoPending {
		t.Fatalf("phase = %q", created.Status.Repository.Phase)
	}
}

func TestPendingSkipsRpmRepoBaseForSingleBuild(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", "single", []string{"gcc"}), "build-prev")
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("GetRpmRepo") != 0 || api.CallCount("CreateRpmRepo") != 0 {
		t.Fatal("single builds must not read or create the RpmRepo of this round")
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared {
		t.Fatalf("phase = %q", stored.Status.Phase)
	}
}

func TestPendingDegradesWhenHistoricalRpmRepoUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeAPI)
	}{
		{name: "not found", setup: func(*fakeAPI) {}},
		{name: "repository status missing", setup: func(api *fakeAPI) {
			api.storeRpmRepo(&ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "build-prev", Namespace: "project-a"}})
		}},
		{name: "repository uid missing", setup: func(api *fakeAPI) {
			api.storeRpmRepo(historicalRpmRepo("project-a", "build-prev", "", repositoryContentURL(seededRepositoryUID)))
		}},
		{name: "content url missing", setup: func(api *fakeAPI) {
			api.storeRpmRepo(historicalRpmRepo("project-a", "build-prev", seededRepositoryUID, ""))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", "incremental", []string{"gcc"}), "build-prev")
			api.storeSnapshot(activeSnapshot("project-a", "build-a"))
			tc.setup(api)
			c := newTestController(t, api, newTestClock())

			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)

			result, err := c.sync(context.Background(), "project-a/build-a")
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if api.CallCount("CreateRpmRepo") != 1 {
				t.Fatalf("create calls = %d", api.CallCount("CreateRpmRepo"))
			}
			created := api.rpmRepo("project-a", "build-a")
			if created == nil || created.Status.Repository == nil {
				t.Fatal("RpmRepo status is missing")
			}
			if created.Status.Repository.RepositoryUID != "" || created.Status.Repository.ContentURL != "" {
				t.Fatalf("seeded base = %+v", created.Status.Repository)
			}
			if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared || !stored.Status.EndTime.IsZero() {
				t.Fatalf("status = %+v", stored.Status)
			}
			logged := output.String()
			if !strings.Contains(logged, "reason=BaseRepositoryUnavailable") || !strings.Contains(logged, `base_build="build-prev"`) {
				t.Fatalf("missing degradation log: %q", logged)
			}
		})
	}
}

func TestPendingStopsWhenContextIsCanceledDuringBaseLookup(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", "incremental", []string{"gcc"}), "build-prev")
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.hooks.getRpmRepo = func(project, name string) (*ebsv1.RpmRepo, error) {
		if name == "build-prev" {
			cancel()
			return nil, context.Canceled
		}
		return nil, apierrors.NewNotFound(rpmReposResource, name)
	}
	c := newTestController(t, api, newTestClock())

	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)

	_, err := c.sync(ctx, "project-a/build-a")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if api.CallCount("GetRpmRepo project-a/build-prev") != 1 {
		t.Fatal("the canceled round must have reached the historical base lookup")
	}
	if api.CallCount("CreateRpmRepo") != 0 {
		t.Fatal("a canceled round must not create the RpmRepo")
	}
	if api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("a canceled round must not write Build.status")
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPending {
		t.Fatalf("phase = %q", stored.Status.Phase)
	}
	if logged := output.String(); strings.Contains(logged, "BaseRepositoryUnavailable") {
		t.Fatalf("a canceled round must not report a degraded baseline: %q", logged)
	}
}

func TestPendingDoesNotReReadBaseAfterRestart(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", "incremental", []string{"gcc"}), "build-prev")
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	api.storeRpmRepo(historicalRpmRepo("project-a", "build-prev", seededRepositoryUID, repositoryContentURL(seededRepositoryUID)))
	api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
		return nil, &clientpkg.WriteError{Operation: "update-status", Outcome: clientpkg.WriteRejected, StatusCode: 409, Err: errors.New("conflict")}
	}

	first := newTestController(t, api, newTestClock())
	result, err := first.sync(context.Background(), "project-a/build-a")
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("CreateRpmRepo") != 1 {
		t.Fatalf("create calls = %d", api.CallCount("CreateRpmRepo"))
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPending {
		t.Fatalf("phase = %q", stored.Status.Phase)
	}

	// A controller restarted after the status write failed reuses the RpmRepo created before the crash and must
	// not resolve or rewrite the inherited base again.
	api.hooks.updateStatus = nil
	restarted := newTestController(t, api, newTestClock())
	if _, err := restarted.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := api.CallCount("CreateRpmRepo"); got != 1 {
		t.Fatalf("create calls = %d", got)
	}
	if got := api.CallCount("GetRpmRepo project-a/build-prev"); got != 1 {
		t.Fatalf("historical reads = %d", got)
	}
	stored := api.rpmRepo("project-a", "build-a")
	if stored.Status.Repository.RepositoryUID != seededRepositoryUID || stored.Status.Repository.ContentURL != repositoryContentURL(seededRepositoryUID) {
		t.Fatalf("existing RpmRepo was overwritten: %+v", stored.Status.Repository)
	}
	if updated := api.build("project-a", "build-a"); updated.Status.Phase != ebsv1.BuildPrepared {
		t.Fatalf("phase = %q", updated.Status.Phase)
	}
}

// TestPendingRetriesWhenHistoricalRpmRepoReadFails pins the split between "no usable baseline" and "the lookup
// itself failed": only the former degrades, so a transient read error can never silently drop the inherited
// baseline. Permanent API rejections and response contract violations keep their permanent classification.
func TestPendingRetriesWhenHistoricalRpmRepoReadFails(t *testing.T) {
	for _, tc := range []struct {
		name          string
		readErr       error
		wantPermanent bool
	}{
		{name: "connection reset", readErr: errors.New("connection reset")},
		{name: "request timeout", readErr: context.DeadlineExceeded},
		{name: "network error", readErr: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}},
		{name: "server error", readErr: apierrors.NewInternalError(errors.New("boom"))},
		{name: "too many requests", readErr: apierrors.NewTooManyRequests("slow down", 1)},
		{name: "unavailable", readErr: apierrors.NewServiceUnavailable("down")},
		{
			name:          "forbidden",
			readErr:       apierrors.NewForbidden(schema.GroupResource{Group: "ebs", Resource: "rpmrepos"}, "build-prev", errors.New("denied")),
			wantPermanent: true,
		},
		{
			name:          "invalid",
			readErr:       apierrors.NewInvalid(schema.GroupKind{Group: "ebs", Kind: "RpmRepo"}, "build-prev", field.ErrorList{}),
			wantPermanent: true,
		},
		{
			name:          "contract violation",
			readErr:       contractErrorf("unexpected RpmRepo response for %s/%s", "project-a", "build-prev"),
			wantPermanent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", "incremental", []string{"gcc"}), "build-prev")
			api.storeSnapshot(activeSnapshot("project-a", "build-a"))
			api.hooks.getRpmRepo = func(project, name string) (*ebsv1.RpmRepo, error) {
				if name == "build-prev" {
					return nil, tc.readErr
				}
				return nil, apierrors.NewNotFound(rpmReposResource, name)
			}
			c := newTestController(t, api, newTestClock())

			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)

			result, err := c.sync(context.Background(), "project-a/build-a")
			if err == nil {
				t.Fatal("a failed historical read must not be swallowed")
			}
			if got := controller.IsPermanent(err); got != tc.wantPermanent {
				t.Fatalf("permanent = %v, want %v (err=%v)", got, tc.wantPermanent, err)
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v", result)
			}
			if got := api.CallCount("CreateRpmRepo"); got != 0 {
				t.Fatalf("create calls = %d, want 0: a failed historical read must not create a baseline-less RpmRepo", got)
			}
			if got := api.CallCount("UpdateBuildStatus"); got != 0 {
				t.Fatalf("status writes = %d, want 0", got)
			}
			if got := api.CallCount("GetRpmRepo project-a/build-prev"); got != 1 {
				t.Fatalf("historical reads = %d, want 1", got)
			}
			if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPending {
				t.Fatalf("phase = %q, want %q", stored.Status.Phase, ebsv1.BuildPending)
			}

			logged := output.String()
			if !strings.Contains(logged, "reason=BaseRepositoryReadFailed") {
				t.Fatalf("missing read-failure log: %q", logged)
			}
			wantRetryable := fmt.Sprintf("retryable=%t", !tc.wantPermanent)
			if !strings.Contains(logged, wantRetryable) {
				t.Fatalf("log misses %q: %q", wantRetryable, logged)
			}
			if strings.Contains(logged, "BaseRepositoryUnavailable") {
				t.Fatalf("a read failure must not degrade the baseline: %q", logged)
			}
		})
	}
}
