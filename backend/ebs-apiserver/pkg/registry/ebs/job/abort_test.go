package job

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
)

type abortStore struct {
	job           *ebsv1.Job
	conflictPhase ebsv1.JobPhase
	writes        int
}

func (s *abortStore) New() runtime.Object { return &ebsv1.Job{} }
func (s *abortStore) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	if s.job == nil {
		return nil, apierrors.NewNotFound(ebsv1.Resource("jobs"), "job")
	}
	return s.job.DeepCopy(), nil
}
func (s *abortStore) Update(ctx context.Context, name string, info rest.UpdatedObjectInfo, _ rest.ValidateObjectFunc, _ rest.ValidateObjectUpdateFunc, _ bool, _ *metav1.UpdateOptions) (runtime.Object, bool, error) {
	s.writes++
	if s.conflictPhase != "" {
		s.job.Status.Phase = s.conflictPhase
		s.conflictPhase = ""
		return nil, false, apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("concurrent update"))
	}
	obj, err := info.UpdatedObject(ctx, s.job)
	if err == nil {
		s.job = obj.(*ebsv1.Job)
	}
	return obj, false, err
}

type abortResponse struct {
	obj runtime.Object
	err error
}

func (r *abortResponse) Object(_ int, obj runtime.Object) { r.obj = obj }
func (r *abortResponse) Error(err error)                  { r.err = err }

func TestAbortJob(t *testing.T) {
	for _, phase := range []ebsv1.JobPhase{ebsv1.JobPending, ebsv1.JobRunning, ebsv1.JobSucceeded, ebsv1.JobFailed, ebsv1.JobAborted} {
		t.Run(string(phase), func(t *testing.T) {
			original := &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", UID: "uid", ResourceVersion: "1"}, Status: ebsv1.JobStatus{Phase: phase, Stage: ebsv1.JobStagePostRun, Runner: "runner", Message: "original"}}
			s := &abortStore{job: original.DeepCopy()}
			r := &abortResponse{}
			now := time.Now().UTC().Truncate(time.Second)
			h, _ := (&abort{getter: s, updater: s, now: func() time.Time { return now }}).Connect(context.Background(), "job", nil, r)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(`{"uid":"uid","reason":"stop"}`)))
			if r.err != nil {
				t.Fatal(r.err)
			}
			if phase.IsTerminal() {
				if s.writes != 0 || s.job.Status.Message != "original" {
					t.Fatal("terminal Job changed")
				}
			} else if s.job.Status.Phase != ebsv1.JobAborted || !s.job.Status.EndTime.Time.Equal(now) || s.job.Status.Runner != "runner" || s.job.Status.Stage != original.Status.Stage {
				t.Fatalf("unexpected status: %+v", s.job.Status)
			}
		})
	}
}

func TestAbortInputAndCompletionRace(t *testing.T) {
	for _, tc := range []struct {
		body     string
		conflict ebsv1.JobPhase
		code     int
	}{
		{`{"uid":"other"}`, "", 409}, {`{}`, "", 422}, {`{"uid":"uid","phase":"Aborted"}`, "", 400},
		{`{"uid":"uid"} {}`, "", 400}, {`{"uid":"uid","reason":"` + strings.Repeat("中", 1025) + `"}`, "", 422},
		{`{"uid":"uid"}`, ebsv1.JobSucceeded, 200},
	} {
		s := &abortStore{job: &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{UID: "uid"}, Status: ebsv1.JobStatus{Phase: ebsv1.JobRunning}}, conflictPhase: tc.conflict}
		r := &abortResponse{}
		h, _ := (&abort{getter: s, updater: s, now: time.Now}).Connect(context.Background(), "job", nil, r)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(tc.body)))
		if tc.code == 200 {
			if r.err != nil || s.job.Status.Phase != ebsv1.JobSucceeded || s.writes != 1 {
				t.Fatalf("race: %+v %v", s, r.err)
			}
			continue
		}
		status, ok := r.err.(apierrors.APIStatus)
		if !ok || int(status.Status().Code) != tc.code {
			t.Fatalf("want %d got %v", tc.code, r.err)
		}
	}
}

func TestTerminalJobStatusImmutable(t *testing.T) {
	for _, phase := range []ebsv1.JobPhase{ebsv1.JobSucceeded, ebsv1.JobFailed, ebsv1.JobAborted} {
		old := &ebsv1.Job{Status: ebsv1.JobStatus{Phase: phase, Stage: ebsv1.JobStagePostRun}}
		if errs := validation.ValidateJobStatusUpdate(old.DeepCopy(), old); len(errs) != 0 {
			t.Fatal(errs)
		}
		next := old.DeepCopy()
		next.Status.Message = "overwrite"
		if len(validation.ValidateJobStatusUpdate(next, old)) == 0 {
			t.Fatal("terminal message mutable")
		}
		next = old.DeepCopy()
		next.Status.Phase = ebsv1.JobRunning
		if len(validation.ValidateJobStatusUpdate(next, old)) == 0 {
			t.Fatal("terminal phase mutable")
		}
	}
}
