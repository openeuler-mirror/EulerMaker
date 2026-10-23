package job

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"

	ebsv1 "ebs-api/ebs/v1"
)

func TestMatchJobByRunner(t *testing.T) {
	store := NewStorage().Resource.(*genericregistry.Store)
	predicate := store.PredicateFunc(labels.Everything(), fields.OneTermEqualSelector("status.runner", "runner-a"))
	matched, err := predicate.Matches(&ebsv1.Job{Status: ebsv1.JobStatus{Runner: "runner-a"}})
	if err != nil || !matched {
		t.Fatalf("expected assigned job to match: matched=%v err=%v", matched, err)
	}
	matched, err = predicate.Matches(&ebsv1.Job{Status: ebsv1.JobStatus{Runner: "runner-b"}})
	if err != nil || matched {
		t.Fatalf("expected other runner job not to match: matched=%v err=%v", matched, err)
	}
}

func TestJobStorageStrategies(t *testing.T) {
	s := NewStorage()
	store := s.Resource.(*genericregistry.Store)
	statusStore := s.Status.(*genericregistry.Store)
	ctx := context.Background()
	created := &ebsv1.Job{Status: ebsv1.JobStatus{Phase: ebsv1.JobRunning}}
	store.CreateStrategy.PrepareForCreate(ctx, created)
	if created.Status.Phase != ebsv1.JobPending || created.Status.Stage != ebsv1.JobStagePending {
		t.Fatalf("initial status=%+v", created.Status)
	}
	old := &ebsv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job-a", Namespace: "project-a", Labels: map[string]string{"owner": "build-a"}},
		Status:     ebsv1.JobStatus{Phase: ebsv1.JobRunning, Runner: "runner-a"},
	}
	old.Spec.Payload = "payload"
	updated := old.DeepCopy()
	updated.Status = ebsv1.JobStatus{}
	store.UpdateStrategy.PrepareForUpdate(ctx, updated, old)
	if !reflect.DeepEqual(updated.Status, old.Status) {
		t.Fatalf("ordinary update status=%+v", updated.Status)
	}
	updated.Spec.Payload = "modified"
	updated.Labels = map[string]string{"owner": "other"}
	updated.Status.Phase = ebsv1.JobSucceeded
	statusStore.UpdateStrategy.PrepareForUpdate(ctx, updated, old)
	if !reflect.DeepEqual(updated.Spec, old.Spec) || !reflect.DeepEqual(updated.Labels, old.Labels) || updated.Status.Phase != ebsv1.JobSucceeded {
		t.Fatalf("status update=%+v", updated)
	}
}
