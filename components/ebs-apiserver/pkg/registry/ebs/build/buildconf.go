package build

import (
	"context"

	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
)

func ValidateBuildTargetConfig(configs rest.Getter, next rest.ValidateObjectFunc) rest.ValidateObjectFunc {
	return func(ctx context.Context, obj runtime.Object) error {
		build := obj.(*ebsv1.Build)
		configuration, err := configs.Get(request.WithNamespace(ctx, ""), "default", &metav1.GetOptions{})
		if err != nil {
			return apierrors.NewServiceUnavailable("BuildConf is unavailable")
		}
		conf, ok := configuration.(*ebsv1.BuildConf)
		if !ok || conf == nil {
			return apierrors.NewServiceUnavailable("invalid BuildConf response")
		}
		if conf.Spec.Targets[build.Spec.BuildTarget.Os].Arches[build.Spec.BuildTarget.Arch].Image == "" {
			return apierrors.NewInvalid(ebsv1.Kind("Build"), build.Name, field.ErrorList{field.Invalid(field.NewPath("spec", "buildTarget"), build.Spec.BuildTarget, "target is not configured in BuildConf")})
		}
		if next != nil {
			return next(ctx, obj)
		}
		return nil
	}
}
