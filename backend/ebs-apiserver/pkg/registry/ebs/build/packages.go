package build

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
)

// ValidateProjectPackages checks explicit build targets against the current Project.
// Project changes after creation are still handled by the snapshot controller.
func ValidateProjectPackages(projects rest.Getter) rest.ValidateObjectFunc {
	return func(ctx context.Context, obj runtime.Object) error {
		build, ok := obj.(*ebsv1.Build)
		if !ok || build == nil {
			return apierrors.NewInternalError(fmt.Errorf("expected Build, got %T", obj))
		}
		if build.Spec.BuildType != "single" && build.Spec.BuildType != "specified" {
			return nil
		}
		project, ok := request.NamespaceFrom(ctx)
		if !ok || project == "" {
			return apierrors.NewBadRequest("project namespace is required")
		}
		// Projects are cluster-scoped; never carry the Build namespace into their GET.
		parent, err := projects.Get(request.WithNamespace(ctx, ""), project, &metav1.GetOptions{})
		if err != nil {
			return err
		}
		p, ok := parent.(*ebsv1.Project)
		if !ok || p == nil {
			return apierrors.NewInternalError(fmt.Errorf("expected Project, got %T", parent))
		}
		names := make(map[string]struct{}, len(p.Spec.PackageRepos))
		for _, repo := range p.Spec.PackageRepos {
			names[repo.Name] = struct{}{}
		}
		var errs field.ErrorList
		for i, name := range build.Spec.Packages {
			if _, exists := names[name]; !exists {
				errs = append(errs, field.Invalid(field.NewPath("spec", "packages").Index(i), name, "package does not exist in Project.spec.packageRepos"))
			}
		}
		if len(errs) > 0 {
			return apierrors.NewInvalid(ebsv1.Kind("Build"), build.Name, errs)
		}
		return nil
	}
}
