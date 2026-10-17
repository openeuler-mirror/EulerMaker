package job

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
)

type archiveFunc func(context.Context, *ebsv1.Job) (*ebsv1.Job, error)

func (f archiveFunc) Archive(ctx context.Context, job *ebsv1.Job) (*ebsv1.Job, error) {
	return f(ctx, job)
}

func TestArchivePreconditionsAndEligibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*ebsv1.Job)
		missing bool
		body    string
		code    int
	}{
		{name: "terminal", code: 200},
		{name: "source deleted", missing: true, code: 404},
		{name: "version changed", change: func(j *ebsv1.Job) { j.ResourceVersion = "11" }, code: 409},
		{name: "UID changed", change: func(j *ebsv1.Job) { j.UID = "other" }, code: 409},
		{name: "running", change: func(j *ebsv1.Job) { j.Status.Phase = ebsv1.JobRunning }, code: 422},
		{name: "end time missing", change: func(j *ebsv1.Job) { j.Status.EndTime = metav1.Time{} }, code: 422},
		{name: "finalizer", change: func(j *ebsv1.Job) { j.Finalizers = []string{"cleanup"} }, code: 422},
		{name: "deleting", change: func(j *ebsv1.Job) { now := metav1.Now(); j.DeletionTimestamp = &now }, code: 422},
		{name: "preconditions missing", body: `{}`, code: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "10"}, Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded, EndTime: metav1.NewTime(time.Unix(100, 0))}}
			if tc.change != nil {
				tc.change(source)
			}
			if tc.missing {
				source = nil
			}
			calls := 0
			storage := NewArchiveStorage(&abortStore{job: source}, archiveFunc(func(_ context.Context, j *ebsv1.Job) (*ebsv1.Job, error) {
				calls++
				return j.DeepCopy(), nil
			})).(rest.Connecter)
			response := &abortResponse{}
			h, err := storage.Connect(context.Background(), "job", nil, response)
			if err != nil {
				t.Fatal(err)
			}
			body := tc.body
			if body == "" {
				body = `{"preconditions":{"uid":"uid","resourceVersion":"10"}}`
			}
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(body)))
			if tc.code == 200 {
				if response.err != nil || calls != 1 || response.obj.(*ebsv1.Job).ResourceVersion != "10" {
					t.Fatalf("response=%+v calls=%d", response, calls)
				}
			} else {
				status, ok := response.err.(apierrors.APIStatus)
				if !ok || int(status.Status().Code) != tc.code || calls != 0 {
					t.Fatalf("error=%v calls=%d", response.err, calls)
				}
			}
		})
	}
}
