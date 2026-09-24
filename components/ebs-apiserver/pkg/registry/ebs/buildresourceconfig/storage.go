package buildresourceconfig

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() rest.StandardStorage {
	storage := scopedresource.NewStorage(scopedresource.Config{
		Resource: "buildresourceconfigs", Singular: "buildresourceconfig", Kind: "BuildResourceConfig", ClusterScoped: true,
		New:           func() runtime.Object { return &ebsv1.BuildResourceConfig{} },
		NewList:       func() runtime.Object { return &ebsv1.BuildResourceConfigList{} },
		PrepareCreate: func(runtime.Object) {},
		CopyStatus:    func(runtime.Object, runtime.Object) {},
		CopySpec:      func(runtime.Object, runtime.Object) {},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateBuildResourceConfig(obj.(*ebsv1.BuildResourceConfig))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateBuildResourceConfigUpdate(obj.(*ebsv1.BuildResourceConfig), old.(*ebsv1.BuildResourceConfig))
		},
		ValidateStatus: func(runtime.Object, runtime.Object) field.ErrorList { return nil },
	})
	return storage.Resource
}
