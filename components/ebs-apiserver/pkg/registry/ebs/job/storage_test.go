package job

import (
	"testing"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"

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
