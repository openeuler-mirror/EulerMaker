package project

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
	Project rest.StandardStorage
	Status  rest.StandardStorage
}

func NewStorage(scheme *runtime.Scheme) *Storage {
	strategy := &strategy{}
	statusStrategy := &statusStrategy{}

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Project{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.ProjectList{} },
		DefaultQualifiedResource:  ebsv1.Resource("projects"),
		SingularQualifiedResource: ebsv1.Resource("project"),
		CreateStrategy:            strategy,
		UpdateStrategy:            strategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("projects")),
	}

	statusStore := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Project{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.ProjectList{} },
		DefaultQualifiedResource:  ebsv1.Resource("projects"),
		SingularQualifiedResource: ebsv1.Resource("project"),
		UpdateStrategy:            statusStrategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("projects")),
	}

	return &Storage{
		Project: store,
		Status:  statusStore,
	}
}

type strategy struct{}

func (s *strategy) NamespaceScoped() bool          { return false }
func (s *strategy) AllowCreateOnUpdate() bool      { return false }
func (s *strategy) AllowUnconditionalUpdate() bool { return false }

func (s *strategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	p := obj.(*ebsv1.Project)
	ebsv1.SetDefaults_Project(p)
	p.Status = ebsv1.ProjectStatus{Phase: "Active"}
}

func (s *strategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newP := obj.(*ebsv1.Project)
	oldP := old.(*ebsv1.Project)
	newP.Status = oldP.Status
}

func (s *strategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return validation.ValidateProject(obj.(*ebsv1.Project))
}

func (s *strategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateProjectUpdate(obj.(*ebsv1.Project), old.(*ebsv1.Project))
}

func (s *strategy) Canonicalize(obj runtime.Object) {}
func (s *strategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Project"}}, false, nil
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
	newP := obj.(*ebsv1.Project)
	oldP := old.(*ebsv1.Project)
	newP.Spec = oldP.Spec
}

func (s *statusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateProjectStatusUpdate(obj.(*ebsv1.Project), old.(*ebsv1.Project))
}

func (s *statusStrategy) Canonicalize(obj runtime.Object) {}
func (s *statusStrategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Project"}}, false, nil
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
