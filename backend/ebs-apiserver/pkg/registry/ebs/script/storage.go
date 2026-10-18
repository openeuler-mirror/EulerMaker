package script

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "scripts", Singular: "script", Kind: "Script", ClusterScoped: true,
		New:     func() runtime.Object { return &ebsv1.Script{} },
		NewList: func() runtime.Object { return &ebsv1.ScriptList{} },
		PrepareCreate: func(obj runtime.Object) {
			p := obj.(*ebsv1.Script)
			p.Generation = 1
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateScript(obj.(*ebsv1.Script))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateScript(obj.(*ebsv1.Script))
		},
	})
}
