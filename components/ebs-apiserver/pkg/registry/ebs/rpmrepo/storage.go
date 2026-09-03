package rpmrepo

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "rpmrepos", Singular: "rpmrepo", Kind: "RpmRepo",
		New:     func() runtime.Object { return &ebsv1.RpmRepo{} },
		NewList: func() runtime.Object { return &ebsv1.RpmRepoList{} },
		PrepareCreate: func(obj runtime.Object) {
			obj.(*ebsv1.RpmRepo).Status = ebsv1.RpmRepoStatus{Phase: "Pending"}
		},
		CopyStatus: func(obj, old runtime.Object) {
			obj.(*ebsv1.RpmRepo).Status = old.(*ebsv1.RpmRepo).Status
		},
		CopySpec: func(obj, old runtime.Object) {
			obj.(*ebsv1.RpmRepo).Spec = old.(*ebsv1.RpmRepo).Spec
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateRpmRepo(obj.(*ebsv1.RpmRepo))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateRpmRepoUpdate(obj.(*ebsv1.RpmRepo), old.(*ebsv1.RpmRepo))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateRpmRepoStatusUpdate(obj.(*ebsv1.RpmRepo), old.(*ebsv1.RpmRepo))
		},
	})
}
