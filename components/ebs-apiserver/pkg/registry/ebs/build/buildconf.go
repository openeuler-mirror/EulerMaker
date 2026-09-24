package build

import (
	"context"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/yaml"
)

func ValidateBuildTargetConfig(configs rest.Getter, next rest.ValidateObjectFunc) rest.ValidateObjectFunc {
	return func(ctx context.Context, obj runtime.Object) error {
		build := obj.(*ebsv1.Build)
		configuration, err := configs.Get(request.WithNamespace(ctx, ""), ebsv1.BuildTargetConfigName, &metav1.GetOptions{})
		if err != nil {
			return apierrors.NewServiceUnavailable("build-target Config is unavailable")
		}
		conf, ok := configuration.(*ebsv1.Config)
		if !ok || conf == nil {
			return apierrors.NewServiceUnavailable("invalid build-target Config response")
		}
		var content ebsv1.BuildConfSpec
		if err := yaml.UnmarshalStrict([]byte(conf.Spec.Content), &content); err != nil {
			return apierrors.NewServiceUnavailable("invalid build-target Config content")
		}
		if len(validation.ValidateBuildConf(&ebsv1.BuildConf{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: content})) != 0 {
			return apierrors.NewServiceUnavailable("invalid build-target Config content")
		}
		if content.Targets[build.Spec.BuildTarget.Os].Arches[build.Spec.BuildTarget.Arch].Image == "" {
			return apierrors.NewInvalid(ebsv1.Kind("Build"), build.Name, field.ErrorList{field.Invalid(field.NewPath("spec", "buildTarget"), build.Spec.BuildTarget, "target is not configured in build-target Config")})
		}
		if next != nil {
			return next(ctx, obj)
		}
		return nil
	}
}
