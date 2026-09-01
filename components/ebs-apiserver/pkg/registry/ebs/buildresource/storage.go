package buildresource

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() rest.StandardStorage {
	storage := scopedresource.NewStorage(scopedresource.Config{
		Resource: "buildresources", Singular: "buildresource", Kind: "BuildResource",
		New:           func() runtime.Object { return &ebsv1.BuildResource{} },
		NewList:       func() runtime.Object { return &ebsv1.BuildResourceList{} },
		PrepareCreate: func(runtime.Object) {},
		CopyStatus:    func(runtime.Object, runtime.Object) {},
		CopySpec:      func(runtime.Object, runtime.Object) {},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateBuildResource(obj.(*ebsv1.BuildResource))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateBuildResourceUpdate(obj.(*ebsv1.BuildResource), old.(*ebsv1.BuildResource))
		},
		ValidateStatus: func(runtime.Object, runtime.Object) field.ErrorList { return nil },
	})
	return storage.Resource
}
