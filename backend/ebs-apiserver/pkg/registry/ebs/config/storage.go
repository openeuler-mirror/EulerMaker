package config

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "configs", Singular: "config", Kind: "Config", ClusterScoped: true,
		New:            func() runtime.Object { return &ebsv1.Config{} },
		NewList:        func() runtime.Object { return &ebsv1.ConfigList{} },
		Validate:       func(obj runtime.Object) field.ErrorList { return validation.ValidateConfig(obj.(*ebsv1.Config)) },
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList { return validation.ValidateConfig(obj.(*ebsv1.Config)) },
	})
}
