package rpmrepo

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/controllers/specname"
	ebsv1 "ebs-api/ebs/v1"
)

const (
	testProject = "project"
	testBuild   = "build-a"
	testArch    = "aarch64"
)

// syncReleaseForTest exercises the release decision independently of the
// repository-entry path. Production reaches it through the same RpmRepo key.
func syncReleaseForTest(c *Controller, ctx context.Context, project, arch string) (controller.ReconcileResult, error) {
	now := c.clock.Now().UTC()
	r := &reconciler{controller: c, ctx: ctx, key: rpmRepoKey(project, testBuild), project: project,
		now: metav1.NewTime(now), nowTime: now, sameRoundAttempts: 1}
	result, err := r.reconcileRelease(arch)
	return c.plan(result, err)
}

func testConfig() Config {
	return Config{
		ArtifactManagerTimeout: 5 * time.Second,
		MaxJobsPerBatch:        20,
		MaterializeRetryLimit:  3,
		PollPeriod:             30 * time.Second,
		MaxRetries:             3,
		Backoff:                Backoff{Initial: 30 * time.Second, Max: 15 * time.Minute, Jitter: 0},
	}
}

func newTestController(t *testing.T, client Client, artifacts ArtifactManagerClient, config Config) *Controller {
	t.Helper()
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	c, err := New(&stubSource{}, client, artifacts, DefaultPublishPolicy{}, clocktesting.NewFakeClock(now), config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func newRpmRepo(name string) *ebsv1.RpmRepo {
	return &ebsv1.RpmRepo{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testProject,
			UID:             types.UID("uid-" + name),
			ResourceVersion: "1",
			Generation:      1,
			Labels: map[string]string{
				ebsv1.BuildTargetArchLabel: testArch,
			},
		},
		Status: ebsv1.RpmRepoStatus{Repository: &ebsv1.RpmRepoRepositoryStatus{}},
	}
}

func newBuild(name string) *ebsv1.Build {
	return &ebsv1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testProject, UID: types.UID("uid-" + name), ResourceVersion: "1"},
		Spec: ebsv1.BuildSpec{
			BuildType: "incremental",
			Arch:      testArch,
		},
		Status: ebsv1.BuildStatus{Phase: ebsv1.BuildProcessing},
	}
}

func newBuildInfo(name string, phase ebsv1.BuildInfoPhase) *ebsv1.BuildInfo {
	return &ebsv1.BuildInfo{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testProject, UID: types.UID("uid-" + name), ResourceVersion: "1"},
		Status:     ebsv1.BuildInfoStatus{Phase: phase},
	}
}

func newSucceededJob(name, spec, uid string, created time.Time) ebsv1.Job {
	return ebsv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testProject,
			UID:               types.UID(uid),
			ResourceVersion:   "1",
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				ebsv1.JobBuildNameLabel:    testBuild,
				ebsv1.JobSpecNameLabel:     specname.Encode(spec),
				ebsv1.BuildTargetArchLabel: testArch,
			},
		},
		Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded},
	}
}
