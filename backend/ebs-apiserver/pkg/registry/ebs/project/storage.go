package project

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "projects", Singular: "project", Kind: "Project", ClusterScoped: true,
		New:     func() runtime.Object { return &ebsv1.Project{} },
		NewList: func() runtime.Object { return &ebsv1.ProjectList{} },
		PrepareCreate: func(obj runtime.Object) {
			p := obj.(*ebsv1.Project)
			ebsv1.SetDefaults_Project(p)
			p.Status = ebsv1.ProjectStatus{Phase: ebsv1.ProjectActive}
		},
		CopyStatus: func(obj, old runtime.Object) {
			newP := obj.(*ebsv1.Project)
			oldP := old.(*ebsv1.Project)
			ebsv1.SetDefaults_Project(newP)
			newP.Status = oldP.Status
		},
		CopySpec: func(obj, old runtime.Object) {
			newP := obj.(*ebsv1.Project)
			oldP := old.(*ebsv1.Project)
			newP.Spec = oldP.Spec
			newP.Labels = oldP.Labels
			newP.Annotations = oldP.Annotations
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateProject(obj.(*ebsv1.Project))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateProjectUpdate(obj.(*ebsv1.Project), old.(*ebsv1.Project))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateProjectStatusUpdate(obj.(*ebsv1.Project), old.(*ebsv1.Project))
		},
	})
}
