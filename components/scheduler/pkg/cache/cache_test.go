package cache

import (
	"sync"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"scheduler/pkg/framework"
)

func testJob(name string, uid types.UID) *ebsv1.Job {
	return &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: name, UID: uid, ResourceVersion: "1"}, Spec: ebsv1.JobSpec{Resources: ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "1", "memory": "1Gi"}}}, Status: ebsv1.JobStatus{Phase: "Pending"}}
}
func testRunner() *ebsv1.Runner {
	return &ebsv1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "r", UID: "runner"}, Spec: ebsv1.RunnerSpec{Type: "ct"}, Status: ebsv1.RunnerStatus{Phase: "Idle", Allocatable: map[string]string{"cpu": "1", "memory": "1Gi"}}}
}
func TestAssumeIsAtomicAndGenerationProtectsForget(t *testing.T) {
	c := New(time.Minute)
	j1, j2 := testJob("a", "a"), testJob("b", "b")
	c.UpsertJob(j1)
	c.UpsertJob(j2)
	c.UpsertRunner(testRunner())
	snap, _ := c.Snapshot("p/a", "a")
	req := framework.Resource{}
	req, _ = framework.ParseRequests(j1.Spec.Resources)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, j := range []*ebsv1.Job{j1, j2} {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Assume(AssumeRequest{JobKey: JobKey(j), JobUID: j.UID, RunnerName: "r", RunnerUID: "runner", RunnerRevision: snap.Runners["r"].Revision, Requests: req, JobResourceVersion: "1"})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("got %d successful assumptions, want 1", success)
	}
}
func TestResyncDoesNotDoubleCountRunningJob(t *testing.T) {
	c := New()
	r := testRunner()
	r.Status.Allocatable["cpu"] = "2"
	c.UpsertRunner(r)
	j := testJob("a", "a")
	j.Status.Phase = "Running"
	j.Status.Runner = "r"
	c.UpsertJob(j)
	c.UpsertJob(j.DeepCopy())
	pending := testJob("b", "b")
	c.UpsertJob(pending)
	snap, err := c.Snapshot("p/b", "b")
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Runners["r"].RunningJobCount; got != 1 {
		t.Fatalf("running count=%d", got)
	}
}
