package config

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() rest.StandardStorage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "configs", Singular: "config", Kind: "Config", ClusterScoped: true,
		New:            func() runtime.Object { return &ebsv1.Config{} },
		NewList:        func() runtime.Object { return &ebsv1.ConfigList{} },
		PrepareCreate:  func(runtime.Object) {},
		CopyStatus:     func(runtime.Object, runtime.Object) {},
		CopySpec:       func(runtime.Object, runtime.Object) {},
		Validate:       func(obj runtime.Object) field.ErrorList { return validation.ValidateConfig(obj.(*ebsv1.Config)) },
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList { return validation.ValidateConfig(obj.(*ebsv1.Config)) },
		ValidateStatus: func(runtime.Object, runtime.Object) field.ErrorList { return nil },
	}).Resource
}
