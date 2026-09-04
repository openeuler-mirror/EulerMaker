package plugin

import (
	"context"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"scheduler/pkg/framework"
)

func TestFiltersAndLeastAllocated(t *testing.T) {
	requests, _ := framework.ParseRequests(ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "1", "memory": "1Gi"}})
	alloc, _ := framework.ParseAllocatable(map[string]string{"cpu": "4", "memory": "4Gi"})
	runner := &framework.RunnerSnapshot{Runner: &ebsv1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "r", Labels: map[string]string{"arch": "x86_64"}}, Spec: ebsv1.RunnerSpec{Type: "ct"}, Status: ebsv1.RunnerStatus{Phase: "Idle"}}, Allocatable: alloc, Available: alloc}
	session := &framework.Session{Job: &ebsv1.Job{Spec: ebsv1.JobSpec{Runtime: "ct", NodeSelector: map[string]string{"arch": "x86_64"}}}, Requests: requests}
	for _, f := range DefaultFilters() {
		if status := f.Filter(context.Background(), session, runner); status.Code != framework.Success {
			t.Fatalf("%s: %+v", f.Name(), status)
		}
	}
	score, status := LeastAllocated().Score(context.Background(), session, runner)
	if status.Code != framework.Success || score != 75 {
		t.Fatalf("score=%d status=%+v", score, status)
	}
}
