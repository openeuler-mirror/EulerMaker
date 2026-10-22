package project

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/yaml"

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

func ValidateArtifactsConfig(configs rest.Getter) rest.ValidateObjectFunc {
	return func(ctx context.Context, obj runtime.Object) error {
		return validateArtifactsConfig(ctx, obj.(*ebsv1.Project), configs)
	}
}

func ValidateArtifactsConfigUpdate(configs rest.Getter) rest.ValidateObjectUpdateFunc {
	return func(ctx context.Context, newObj, oldObj runtime.Object) error {
		project := newObj.(*ebsv1.Project)
		old := oldObj.(*ebsv1.Project)
		newValue, newSet := project.Annotations[ebsv1.ProjectArtifactsAnnotation]
		oldValue, oldSet := old.Annotations[ebsv1.ProjectArtifactsAnnotation]
		if newValue == oldValue && newSet == oldSet {
			return nil
		}
		return validateArtifactsConfig(ctx, project, configs)
	}
}

func validateArtifactsConfig(ctx context.Context, project *ebsv1.Project, configs rest.Getter) error {
	obj, err := configs.Get(request.WithNamespace(ctx, ""), ebsv1.ServiceConfigName, &metav1.GetOptions{})
	if err != nil {
		return apierrors.NewServiceUnavailable("service-config Config is unavailable")
	}
	config, ok := obj.(*ebsv1.Config)
	if !ok || config == nil {
		return apierrors.NewServiceUnavailable("invalid service-config Config response")
	}
	var content ebsv1.ServiceConfigContent
	if err := yaml.UnmarshalStrict([]byte(config.Spec.Content), &content); err != nil {
		return apierrors.NewServiceUnavailable("invalid service-config Config content")
	}
	if errs := validation.ValidateProjectArtifactInstance(project, &content); len(errs) > 0 {
		return apierrors.NewInvalid(ebsv1.Kind("Project"), project.Name, errs)
	}
	return nil
}
