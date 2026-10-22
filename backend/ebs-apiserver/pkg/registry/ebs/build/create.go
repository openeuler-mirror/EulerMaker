package build

import (
	"context"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
)

func limitedBuild(build *ebsv1.Build) bool {
	switch build.Spec.BuildType {
	case "", "full", "incremental", "specified":
		return true
	default:
		return false
	}
}

var _ coordinationBackend = (*es.Client)(nil)

func validateLatestBuild(ctx context.Context, builds rest.Lister, build *ebsv1.Build) error {
	selector := labels.SelectorFromSet(labels.Set{
		ebsv1.BuildTargetOSLabel:   build.Spec.BuildTarget.Os,
		ebsv1.BuildTargetArchLabel: build.Spec.BuildTarget.Arch,
	})
	nonSingle, err := labels.NewRequirement(ebsv1.BuildTypeLabel, selection.NotEquals, []string{"single"})
	if err != nil {
		return err
	}
	// ES lists newest creationTimestamp first. Do not filter phase: only the
	// latest non-single Build is the gate, not all historical active Builds.
	ctx = esstore.WithSourceFilter(ctx, es.SourceFilter{Includes: []string{"data.metadata.name", "data.status.phase"}})
	obj, err := builds.List(ctx, &internalversion.ListOptions{LabelSelector: selector.Add(*nonSingle), Limit: 1})
	if err != nil {
		return err
	}
	list, ok := obj.(*ebsv1.BuildList)
	if !ok {
		return apierrors.NewInternalError(fmt.Errorf("expected BuildList, got %T", obj))
	}
	if len(list.Items) == 0 || list.Items[0].Status.Phase.IsTerminal() {
		return nil
	}
	previous := &list.Items[0]
	return apierrors.NewConflict(ebsv1.Resource("builds"), build.Name,
		fmt.Errorf("latest non-single build %q for target %s/%s is not terminal (phase %q)", previous.Name, build.Spec.BuildTarget.Os, build.Spec.BuildTarget.Arch, previous.Status.Phase))
}

func validateFullBuildBaseline(ctx context.Context, builds buildQueries, build *ebsv1.Build) error {
	if build.Spec.BuildType != "incremental" && build.Spec.BuildType != "specified" {
		return nil
	}
	exists, err := builds.Exists(ctx, &internalversion.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			ebsv1.BuildTargetOSLabel:   build.Spec.BuildTarget.Os,
			ebsv1.BuildTargetArchLabel: build.Spec.BuildTarget.Arch,
			ebsv1.BuildTypeLabel:       "full",
		}),
		FieldSelector: fields.OneTermEqualSelector("status.phase", string(ebsv1.BuildSuccess)),
	})
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReason("FullBuildRequired"),
		Message:  "No complete full build exists yet for this project and target",
		Code:     http.StatusPreconditionFailed,
	}}
}

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
		var content ebsv1.BuildTargetContent
		if err := yaml.UnmarshalStrict([]byte(conf.Spec.Content), &content); err != nil {
			return apierrors.NewServiceUnavailable("invalid build-target Config content")
		}
		if errs := validation.ValidateBuildTarget(build, &content); len(errs) > 0 {
			return apierrors.NewInvalid(ebsv1.Kind("Build"), build.Name, errs)
		}
		if next != nil {
			return next(ctx, obj)
		}
		return nil
	}
}

// ValidateProjectPackages checks requested packages against the current Project.
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
		if errs := validation.ValidateBuildPackages(build, p); len(errs) > 0 {
			return apierrors.NewInvalid(ebsv1.Kind("Build"), build.Name, errs)
		}
		return nil
	}
}
