package runner

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
)

type Storage struct {
	Runner rest.StandardStorage
	Status rest.StandardStorage
}

type strategy struct{}

func (s *strategy) NamespaceScoped() bool          { return false }
func (s *strategy) AllowCreateOnUpdate() bool      { return false }
func (s *strategy) AllowUnconditionalUpdate() bool { return false }

func (s *strategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	r := obj.(*ebsv1.Runner)
	r.Status = ebsv1.RunnerStatus{Phase: "Registering"}
}

func (s *strategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newR := obj.(*ebsv1.Runner)
	oldR := old.(*ebsv1.Runner)
	newR.Spec.Type = oldR.Spec.Type
	newR.Spec.Arch = oldR.Spec.Arch
}

func (s *strategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return validation.ValidateRunner(obj.(*ebsv1.Runner))
}

func (s *strategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateRunnerUpdate(obj.(*ebsv1.Runner), old.(*ebsv1.Runner))
}

func (s *strategy) Canonicalize(obj runtime.Object) {}
func (s *strategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Runner"}}, false, nil
}
func (s *strategy) GenerateName(base string) string { return base }
func (s *strategy) Recognizes(gvk schema.GroupVersionKind) bool {
	return gvk.Group == "ebs" && gvk.Version == "v1"
}
func (s *strategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string { return nil }
func (s *strategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

type statusStrategy struct{}

func (s *statusStrategy) NamespaceScoped() bool          { return false }
func (s *statusStrategy) AllowCreateOnUpdate() bool      { return false }
func (s *statusStrategy) AllowUnconditionalUpdate() bool { return false }

func (s *statusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newR := obj.(*ebsv1.Runner)
	oldR := old.(*ebsv1.Runner)
	newR.Spec = oldR.Spec
}

func (s *statusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateRunnerStatusUpdate(obj.(*ebsv1.Runner), old.(*ebsv1.Runner))
}

func (s *statusStrategy) Canonicalize(obj runtime.Object) {}
func (s *statusStrategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Runner"}}, false, nil
}
func (s *statusStrategy) GenerateName(base string) string { return base }
func (s *statusStrategy) Recognizes(gvk schema.GroupVersionKind) bool {
	return gvk.Group == "ebs" && gvk.Version == "v1"
}
func (s *statusStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}
func (s *statusStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

func NewStorage(scheme *runtime.Scheme) *Storage {
	strategy := &strategy{}
	statusStrategy := &statusStrategy{}

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Runner{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.RunnerList{} },
		DefaultQualifiedResource:  ebsv1.Resource("runners"),
		SingularQualifiedResource: ebsv1.Resource("runner"),
		CreateStrategy:            strategy,
		UpdateStrategy:            strategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("runners")),
	}

	statusStore := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Runner{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.RunnerList{} },
		DefaultQualifiedResource:  ebsv1.Resource("runners"),
		SingularQualifiedResource: ebsv1.Resource("runner"),
		UpdateStrategy:            statusStrategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("runners")),
	}

	return &Storage{
		Runner: store,
		Status: statusStore,
	}
}
