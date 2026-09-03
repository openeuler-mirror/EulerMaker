package buildinfo

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "buildinfos", Singular: "buildinfo", Kind: "BuildInfo",
		New:     func() runtime.Object { return &ebsv1.BuildInfo{} },
		NewList: func() runtime.Object { return &ebsv1.BuildInfoList{} },
		PrepareCreate: func(obj runtime.Object) {
			obj.(*ebsv1.BuildInfo).Status = ebsv1.BuildInfoStatus{Phase: "Pending"}
		},
		CopyStatus: func(obj, old runtime.Object) {
			obj.(*ebsv1.BuildInfo).Status = old.(*ebsv1.BuildInfo).Status
		},
		CopySpec: func(obj, old runtime.Object) {
			obj.(*ebsv1.BuildInfo).Spec = old.(*ebsv1.BuildInfo).Spec
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateBuildInfo(obj.(*ebsv1.BuildInfo))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateBuildInfoUpdate(obj.(*ebsv1.BuildInfo), old.(*ebsv1.BuildInfo))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateBuildInfoStatusUpdate(obj.(*ebsv1.BuildInfo), old.(*ebsv1.BuildInfo))
		},
	})
}
