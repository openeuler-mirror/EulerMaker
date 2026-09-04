package statement

import (
	"context"
	"errors"
	"testing"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"scheduler/pkg/cache"
	"scheduler/pkg/client"
	"scheduler/pkg/framework"
)

type fakeCache struct{ assumes, forgets int }

func (f *fakeCache) Snapshot(string, types.UID) (*framework.Snapshot, error) { return nil, nil }
func (f *fakeCache) Assume(cache.AssumeRequest) (uint64, error)              { f.assumes++; return 7, nil }
func (f *fakeCache) Forget(string, types.UID, uint64) bool                   { f.forgets++; return true }
func (f *fakeCache) GetAssumed(string, types.UID) (*cache.AssumedJob, bool)  { return nil, false }
func (f *fakeCache) ClaimExpiredAssumed(time.Time, time.Duration, int) []*cache.AssumedJob {
	return nil
}

type fakeJobs struct {
	current, response *ebsv1.Job
	updateErr         error
	gets, updates     int
}

func (f *fakeJobs) List(context.Context, metav1.ListOptions) (*ebsv1.JobList, error) { return nil, nil }
func (f *fakeJobs) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, nil
}
func (f *fakeJobs) Get(context.Context, string, string, metav1.GetOptions) (*ebsv1.Job, error) {
	f.gets++
	return f.current.DeepCopy(), nil
}
func (f *fakeJobs) UpdateStatus(context.Context, string, string, *ebsv1.Job, metav1.UpdateOptions) (*ebsv1.Job, error) {
	f.updates++
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.response == nil {
		return nil, nil
	}
	return f.response.DeepCopy(), nil
}
func pending() *ebsv1.Job {
	return &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "j", UID: "u", ResourceVersion: "1"}, Status: ebsv1.JobStatus{Phase: "Pending"}}
}
func request() Request {
	return Request{JobKey: "p/j", JobUID: "u", RunnerName: "r", RunnerUID: "ru", RunnerRevision: 1, JobResourceVersion: "1"}
}
func TestUnknownWriteRetainsAssumptionAndIsIdempotent(t *testing.T) {
	fc := &fakeCache{}
	jobs := &fakeJobs{current: pending(), updateErr: &client.WriteError{Outcome: client.WriteUnknown, Err: errors.New("timeout")}}
	s, err := New(fc, jobs, request())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Commit(context.Background()); !errors.Is(err, ErrBindOutcomeUnknown) {
		t.Fatalf("err=%v", err)
	}
	s.Discard()
	_ = s.Commit(context.Background())
	if fc.assumes != 1 || fc.forgets != 0 || jobs.updates != 1 {
		t.Fatalf("assumes=%d forgets=%d updates=%d", fc.assumes, fc.forgets, jobs.updates)
	}
}
func TestUnexpectedSuccessResponseIsUnknown(t *testing.T) {
	fc := &fakeCache{}
	jobs := &fakeJobs{current: pending(), response: pending()}
	s, _ := New(fc, jobs, request())
	if err := s.Commit(context.Background()); !errors.Is(err, ErrBindOutcomeUnknown) {
		t.Fatalf("err=%v", err)
	}
	if fc.forgets != 0 {
		t.Fatal("unknown outcome must retain assumption")
	}
}
func TestConflictIsRechecked(t *testing.T) {
	fc := &fakeCache{}
	jobs := &fakeJobs{current: pending(), updateErr: &client.WriteError{Outcome: client.WriteRejected, Err: apierrors.NewConflict(schema.GroupResource{Group: "ebs", Resource: "jobs"}, "j", errors.New("conflict"))}}
	s, _ := New(fc, jobs, request())
	if err := s.Commit(context.Background()); !errors.Is(err, ErrConflictRetryable) {
		t.Fatalf("err=%v", err)
	}
	if fc.forgets != 1 || jobs.gets != 2 {
		t.Fatalf("forgets=%d gets=%d", fc.forgets, jobs.gets)
	}
}
