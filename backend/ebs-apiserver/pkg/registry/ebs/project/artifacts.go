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
)

type artifactManagerConfig struct {
	Instances map[string]struct {
		InternalURL string `json:"internalURL"`
	} `json:"instances"`
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
	instance, exists := project.Annotations[ebsv1.ProjectArtifactsAnnotation]
	if !exists {
		instance = "default"
	}
	obj, err := configs.Get(request.WithNamespace(ctx, ""), ebsv1.ArtifactManagerConfigName, &metav1.GetOptions{})
	if err != nil {
		return apierrors.NewServiceUnavailable("artifact-manager-conf Config is unavailable")
	}
	config, ok := obj.(*ebsv1.Config)
	if !ok || config == nil {
		return apierrors.NewServiceUnavailable("invalid artifact-manager-conf Config response")
	}
	var content artifactManagerConfig
	if err := yaml.UnmarshalStrict([]byte(config.Spec.Content), &content); err != nil || len(content.Instances) == 0 {
		return apierrors.NewServiceUnavailable("invalid artifact-manager-conf Config content")
	}
	entry, found := content.Instances[instance]
	if !found {
		return apierrors.NewInvalid(ebsv1.Kind("Project"), project.Name, field.ErrorList{
			field.Invalid(field.NewPath("metadata", "annotations").Key(ebsv1.ProjectArtifactsAnnotation), instance,
				"instance is not configured in artifact-manager-conf"),
		})
	}
	if entry.InternalURL == "" {
		return apierrors.NewServiceUnavailable("invalid artifact-manager-conf Config content")
	}
	return nil
}
