package runner

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "runners", Singular: "runner", Kind: "Runner", ClusterScoped: true,
		New:     func() runtime.Object { return &ebsv1.Runner{} },
		NewList: func() runtime.Object { return &ebsv1.RunnerList{} },
		PrepareCreate: func(obj runtime.Object) {
			r := obj.(*ebsv1.Runner)
			ebsv1.SetDefaults_Runner(r)
			r.Status = ebsv1.RunnerStatus{Phase: ebsv1.RunnerOffline}
		},
		CopyStatus: func(obj, old runtime.Object) {
			newR, oldR := obj.(*ebsv1.Runner), old.(*ebsv1.Runner)
			newR.Spec.Type = oldR.Spec.Type
			newR.Spec.Arch = oldR.Spec.Arch
			newR.Status = oldR.Status
		},
		CopySpec: func(obj, old runtime.Object) {
			obj.(*ebsv1.Runner).Spec = old.(*ebsv1.Runner).Spec
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateRunner(obj.(*ebsv1.Runner))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateRunnerUpdate(obj.(*ebsv1.Runner), old.(*ebsv1.Runner))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateRunnerStatusUpdate(obj.(*ebsv1.Runner), old.(*ebsv1.Runner))
		},
		ValidateDelete: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateRunnerDelete(obj.(*ebsv1.Runner))
		},
	})
}
