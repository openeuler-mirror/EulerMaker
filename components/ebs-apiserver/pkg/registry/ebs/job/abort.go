package job

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"
)

type abort struct {
	getter  rest.Getter
	updater rest.Updater
	now     func() time.Time
}

func NewAbortStorage(getter rest.Getter, updater rest.Updater) rest.Storage {
	return &abort{getter: getter, updater: updater, now: time.Now}
}
func (*abort) NamespaceScoped() bool                             { return true }
func (*abort) New() runtime.Object                               { return &ebsv1.Job{} }
func (*abort) Destroy()                                          {}
func (*abort) NewConnectOptions() (runtime.Object, bool, string) { return nil, false, "" }
func (*abort) ConnectMethods() []string                          { return []string{http.MethodPost} }

func (a *abort) Connect(_ context.Context, name string, _ runtime.Object, responder rest.Responder) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			UID    string `json:"uid"`
			Reason string `json:"reason"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			responder.Error(apierrors.NewBadRequest(err.Error()))
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			responder.Error(apierrors.NewBadRequest("expected a single JSON object"))
			return
		}
		var errs field.ErrorList
		if strings.TrimSpace(input.UID) == "" {
			errs = append(errs, field.Required(field.NewPath("uid"), "Job UID is required"))
		}
		if utf8.RuneCountInString(input.Reason) > 1024 {
			errs = append(errs, field.TooLong(field.NewPath("reason"), "", 1024))
		}
		if len(errs) > 0 {
			responder.Error(apierrors.NewInvalid(ebsv1.SchemeGroupVersion.WithKind("Job").GroupKind(), name, errs))
			return
		}
		for attempt := 0; attempt < 5; attempt++ {
			obj, err := a.getter.Get(r.Context(), name, &metav1.GetOptions{})
			if err != nil {
				responder.Error(err)
				return
			}
			job := obj.(*ebsv1.Job)
			if string(job.UID) != input.UID {
				responder.Error(apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("Job UID changed")))
				return
			}
			switch job.Status.Phase {
			case ebsv1.JobSucceeded, ebsv1.JobFailed, ebsv1.JobAborted:
				responder.Object(http.StatusOK, job)
				return
			case ebsv1.JobPending, ebsv1.JobRunning:
			default:
				responder.Error(apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("cannot abort phase %q", job.Status.Phase)))
				return
			}
			next := job.DeepCopy()
			next.Status.Phase = ebsv1.JobAborted
			next.Status.EndTime = metav1.NewTime(a.now().UTC())
			next.Status.Message = input.Reason
			if next.Status.Message == "" {
				next.Status.Message = "job aborted by user"
			}
			updated, _, err := a.updater.Update(r.Context(), name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, &metav1.UpdateOptions{})
			if apierrors.IsConflict(err) {
				continue
			}
			if err != nil {
				responder.Error(err)
				return
			}
			responder.Object(http.StatusOK, updated)
			return
		}
		responder.Error(apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("Job changed during abort; retry with the same UID")))
	}), nil
}
