package job

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
)

type Storage struct {
	Job            *genericregistry.Store
	Status         rest.Storage
	statusStrategy rest.RESTUpdateStrategy
}

type StatusREST struct {
	store *genericregistry.Store
}

func NewStorage(scheme *runtime.Scheme) *Storage {
	strategy := &strategy{}
	statusStrategy := &statusStrategy{}

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Job{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.JobList{} },
		DefaultQualifiedResource:  ebsv1.Resource("jobs"),
		SingularQualifiedResource: ebsv1.Resource("job"),
		CreateStrategy:            strategy,
		UpdateStrategy:            strategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("jobs")),
		PredicateFunc:             matchJob,
	}

	return &Storage{
		Job:            store,
		statusStrategy: statusStrategy,
	}
}

func (s *Storage) CompleteWithOptions(options *generic.StoreOptions) error {
	if err := s.Job.CompleteWithOptions(jobStoreOptions(options)); err != nil {
		return err
	}
	statusStore := *s.Job
	statusStore.UpdateStrategy = s.statusStrategy
	statusStore.CreateStrategy = nil
	statusStore.DeleteStrategy = nil
	statusStore.DestroyFunc = nil
	s.Status = &StatusREST{store: &statusStore}
	return nil
}

func jobStoreOptions(options *generic.StoreOptions) *generic.StoreOptions {
	jobOptions := *options
	jobOptions.AttrFunc = jobAttrs
	return &jobOptions
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

func matchJob(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: field, GetAttrs: jobAttrs}
}

func jobAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	job, ok := obj.(*ebsv1.Job)
	if !ok {
		return nil, nil, fmt.Errorf("expected Job, got %T", obj)
	}
	return labels.Set(job.Labels), fields.Set{
		"metadata.name":      job.Name,
		"metadata.namespace": job.Namespace,
		"status.runner":      job.Status.Runner,
		"status.phase":       job.Status.Phase,
	}, nil
}

type strategy struct{}

func (s *strategy) NamespaceScoped() bool          { return true }
func (s *strategy) AllowCreateOnUpdate() bool      { return false }
func (s *strategy) AllowUnconditionalUpdate() bool { return false }

func (s *strategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	j := obj.(*ebsv1.Job)
	ebsv1.SetDefaults_Job(j)
	j.Status = ebsv1.JobStatus{Phase: "Pending"}
}

func (s *strategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newJ := obj.(*ebsv1.Job)
	oldJ := old.(*ebsv1.Job)
	newJ.Status = oldJ.Status
}

func (s *strategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return validation.ValidateJob(obj.(*ebsv1.Job))
}

func (s *strategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateJobUpdate(obj.(*ebsv1.Job), old.(*ebsv1.Job))
}

func (s *strategy) Canonicalize(obj runtime.Object) {}
func (s *strategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Job"}}, false, nil
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

func (s *statusStrategy) NamespaceScoped() bool          { return true }
func (s *statusStrategy) AllowCreateOnUpdate() bool      { return false }
func (s *statusStrategy) AllowUnconditionalUpdate() bool { return false }

func (s *statusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newJ := obj.(*ebsv1.Job)
	oldJ := old.(*ebsv1.Job)
	newJ.Spec = oldJ.Spec
}

func (s *statusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateJobStatusUpdate(obj.(*ebsv1.Job), old.(*ebsv1.Job))
}

func (s *statusStrategy) Canonicalize(obj runtime.Object) {}
func (s *statusStrategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Job"}}, false, nil
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
