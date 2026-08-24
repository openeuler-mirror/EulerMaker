package job

import (
	"testing"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/generic"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
)

func TestMatchJobByRunner(t *testing.T) {
	predicate := matchJob(labels.Everything(), fields.OneTermEqualSelector("status.runner", "runner-a"))
	matched, err := predicate.Matches(&ebsv1.Job{Status: ebsv1.JobStatus{Runner: "runner-a"}})
	if err != nil || !matched {
		t.Fatalf("expected assigned job to match: matched=%v err=%v", matched, err)
	}
	matched, err = predicate.Matches(&ebsv1.Job{Status: ebsv1.JobStatus{Runner: "runner-b"}})
	if err != nil || matched {
		t.Fatalf("expected other runner job not to match: matched=%v err=%v", matched, err)
	}
}

func TestJobStoreOptionsUseJobAttrsWithoutMutatingSharedOptions(t *testing.T) {
	originalAttrs := func(runtime.Object) (labels.Set, fields.Set, error) {
		return nil, fields.Set{"source": "shared"}, nil
	}
	shared := &generic.StoreOptions{AttrFunc: originalAttrs}

	jobOptions := jobStoreOptions(shared)

	job := &ebsv1.Job{Status: ebsv1.JobStatus{Runner: "runner-a", Phase: "Running"}}
	_, jobFields, err := jobOptions.AttrFunc(job)
	if err != nil {
		t.Fatalf("get job attrs: %v", err)
	}
	if got := jobFields["status.runner"]; got != "runner-a" {
		t.Fatalf("status.runner = %q, want %q", got, "runner-a")
	}
	if got := jobFields["status.phase"]; got != "Running" {
		t.Fatalf("status.phase = %q, want %q", got, "Running")
	}

	_, sharedFields, err := shared.AttrFunc(job)
	if err != nil {
		t.Fatalf("get shared attrs: %v", err)
	}
	if got := sharedFields["source"]; got != "shared" {
		t.Fatalf("shared AttrFunc was mutated: source = %q", got)
	}
}
