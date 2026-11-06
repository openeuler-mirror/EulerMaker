package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

const seededRepositoryUID = "DB3A8CE2-00CD-4C89-9C20-ADA417C83155"

func TestChildCreationConfirmation(t *testing.T) {
	for _, kind := range []string{"Snapshot", "RpmRepo", "BuildInfo"} {
		for _, writeOutcome := range []clientpkg.WriteOutcome{clientpkg.WriteRejected, clientpkg.WriteUnknown} {
			for _, confirmation := range []string{"found", "missing", "deleting", "temporary error", "forbidden"} {
				t.Run(kind+"/"+string(writeOutcome)+"/"+confirmation, func(t *testing.T) {
					api := newFakeAPI()
					build := newBuild("project-a", "build-a", "full", nil)
					api.projects[build.Namespace] = newProject(build.Namespace)
					writeErr := &clientpkg.WriteError{
						Operation:  "create",
						Outcome:    writeOutcome,
						StatusCode: http.StatusConflict,
						Err:        errors.New("create response unavailable"),
					}
					if writeOutcome == clientpkg.WriteUnknown {
						writeErr.StatusCode = 0
					}
					meta := metav1.ObjectMeta{
						Name:            build.Name,
						Namespace:       build.Namespace,
						UID:             "confirmed-child",
						ResourceVersion: "confirmed-version",
					}
					var readErr error
					resource := schema.GroupResource{Group: "ebs", Resource: strings.ToLower(kind) + "s"}
					switch confirmation {
					case "missing":
						readErr = apierrors.NewNotFound(resource, build.Name)
					case "deleting":
						now := metav1.Now()
						meta.DeletionTimestamp = &now
					case "temporary error":
						readErr = errors.New("confirmation connection interrupted")
					case "forbidden":
						readErr = apierrors.NewForbidden(resource, build.Name, errors.New("denied"))
					}
					r := &reconciler{
						controller: newTestController(t, api, newTestClock()),
						ctx:        context.Background(),
						key:        key(build.Namespace, build.Name),
						project:    build.Namespace,
						current:    build,
					}
					var actual metav1.Object
					var outcome ensureOutcome
					switch kind {
					case "Snapshot":
						api.hooks.createSnapshot = func(*ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
							api.hooks.getSnapshot = func(string, string) (*ebsv1.Snapshot, error) {
								return &ebsv1.Snapshot{ObjectMeta: meta}, readErr
							}
							return nil, writeErr
						}
						obj, result := r.ensureSnapshot()
						outcome = result
						if obj != nil {
							actual = obj
						}
					case "RpmRepo":
						api.hooks.createRpmRepo = func(*ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
							api.hooks.getRpmRepo = func(string, string) (*ebsv1.RpmRepo, error) {
								return &ebsv1.RpmRepo{ObjectMeta: meta}, readErr
							}
							return nil, writeErr
						}
						obj, result := r.ensureRpmRepo()
						outcome = result
						if obj != nil {
							actual = obj
						}
					case "BuildInfo":
						api.hooks.createBuildInfo = func(*ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
							api.hooks.getBuildInfo = func(string, string) (*ebsv1.BuildInfo, error) {
								return &ebsv1.BuildInfo{ObjectMeta: meta}, readErr
							}
							return nil, writeErr
						}
						obj, result := r.ensureBuildInfo()
						outcome = result
						if obj != nil {
							actual = obj
						}
					}
					want := ensureOutcome{ready: confirmation == "found"}
					switch confirmation {
					case "missing":
						if writeOutcome == clientpkg.WriteRejected {
							want.requeueAfter = conflictRequeueDelay
						} else {
							want.err = writeErr
						}
					case "temporary error":
						want.err = readErr
					case "forbidden":
						want.err = controller.NewPermanentError(readErr)
					}
					if outcome.ready != want.ready || outcome.requeueAfter != want.requeueAfter || !errors.Is(outcome.err, want.err) {
						t.Fatalf("outcome = %+v, want %+v", outcome, want)
					}
					if controller.IsPermanent(outcome.err) != (confirmation == "forbidden") {
						t.Fatalf("unexpected error classification: %v", outcome.err)
					}
					if readErr == nil {
						if actual == nil || actual.GetResourceVersion() != meta.ResourceVersion || actual.GetUID() != meta.UID {
							t.Fatalf("confirmed object = %+v, want metadata %+v", actual, meta)
						}
					} else if actual != nil {
						t.Fatalf("failed confirmation returned an object: %+v", actual)
					}
					if api.CallCount("Get"+kind+" "+r.key) != 2 || api.CallCount("Create"+kind+" "+r.key) != 1 {
						t.Fatalf("unexpected confirmation calls: %v", api.Calls())
					}
				})
			}
		}
	}
}

func TestPreparedSelectsImageFromProjectDistroAndBuildArch(t *testing.T) {
	content := &ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{
		"distro-a": {Arches: map[string]ebsv1.BuildTargetArch{
			"aarch64": {Image: "build:a-arm"}, "x86_64": {Image: "build:a-x86"},
		}},
		"distro-b": {Arches: map[string]ebsv1.BuildTargetArch{
			"aarch64": {Image: "build:b-arm"},
		}},
	}}
	for _, tc := range []struct {
		distro, arch, image string
		buildType           ebsv1.BuildType
	}{
		{"distro-a", "aarch64", "build:a-arm", ebsv1.BuildTypeFull},
		{"distro-a", "x86_64", "build:a-x86", ebsv1.BuildTypeSingle},
		{"distro-b", "aarch64", "build:b-arm", ebsv1.BuildTypeFull},
	} {
		t.Run(tc.distro+"/"+tc.arch, func(t *testing.T) {
			api := newFakeAPI()
			build := withPhaseStage(newBuild("project-a", "build-a", tc.buildType, nil), ebsv1.BuildPrepared, "")
			build.Spec.Arch = tc.arch
			api.builds[key("project-a", "build-a")] = build
			project := newProject("project-a")
			project.Spec.Distro = tc.distro
			api.projects[project.Name] = project
			api.hooks.getBuildTargetContent = func() (*ebsv1.BuildTargetContent, error) { return content, nil }
			c := newTestController(t, api, newTestClock())
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			if got := api.buildInfo("project-a", "build-a").Spec.Image; got != tc.image {
				t.Fatalf("image = %q, want %q", got, tc.image)
			}
		})
	}
}

func TestPendingReusesExistingSnapshotWithoutReadingProject(t *testing.T) {
	build := withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = build
	api.storeSnapshot(&ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotPending},
	})
	c := newTestController(t, api, newTestClock())
	beforeTerminating := counterValue(t, metricEnsureTerminating)
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("GetProject") != 0 || api.CallCount("CreateSnapshot") != 0 {
		t.Fatal("an existing Snapshot must not be filtered or recreated")
	}
	if api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("Build must keep waiting")
	}
	assertCounterDelta(t, metricEnsureTerminating, beforeTerminating, 0)
}

func TestPendingCreatesAndReusesSnapshotWithGitRef(t *testing.T) {
	for _, outcome := range []clientpkg.WriteOutcome{"", clientpkg.WriteRejected, clientpkg.WriteUnknown} {
		t.Run(string(outcome), func(t *testing.T) {
			api := newFakeAPI()
			build := newBuild("project-a", "build-a", "single", []string{"gcc"})
			build.Annotations = map[string]string{ebsv1.BuildGitRefAnnotation: "refs/merge-requests/501/head"}
			api.builds[key(build.Namespace, build.Name)] = build
			api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"), newPackageRepo("glibc"))
			if outcome != "" {
				api.hooks.createSnapshot = func(request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
					api.storeSnapshot(request)
					if outcome == clientpkg.WriteRejected {
						return nil, &clientpkg.WriteError{
							Operation: "create", Outcome: outcome, StatusCode: 409,
							Err: apierrors.NewAlreadyExists(snapshotsResource, request.Name),
						}
					}
					return nil, &clientpkg.WriteError{Operation: "create", Outcome: outcome, Err: errors.New("lost response")}
				}
			}
			c := newTestController(t, api, newTestClock())
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatalf("sync: %v", err)
			}
			snapshot := api.snapshot("project-a", "build-a")
			if snapshot == nil {
				t.Fatal("Snapshot was not created")
			}
			project := api.getProject("project-a")
			wantRepo := project.Spec.PackageRepos[0]
			wantRepo.Ref = build.Annotations[ebsv1.BuildGitRefAnnotation]
			wantRepo.CommitID = ""
			if !reflect.DeepEqual(snapshot.Spec.PackageRepos, []ebsv1.PackageRepo{wantRepo}) ||
				snapshot.Spec.DefaultRef != project.Spec.DefaultRef {
				t.Fatalf("Snapshot input: %+v", snapshot.Spec)
			}
			api.projects["project-a"] = newProject("project-a", newPackageRepo("changed"))
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatalf("repeated sync: %v", err)
			}
			if got := api.snapshot("project-a", "build-a"); !reflect.DeepEqual(got, snapshot) {
				t.Fatalf("reused Snapshot = %+v, want %+v", got, snapshot)
			}
			if api.CallCount("CreateSnapshot") != 1 || api.CallCount("GetProject") != 1 {
				t.Fatalf("create calls = %d, Project reads = %d", api.CallCount("CreateSnapshot"), api.CallCount("GetProject"))
			}
		})
	}
}

func TestPendingWaitsForTerminatingSnapshot(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	deletion := metav1.NewTime(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC))
	api.storeSnapshot(&ebsv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", DeletionTimestamp: &deletion},
		Status:     ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotActive},
	})
	beforeTerminating := counterValue(t, metricEnsureTerminating)
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("a terminating Snapshot must not advance the Build")
	}
	assertCounterDelta(t, metricEnsureTerminating, beforeTerminating, 1)
}

func TestPendingProjectReadFailures(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		c := newTestController(t, api, newTestClock())
		_, err := c.sync(context.Background(), "project-a/build-a")
		if !controller.IsPermanent(err) {
			t.Fatalf("want permanent error, got %v", err)
		}
		if api.CallCount("CreateSnapshot") != 0 || api.CallCount("UpdateBuildStatus") != 0 {
			t.Fatal("a missing Project must not create resources or write status")
		}
	})

	t.Run("temporary", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		temporary := errors.New("connection reset")
		api.hooks.getProject = func(string) (*ebsv1.Project, error) { return nil, temporary }
		c := newTestController(t, api, newTestClock())
		result, err := c.sync(context.Background(), "project-a/build-a")
		if !errors.Is(err, temporary) || controller.IsPermanent(err) {
			t.Fatalf("want transient error, got %v", err)
		}
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if api.CallCount("CreateSnapshot") != 0 || api.CallCount("UpdateBuildStatus") != 0 {
			t.Fatal("a transient read failure must not create resources or write status")
		}
	})
}

func TestChildCreationConflictPreservesPhaseFlow(t *testing.T) {
	for _, tc := range []struct {
		kind             string
		wantPhase        ebsv1.BuildPhase
		wantStage        ebsv1.BuildStage
		wantProjectReads int
		wantStatusWrites int
	}{
		{kind: "Snapshot", wantPhase: ebsv1.BuildPending, wantProjectReads: 1},
		{kind: "RpmRepo", wantPhase: ebsv1.BuildPrepared, wantStatusWrites: 1},
		{
			kind: "BuildInfo", wantPhase: ebsv1.BuildProcessing, wantStage: ebsv1.BuildStageBuild,
			wantProjectReads: 1, wantStatusWrites: 1,
		},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			api := newFakeAPI()
			build := withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
			api.builds[key(build.Namespace, build.Name)] = build
			api.projects[build.Namespace] = newProject(build.Namespace, newPackageRepo("gcc"))
			resource := schema.GroupResource{Group: "ebs", Resource: strings.ToLower(tc.kind) + "s"}
			writeErr := &clientpkg.WriteError{
				Operation:  "create",
				Outcome:    clientpkg.WriteRejected,
				StatusCode: http.StatusConflict,
				Err:        apierrors.NewAlreadyExists(resource, build.Name),
			}
			switch tc.kind {
			case "Snapshot":
				api.hooks.createSnapshot = func(request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
					api.storeSnapshot(request)
					return nil, writeErr
				}
			case "RpmRepo":
				api.storeSnapshot(activeSnapshot(build.Namespace, build.Name))
				api.hooks.createRpmRepo = func(request *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
					api.storeRpmRepo(request)
					return nil, writeErr
				}
			case "BuildInfo":
				build.Status.Phase = ebsv1.BuildPrepared
				api.hooks.createBuildInfo = func(request *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
					api.storeBuildInfo(request)
					return nil, writeErr
				}
			}
			c := newTestController(t, api, newTestClock())
			beforeConflicts := counterValue(t, metricEnsureConflicts)
			result, err := c.sync(context.Background(), key(build.Namespace, build.Name))
			if err != nil || result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if api.CallCount("Create"+tc.kind) != 1 || api.CallCount("Get"+tc.kind) != 2 {
				t.Fatalf("unexpected creation calls: %v", api.Calls())
			}
			if api.CallCount("GetProject") != tc.wantProjectReads || api.CallCount("UpdateBuildStatus") != tc.wantStatusWrites {
				t.Fatalf("unexpected phase calls: %v", api.Calls())
			}
			stored := api.build(build.Namespace, build.Name)
			if stored.Status.Phase != tc.wantPhase || stored.Status.Stage != tc.wantStage {
				t.Fatalf("phase/stage = %q/%q, want %q/%q", stored.Status.Phase, stored.Status.Stage, tc.wantPhase, tc.wantStage)
			}
			if tc.kind == "Snapshot" {
				snapshot := api.snapshot(build.Namespace, build.Name)
				project := api.getProject(build.Namespace)
				if snapshot == nil || snapshot.Spec.DefaultRef != project.Spec.DefaultRef ||
					!reflect.DeepEqual(snapshot.Spec.PackageRepos, project.Spec.PackageRepos) {
					t.Fatalf("reused Snapshot does not match the Project input: %+v", snapshot)
				}
			}
			assertCounterDelta(t, metricEnsureConflicts, beforeConflicts, 1)
		})
	}
}

func TestPendingCreateSnapshotUnknownConfirmsExistence(t *testing.T) {
	t.Run("object exists", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
		api.hooks.createSnapshot = func(request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
			api.storeSnapshot(request)
			return nil, &clientpkg.WriteError{
				Operation: "create",
				Outcome:   clientpkg.WriteUnknown,
				Err:       errors.New("lost response"),
			}
		}
		c := newTestController(t, api, newTestClock())
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err != nil || result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if api.CallCount("CreateSnapshot") != 1 {
			t.Fatalf("create calls = %d", api.CallCount("CreateSnapshot"))
		}
		if got := api.CallCount("GetProject project-a"); got != 1 {
			t.Fatalf("project reads = %d, want 1", got)
		}
		confirmed := api.snapshot("project-a", "build-a")
		if confirmed == nil {
			t.Fatal("the confirmed Snapshot is missing")
		}
		project := api.getProject("project-a")
		if confirmed.Spec.DefaultRef != project.Spec.DefaultRef || !reflect.DeepEqual(repoNames(confirmed.Spec.PackageRepos), []string{"gcc"}) {
			t.Fatalf("the confirmed Snapshot must be built from the single Project read: %+v", confirmed.Spec)
		}
	})

	t.Run("object missing", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
		var requested *ebsv1.Snapshot
		api.hooks.createSnapshot = func(request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
			requested = request
			return nil, &clientpkg.WriteError{
				Operation: "create",
				Outcome:   clientpkg.WriteUnknown,
				Err:       errors.New("lost response"),
			}
		}
		c := newTestController(t, api, newTestClock())
		result, err := c.sync(context.Background(), "project-a/build-a")
		if err == nil || controller.IsPermanent(err) {
			t.Fatalf("want retryable error, got %v", err)
		}
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if api.CallCount("CreateSnapshot") != 1 {
			t.Fatal("an unconfirmed create must not be repeated inside one round")
		}
		if got := api.CallCount("GetProject project-a"); got != 1 {
			t.Fatalf("project reads = %d, want 1", got)
		}
		if requested == nil {
			t.Fatal("the create request is missing")
		}
		project := api.getProject("project-a")
		if requested.Spec.DefaultRef != project.Spec.DefaultRef || !reflect.DeepEqual(repoNames(requested.Spec.PackageRepos), []string{"gcc"}) {
			t.Fatalf("the create request must be built from the single Project read: %+v", requested.Spec)
		}
		if api.CallCount("UpdateBuildStatus") != 0 {
			t.Fatal("an unconfirmed create must not advance the Build")
		}
	})
}

// Representative rejections must preserve classification without confirming creation or advancing the Build.
func TestPendingCreateSnapshotRejections(t *testing.T) {
	for _, tc := range []struct {
		name          string
		statusCode    int
		wantPermanent bool
	}{
		{name: "too many requests", statusCode: 429},
		{name: "not found", statusCode: 404},
		{name: "forbidden", statusCode: 403, wantPermanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
			api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
			beforeConflicts := counterValue(t, metricEnsureConflicts)
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			api.hooks.createSnapshot = func(*ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
				return nil, &clientpkg.WriteError{
					Operation: "create", Outcome: clientpkg.WriteRejected,
					StatusCode: tc.statusCode, Err: errors.New("create rejected"),
				}
			}
			c := newTestController(t, api, newTestClock())
			result, err := c.sync(context.Background(), "project-a/build-a")
			if got := controller.IsPermanent(err); got != tc.wantPermanent {
				t.Fatalf("permanent = %v, want %v (err=%v)", got, tc.wantPermanent, err)
			}
			if !tc.wantPermanent {
				var writeErr *clientpkg.WriteError
				if !errors.As(err, &writeErr) || writeErr.StatusCode != tc.statusCode {
					t.Fatalf("a retryable rejection must be returned unchanged: %v", err)
				}
			}
			if result != (controller.ReconcileResult{}) {
				t.Fatalf("result=%+v", result)
			}
			if api.CallCount("GetSnapshot") != 1 {
				t.Fatalf("a rejected create must not run a confirmation read: %d", api.CallCount("GetSnapshot"))
			}
			if api.CallCount("UpdateBuildStatus") != 0 {
				t.Fatal("a rejected create must not write Build.status")
			}
			if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPending {
				t.Fatalf("phase = %q", stored.Status.Phase)
			}
			if api.CallCount("CreateRpmRepo") != 0 {
				t.Fatal("a rejected create must not advance the Build")
			}
			logged := output.String()
			for _, want := range []string{
				"reason=CreateRejected",
				"kind=Snapshot",
				"operation=create",
				fmt.Sprintf("status=%d", tc.statusCode),
			} {
				if !strings.Contains(logged, want) {
					t.Fatalf("create rejection log misses %q: %q", want, logged)
				}
			}
			wantRetryable := fmt.Sprintf("retryable=%t", !tc.wantPermanent)
			if !strings.Contains(logged, wantRetryable) {
				t.Fatalf("create rejection log misses %q: %q", wantRetryable, logged)
			}
			assertCounterDelta(t, metricEnsureConflicts, beforeConflicts, 0)
		})
	}
}

func TestPendingWaitsForTerminatingRpmRepo(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", "full", []string{"gcc"}), "build-prev")
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	deletion := metav1.NewTime(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC))
	api.storeRpmRepo(&ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", DeletionTimestamp: &deletion},
	})
	c := newTestController(t, api, newTestClock())
	result, err := c.sync(context.Background(), "project-a/build-a")
	if err != nil || result != (controller.ReconcileResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.CallCount("CreateRpmRepo") != 0 || api.CallCount("UpdateBuildStatus") != 0 {
		t.Fatal("a terminating RpmRepo must keep the Build waiting")
	}
	if api.CallCount("GetRpmRepo project-a/build-prev") != 0 {
		t.Fatal("an existing RpmRepo must not trigger the historical base read")
	}
}

func TestNewSnapshotCopiesProjectInputWithoutSharingState(t *testing.T) {
	project := newProject("project-a", newPackageRepo("gcc"), newPackageRepo("glibc"))
	build := withBaseBuildRef(newBuild("project-a", "build-a", "single", []string{"glibc", "gcc", "glibc"}))
	snapshot := newSnapshot(build, project)
	if !reflect.DeepEqual(repoNames(snapshot.Spec.PackageRepos), []string{"gcc", "glibc"}) {
		t.Fatalf("packageRepos = %v", repoNames(snapshot.Spec.PackageRepos))
	}
	if snapshot.Spec.DefaultRef != project.Spec.DefaultRef {
		t.Fatalf("defaultRef = %+v", snapshot.Spec.DefaultRef)
	}
	snapshot.Spec.PackageRepos[0].Name = "mutated"
	snapshot.Spec.PackageRepos[0].Ref = "mutated"
	snapshot.Spec.DefaultRef = "mutated"
	if project.Spec.PackageRepos[0].Name != "gcc" || project.Spec.PackageRepos[0].Ref != "refs/heads/master" || project.Spec.DefaultRef != "refs/heads/master" {
		t.Fatal("Snapshot construction must not share state with the Project")
	}
}

func TestNewSnapshotKeepsFullRepositoryListForNonSingleBuilds(t *testing.T) {
	project := newProject("project-a", newPackageRepo("gcc"), newPackageRepo("glibc"))
	build := withBaseBuildRef(newBuild("project-a", "build-a", "incremental", []string{"gcc"}))
	snapshot := newSnapshot(build, project)
	if !reflect.DeepEqual(repoNames(snapshot.Spec.PackageRepos), []string{"gcc", "glibc"}) {
		t.Fatalf("packageRepos = %v", repoNames(snapshot.Spec.PackageRepos))
	}
	snapshot.Spec.PackageRepos[0].Ref = "refs/heads/other"
	if project.Spec.PackageRepos[0].Ref != "refs/heads/master" {
		t.Fatal("Snapshot repositories must be independent of Project repositories")
	}
	for _, repos := range [][]ebsv1.PackageRepo{nil, {}} {
		project.Spec.PackageRepos = repos
		if got := newSnapshot(build, project).Spec.PackageRepos; !reflect.DeepEqual(got, []ebsv1.PackageRepo{}) {
			t.Fatalf("empty packageRepos = %#v", got)
		}
	}
}

func TestNewSnapshotFreezesGitRefInPackageRepo(t *testing.T) {
	project := newProject("project-a", newPackageRepo("glibc"), newPackageRepo("gcc"))
	const commitID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	project.Spec.PackageRepos[1].CommitID = commitID
	project.Spec.PackageRepos[1].Ref = "refs/heads/main"
	originalProject := project.DeepCopy()
	build := newBuild("project-a", "build-a", "single", []string{"gcc"})
	if got := newSnapshot(build, project).Spec.PackageRepos[0].CommitID; got != commitID {
		t.Fatalf("copied commitID=%q", got)
	}
	ref := "refs/merge-requests/501/head"
	build.Annotations = map[string]string{ebsv1.BuildGitRefAnnotation: ref, "example.com/note": "build only"}
	snapshot := newSnapshot(build, project)
	wantRepo := project.Spec.PackageRepos[1]
	wantRepo.Ref, wantRepo.CommitID = ref, ""
	if !reflect.DeepEqual(snapshot.Spec.PackageRepos, []ebsv1.PackageRepo{wantRepo}) {
		t.Fatalf("packageRepos = %+v, want %+v", snapshot.Spec.PackageRepos, wantRepo)
	}
	if !reflect.DeepEqual(project, originalProject) {
		t.Fatal("Snapshot ref override changed the Project")
	}
	if len(snapshot.Annotations) != 0 {
		t.Fatalf("Snapshot annotations = %v", snapshot.Annotations)
	}
	snapshot.Spec.PackageRepos[0].Ref = "refs/merge-requests/502/head"
	if build.Annotations[ebsv1.BuildGitRefAnnotation] != ref {
		t.Fatal("Snapshot ref must be independent of the Build annotation")
	}
}

func TestNewBuildInfoCopiesBootstrapRepos(t *testing.T) {
	project := newProject("project-a", newPackageRepo("gcc"))
	build := withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
	info := newBuildInfo(build, project, "build:test")
	if !reflect.DeepEqual(info.Spec.BootstrapRepos, project.Spec.BootstrapRepos) {
		t.Fatalf("bootstrapRepos = %+v", info.Spec.BootstrapRepos)
	}
	info.Spec.BootstrapRepos[0].URL = "mutated"
	if project.Spec.BootstrapRepos[0].URL != "https://example.com/repo/everything" {
		t.Fatal("BuildInfo construction must not share state with the Project")
	}
	for _, repos := range [][]ebsv1.BootstrapRepo{nil, {}} {
		project.Spec.BootstrapRepos = repos
		if got := newBuildInfo(build, project, "build:test").Spec.BootstrapRepos; got != nil {
			t.Fatalf("empty bootstrapRepos = %#v", got)
		}
	}
}

func TestNewBuildInfoFreezesBuildPayload(t *testing.T) {
	for _, payload := range []string{"", "macros:\n  dist: .oe2403\n"} {
		t.Run(payload, func(t *testing.T) {
			project := newProject("project-a", newPackageRepo("gcc"))
			project.Spec.BuildPayload = payload
			info := newBuildInfo(newBuild("project-a", "build-a", "full", nil), project, "build:test")
			project.Spec.BuildPayload = "changed"
			if info.Spec.BuildPayload != payload {
				t.Fatalf("buildPayload = %q, want %q", info.Spec.BuildPayload, payload)
			}
		})
	}
}

func TestPreparedReusesExistingBuildInfo(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, "")
	api.storeBuildInfo(&ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"},
		Spec: ebsv1.BuildInfoSpec{
			Image:          "frozen:image",
			BuildPayload:   "frozen payload",
			BootstrapRepos: []ebsv1.BootstrapRepo{{Name: "existing", URL: "https://example.com/existing"}},
		},
	})
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("CreateSnapshot") != 0 || api.CallCount("GetProject") != 0 || api.CallCount("GetBuildTargetContent") != 0 || api.CallCount("CreateBuildInfo") != 0 {
		t.Fatal("an existing BuildInfo must not be recreated or re-read from the Project")
	}
	if info := api.buildInfo("project-a", "build-a"); info.Spec.BootstrapRepos[0].Name != "existing" || info.Spec.BuildPayload != "frozen payload" || info.Spec.Image != "frozen:image" {
		t.Fatalf("BuildInfo spec was overwritten: %+v", info.Spec)
	}
	if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildProcessing || stored.Status.Stage != ebsv1.BuildStageBuild {
		t.Fatalf("phase/stage = %q/%q", stored.Status.Phase, stored.Status.Stage)
	}
}

func TestPreparedImageConfigurationFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readErr   error
		permanent bool
	}{
		{name: "temporary read failure", readErr: fmt.Errorf("connection interrupted")},
		{name: "mapping removed", permanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withPhaseStage(
				newBuild("project-a", "build-a", "full", nil), ebsv1.BuildPrepared, "")
			api.projects["project-a"] = newProject("project-a")
			api.hooks.getBuildTargetContent = func() (*ebsv1.BuildTargetContent, error) {
				return &ebsv1.BuildTargetContent{}, tc.readErr
			}
			c := newTestController(t, api, newTestClock())
			_, err := c.sync(context.Background(), "project-a/build-a")
			if err == nil || controller.IsPermanent(err) != tc.permanent {
				t.Fatalf("sync error = %v, want permanent=%v", err, tc.permanent)
			}
			if api.buildInfo("project-a", "build-a") != nil || api.build("project-a", "build-a").Status.Phase != ebsv1.BuildPrepared {
				t.Fatal("configuration failure must leave Build Prepared without a BuildInfo")
			}
			api.hooks.getBuildTargetContent = nil
			if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
				t.Fatal(err)
			}
			if got := api.buildInfo("project-a", "build-a").Spec.Image; got != "build:test" {
				t.Fatalf("recovered image = %q", got)
			}
		})
	}
}

// TestCreateRejectionLogsTheChildKind checks the structured log every non conflict create rejection emits, for
// each child resource the controller creates on its own paths.
func TestCreateRejectionLogsTheChildKind(t *testing.T) {
	t.Run("rpm repo with a missing route", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "full", []string{"gcc"}))
		api.storeSnapshot(activeSnapshot("project-a", "build-a"))
		api.hooks.createRpmRepo = func(*ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
			return nil, &clientpkg.WriteError{
				Operation: "create", Outcome: clientpkg.WriteRejected,
				StatusCode: http.StatusNotFound, Err: errors.New("the server could not find the requested resource"),
			}
		}
		c := newTestController(t, api, newTestClock())

		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)

		result, err := c.sync(context.Background(), "project-a/build-a")
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if err == nil || controller.IsPermanent(err) {
			t.Fatalf("a create 404 must be retryable, got %v", err)
		}
		logged := output.String()
		for _, want := range []string{"reason=CreateRejected", "kind=RpmRepo", "operation=create", "status=404", "retryable=true"} {
			if !strings.Contains(logged, want) {
				t.Fatalf("rejection log misses %q: %q", want, logged)
			}
		}
		if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPending {
			t.Fatalf("phase = %q, want %q", stored.Status.Phase, ebsv1.BuildPending)
		}
	})

	t.Run("build info with a permanent rejection", func(t *testing.T) {
		api := newFakeAPI()
		api.builds[key("project-a", "build-a")] = withPhaseStage(newBuild("project-a", "build-a", "full", []string{"gcc"}), ebsv1.BuildPrepared, "")
		api.projects["project-a"] = newProject("project-a", newPackageRepo("gcc"))
		api.hooks.createBuildInfo = func(*ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
			return nil, &clientpkg.WriteError{
				Operation: "create", Outcome: clientpkg.WriteRejected,
				StatusCode: http.StatusForbidden, Err: errors.New("denied"),
			}
		}
		c := newTestController(t, api, newTestClock())

		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)

		result, err := c.sync(context.Background(), "project-a/build-a")
		if result != (controller.ReconcileResult{}) {
			t.Fatalf("result=%+v", result)
		}
		if !controller.IsPermanent(err) {
			t.Fatalf("a 403 create rejection must be permanent, got %v", err)
		}
		logged := output.String()
		for _, want := range []string{"reason=CreateRejected", "kind=BuildInfo", "status=403", "retryable=false"} {
			if !strings.Contains(logged, want) {
				t.Fatalf("rejection log misses %q: %q", want, logged)
			}
		}
		if stored := api.build("project-a", "build-a"); stored.Status.Phase != ebsv1.BuildPrepared {
			t.Fatalf("phase = %q, want %q", stored.Status.Phase, ebsv1.BuildPrepared)
		}
	})
}

func seedIncrementalHistory(api *fakeAPI) {
	api.storeSnapshot(snapshotWithCommits("build-a", map[string]string{"gcc": "commit"}))
	api.storeSnapshot(snapshotWithCommits("build-prev", map[string]string{"gcc": "commit"}))
	api.storeBuildInfo(&ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Name: "build-prev", Namespace: "project-a"},
		Status: ebsv1.BuildInfoStatus{
			Phase: ebsv1.BuildInfoCompleted, SpecResults: ebsv1.SpecResults{"gcc/" + "gcc": {Status: ebsv1.SpecPrepareFailed}},
		},
	})
}

func historicalRpmRepo(project, name, repositoryUID, contentURL string) *ebsv1.RpmRepo {
	return &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project},
		Status: ebsv1.RpmRepoStatus{Repository: &ebsv1.RpmRepoRepositoryStatus{
			RepositoryUID: repositoryUID, ContentURL: contentURL,
		}},
	}
}

func repositoryContentURL(repositoryUID string) string {
	return "https://artifact-manager/repositories/project/builds/build/" + repositoryUID
}

// assertRpmRepoTargetLabels pins the target labels stamped at creation: they come from the Build target and
// must be the only labels carried into the RpmRepo.
func assertRpmRepoTargetLabels(t *testing.T, build *ebsv1.Build, repo *ebsv1.RpmRepo) {
	t.Helper()
	want := map[string]string{
		ebsv1.BuildTargetArchLabel: build.Spec.Arch,
	}
	if !reflect.DeepEqual(repo.Labels, want) {
		t.Fatalf("labels = %+v, want %+v", repo.Labels, want)
	}
}

func TestPendingSeedsRpmRepoBaseFromHistory(t *testing.T) {
	for _, buildType := range []ebsv1.BuildType{"full", "incremental", "specified"} {
		t.Run(string(buildType), func(t *testing.T) {
			api := newFakeAPI()
			api.builds[key("project-a", "build-a")] = withBaseBuildRefName(newBuild("project-a", "build-a", buildType, []string{"gcc"}), "build-prev")
			api.storeSnapshot(activeSnapshot("project-a", "build-a"))
			if buildType == "incremental" {
				seedIncrementalHistory(api)
			}
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
			if created.Status.Repository == nil {
				t.Fatal("repository status is missing")
			}
			if created.Status.Repository.RepositoryUID != seededRepositoryUID || created.Status.Repository.ContentURL != repositoryContentURL(seededRepositoryUID) {
				t.Fatalf("seeded base = %+v", created.Status.Repository)
			}
			if created.Status.Release != nil || len(created.Status.Conditions) != 0 {
				t.Fatalf("only the seeded base must survive creation: %+v", created.Status)
			}
			assertRpmRepoTargetLabels(t, api.build("project-a", "build-a"), created)
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
	if created == nil {
		t.Fatal("RpmRepo is missing")
	}
	if created.Status.Repository != nil {
		t.Fatalf("unexpected repository: %+v", created.Status.Repository)
	}
	assertRpmRepoTargetLabels(t, api.build("project-a", "build-a"), created)
}

func TestPendingDoesNotBackfillLabelsOnExistingRpmRepo(t *testing.T) {
	api := newFakeAPI()
	api.builds[key("project-a", "build-a")] = withBaseBuildRef(newBuild("project-a", "build-a", "incremental", []string{"gcc"}))
	api.storeSnapshot(activeSnapshot("project-a", "build-a"))
	api.storeRpmRepo(&ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a"}})
	c := newTestController(t, api, newTestClock())
	if _, err := c.sync(context.Background(), "project-a/build-a"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if api.CallCount("CreateRpmRepo") != 0 {
		t.Fatal("an existing RpmRepo must be reused, not created again")
	}
	if repo := api.rpmRepo("project-a", "build-a"); len(repo.Labels) != 0 {
		t.Fatalf("labels must not be backfilled on existing objects: %+v", repo.Labels)
	}
}

func TestRpmRepoKeepsBaseline(t *testing.T) {
	for _, base := range []repositoryBase{{}, {
		repositoryUID: "version",
		contentURL:    "https://artifact/repositories/project/builds/build/version/",
	}} {
		build := newBuild("project-a", "build-a", "full", []string{"gcc"})
		repo := newRpmRepo(build, base)
		if base.repositoryUID == "" {
			if repo.Status.Repository != nil {
				t.Fatal("empty baseline must stay nil")
			}
		} else if repo.Status.Repository == nil || repo.Status.Repository.RepositoryUID != base.repositoryUID || repo.Status.Repository.ContentURL != base.contentURL {
			t.Fatalf("baseline lost: %+v", repo.Status.Repository)
		}
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
			seedIncrementalHistory(api)
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
			if created == nil {
				t.Fatal("RpmRepo is missing")
			}
			if created.Status.Repository != nil {
				t.Fatalf("unexpected repository: %+v", created.Status.Repository)
			}
			assertRpmRepoTargetLabels(t, api.build("project-a", "build-a"), created)
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
	seedIncrementalHistory(api)

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
	seedIncrementalHistory(api)
	api.storeRpmRepo(historicalRpmRepo("project-a", "build-prev", seededRepositoryUID, repositoryContentURL(seededRepositoryUID)))
	api.hooks.updateStatus = func(*ebsv1.Build) (*ebsv1.Build, error) {
		return nil, &clientpkg.WriteError{
			Operation:  "update-status",
			Outcome:    clientpkg.WriteRejected,
			StatusCode: 409,
			Err:        errors.New("conflict"),
		}
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
	assertRpmRepoTargetLabels(t, api.build("project-a", "build-a"), stored)
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
			seedIncrementalHistory(api)
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
