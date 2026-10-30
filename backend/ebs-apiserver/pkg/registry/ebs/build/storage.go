package build

import (
	"context"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "builds", Singular: "build", Kind: "Build",
		New:     func() runtime.Object { return &ebsv1.Build{} },
		NewList: func() runtime.Object { return &ebsv1.BuildList{} },
		PrepareCreate: func(obj runtime.Object) {
			b := obj.(*ebsv1.Build)
			ebsv1.SetDefaults_Build(b)
			if b.Spec.BuildType == ebsv1.BuildTypeFull || b.Spec.BuildType == ebsv1.BuildTypeIncremental {
				b.Spec.Packages = nil
			}
			ensureBuildTargetLabels(b)
			b.Status = ebsv1.BuildStatus{Phase: ebsv1.BuildPending}
		},
		CopyStatus: func(obj, old runtime.Object) {
			newB := obj.(*ebsv1.Build)
			oldB := old.(*ebsv1.Build)
			ensureBuildTargetLabels(newB)
			newB.Status = oldB.Status
		},
		CopySpec: func(obj, old runtime.Object) {
			newB := obj.(*ebsv1.Build)
			oldB := old.(*ebsv1.Build)
			metav1.ResetObjectMetaForStatus(&newB.ObjectMeta, &oldB.ObjectMeta)
			newB.Spec = oldB.Spec
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateBuild(obj.(*ebsv1.Build))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateBuildUpdate(obj.(*ebsv1.Build), old.(*ebsv1.Build))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateBuildStatusUpdate(obj.(*ebsv1.Build), old.(*ebsv1.Build))
		},
	})
}

type abort struct {
	getter  rest.Getter
	updater rest.Updater
	now     func() time.Time
}

func (a *abort) NamespaceScoped() bool { return true }
func (a *abort) New() runtime.Object   { return &ebsv1.Build{} }
func (a *abort) Connect(ctx context.Context, id string, options runtime.Object, responder rest.Responder) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		obj, err := a.getter.Get(req.Context(), id, &metav1.GetOptions{})
		if err != nil {
			responder.Error(err)
			return
		}
		build := obj.(*ebsv1.Build)
		switch build.Status.Phase {
		case ebsv1.BuildAborted:
			responder.Object(http.StatusOK, build)
			return
		case ebsv1.BuildPending, ebsv1.BuildPrepared, ebsv1.BuildProcessing:
		default:
			responder.Error(apierrors.NewConflict(ebsv1.Resource("builds"), id, fmt.Errorf("build in phase %q cannot be aborted", build.Status.Phase)))
			return
		}
		next := build.DeepCopy()
		next.Status.Phase = ebsv1.BuildAborted
		next.Status.EndTime = metav1.NewTime(a.now().UTC())
		updated, _, err := a.updater.Update(
			req.Context(), id, rest.DefaultUpdatedObjectInfo(next),
			nil, nil, false, &metav1.UpdateOptions{},
		)
		if err != nil {
			responder.Error(err)
			return
		}
		responder.Object(http.StatusOK, updated)
	}), nil
}
func (a *abort) NewConnectOptions() (runtime.Object, bool, string) { return nil, false, "" }
func (a *abort) ConnectMethods() []string                          { return []string{http.MethodPost} }
func (a *abort) Destroy()                                          {}

func NewAbortStorage(getter rest.Getter, updater rest.Updater) rest.Storage {
	return &abort{getter: getter, updater: updater, now: time.Now}
}

func ensureBuildTargetLabels(build *ebsv1.Build) {
	if build.Labels == nil {
		build.Labels = make(map[string]string, 3)
	}

	if _, ok := build.Labels[ebsv1.BuildTargetArchLabel]; !ok && len(build.Spec.Arch) > 0 {
		build.Labels[ebsv1.BuildTargetArchLabel] = build.Spec.Arch
	}
	if _, ok := build.Labels[ebsv1.BuildTypeLabel]; !ok && len(build.Spec.BuildType) > 0 {
		build.Labels[ebsv1.BuildTypeLabel] = string(build.Spec.BuildType)
	}
}
