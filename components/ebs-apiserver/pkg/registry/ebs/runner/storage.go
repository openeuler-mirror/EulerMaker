package runner

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
)

type Storage struct {
	Runner         *genericregistry.Store
	Status         rest.Storage
	statusStrategy rest.RESTUpdateStrategy
}

type StatusREST struct {
	store *genericregistry.Store
}

type strategy struct{}

func (s *strategy) NamespaceScoped() bool          { return false }
func (s *strategy) AllowCreateOnUpdate() bool      { return false }
func (s *strategy) AllowUnconditionalUpdate() bool { return false }

func (s *strategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	r := obj.(*ebsv1.Runner)
	ebsv1.SetDefaults_Runner(r)
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

	return &Storage{
		Runner:         store,
		statusStrategy: statusStrategy,
	}
}

func (s *Storage) CompleteWithOptions(options *generic.StoreOptions) error {
	if err := s.Runner.CompleteWithOptions(options); err != nil {
		return err
	}
	statusStore := *s.Runner
	statusStore.UpdateStrategy = s.statusStrategy
	statusStore.CreateStrategy = nil
	statusStore.DeleteStrategy = nil
	statusStore.DestroyFunc = nil
	s.Status = &StatusREST{store: &statusStore}
	return nil
}

func (r *StatusREST) New() runtime.Object { return r.store.New() }
func (r *StatusREST) Destroy()            {}
func (r *StatusREST) NamespaceScoped() bool {
	return r.store.UpdateStrategy.NamespaceScoped()
}
func (r *StatusREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return r.store.Get(ctx, name, options)
}
func (r *StatusREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return r.store.Update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
}
