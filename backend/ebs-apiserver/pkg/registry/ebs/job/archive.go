package job

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
)

type archive struct {
	lifecycle *Lifecycle
	finalizer bool
	oldJob    bool
}

func NewArchiveStorage(lifecycle *Lifecycle) rest.Storage { return &archive{lifecycle: lifecycle} }

func NewFinalizerStorage(lifecycle *Lifecycle, oldJob bool) rest.Storage {
	return &archive{lifecycle: lifecycle, finalizer: true, oldJob: oldJob}
}

func (*archive) NamespaceScoped() bool                             { return true }
func (*archive) New() runtime.Object                               { return &ebsv1.Job{} }
func (*archive) Destroy()                                          {}
func (*archive) NewConnectOptions() (runtime.Object, bool, string) { return nil, false, "" }

func (a *archive) ConnectMethods() []string {
	if a.finalizer {
		return []string{http.MethodPut}
	}
	return []string{http.MethodGet, http.MethodPost}
}

func (a *archive) Connect(_ context.Context, name string, _ runtime.Object, responder rest.Responder) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			state, err := a.lifecycle.ArchiveState(r.Context(), name)
			if err != nil {
				responder.Error(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
			return
		}
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
		var result *ebsv1.Job
		var err error
		switch {
		case a.finalizer && a.oldJob:
			result, err = a.lifecycle.FinalizeOldJob(r.Context(), name, pre)
		case a.finalizer:
			result, err = a.lifecycle.Finalize(r.Context(), name, pre)
		default:
			result, err = a.lifecycle.Archive(r.Context(), name, pre)
		}
		if err != nil {
			responder.Error(err)
			return
		}
		responder.Object(http.StatusOK, result)
	}), nil
}
