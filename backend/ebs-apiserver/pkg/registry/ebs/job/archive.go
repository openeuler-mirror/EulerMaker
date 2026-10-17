package job

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
)

type jobArchiver interface {
	Archive(context.Context, *ebsv1.Job) (*ebsv1.Job, error)
}

type archive struct {
	getter  rest.Getter
	archive jobArchiver
}

func NewArchiveStorage(getter rest.Getter, archiver jobArchiver) rest.Storage {
	return &archive{getter: getter, archive: archiver}
}
func (*archive) NamespaceScoped() bool                             { return true }
func (*archive) New() runtime.Object                               { return &ebsv1.Job{} }
func (*archive) Destroy()                                          {}
func (*archive) NewConnectOptions() (runtime.Object, bool, string) { return nil, false, "" }
func (*archive) ConnectMethods() []string                          { return []string{http.MethodPost} }

func (a *archive) Connect(_ context.Context, name string, _ runtime.Object, responder rest.Responder) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Preconditions *metav1.Preconditions `json:"preconditions"`
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
		pre := input.Preconditions
		if pre == nil || pre.UID == nil || *pre.UID == "" || pre.ResourceVersion == nil || *pre.ResourceVersion == "" {
			responder.Error(apierrors.NewBadRequest("UID and resourceVersion preconditions are required"))
			return
		}
		obj, err := a.getter.Get(r.Context(), name, &metav1.GetOptions{})
		if err != nil {
			responder.Error(err)
			return
		}
		job := obj.(*ebsv1.Job)
		if job.UID != *pre.UID || job.ResourceVersion != *pre.ResourceVersion {
			responder.Error(apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("Job preconditions failed")))
			return
		}
		if !job.Status.Phase.IsTerminal() || job.Status.EndTime.IsZero() || job.DeletionTimestamp != nil || len(job.Finalizers) > 0 {
			responder.Error(apierrors.NewInvalid(ebsv1.Kind("Job"), name, field.ErrorList{
				field.Forbidden(field.NewPath("metadata"), "archive requires a terminal Job with endTime and no deletionTimestamp or finalizers"),
			}))
			return
		}
		result, err := a.archive.Archive(r.Context(), job)
		if err != nil {
			responder.Error(err)
			return
		}
		responder.Object(http.StatusOK, result)
	}), nil
}
