package scopedresource

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/validation/path"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"

	ebsv1 "ebs-api/ebs/v1"
)

type Config struct {
	Resource      string
	Singular      string
	Kind          string
	ClusterScoped bool
	New           func() runtime.Object
	NewList       func() runtime.Object
	PrepareCreate func(runtime.Object)
	CopyStatus    func(runtime.Object, runtime.Object)
	// AttrFunc supplies the same attributes to List/Watch filtering and the native storage cache.
	AttrFunc storage.AttrFunc

	// CopySpec preserves fields owned by the main resource during status updates.
	// A nil callback means the resource has no status subresource.
	CopySpec       func(runtime.Object, runtime.Object)
	Validate       func(runtime.Object) field.ErrorList
	ValidateUpdate func(runtime.Object, runtime.Object) field.ErrorList
	ValidateStatus func(runtime.Object, runtime.Object) field.ErrorList

	// ValidateDelete is applied by native storage during deletion.
	ValidateDelete func(runtime.Object) field.ErrorList
}

type Storage struct {
	Resource       rest.StandardStorage
	Status         rest.Storage
	validateDelete func(runtime.Object) field.ErrorList
	attrFunc       storage.AttrFunc
}

func NewStorage(c Config) *Storage {
	strategy := &strategy{config: c}
	statusStrategy := &statusStrategy{config: c}
	newStore := func(update rest.RESTUpdateStrategy) *genericregistry.Store {
		store := &genericregistry.Store{
			NewFunc:                   c.New,
			NewListFunc:               c.NewList,
			DefaultQualifiedResource:  ebsv1.Resource(c.Resource),
			SingularQualifiedResource: ebsv1.Resource(c.Singular),
			UpdateStrategy:            update,
			DeleteStrategy:            strategy,
			TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource(c.Resource)),
			KeyRootFunc:               keyRootFunc(c.Resource, c.ClusterScoped),
			KeyFunc:                   keyFunc(c.Resource, c.ClusterScoped),
		}
		if update == strategy {
			store.CreateStrategy = strategy
		}
		if c.AttrFunc != nil {
			store.PredicateFunc = func(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
				return storage.SelectionPredicate{Label: label, Field: field, GetAttrs: c.AttrFunc}
			}
		}
		return store
	}
	storage := &Storage{Resource: newStore(strategy), validateDelete: c.ValidateDelete, attrFunc: c.AttrFunc}
	if c.CopySpec != nil {
		storage.Status = newStore(statusStrategy)
	}
	return storage
}

// CompleteWithOptions initializes native storage and shares it with the status subresource.
func (s *Storage) CompleteWithOptions(options *generic.StoreOptions) error {
	store := s.Resource.(*genericregistry.Store)
	// Native storage keys use the configured resource prefix, not the ES adapter's keys.
	store.KeyRootFunc, store.KeyFunc = nil, nil
	storeOptions := *options
	if s.attrFunc != nil {
		storeOptions.AttrFunc = s.attrFunc
	}
	if err := store.CompleteWithOptions(&storeOptions); err != nil {
		return err
	}
	if s.Status != nil {
		status := *store
		status.UpdateStrategy = s.Status.(*genericregistry.Store).UpdateStrategy
		status.CreateStrategy, status.DeleteStrategy = nil, nil
		status.DestroyFunc = nil
		s.Status = &statusREST{store: &status}
	}
	if s.validateDelete != nil {
		s.Resource = &resourceREST{Store: store, validateDelete: s.validateDelete}
	}
	return nil
}

type resourceREST struct {
	*genericregistry.Store
	validateDelete func(runtime.Object) field.ErrorList
}

func (r *resourceREST) Delete(ctx context.Context, name string, validate rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	return r.Store.Delete(ctx, name, r.deletionValidation(validate), options)
}

func (r *resourceREST) DeleteCollection(ctx context.Context, validate rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
	return r.Store.DeleteCollection(ctx, r.deletionValidation(validate), options, listOptions)
}

func (r *resourceREST) deletionValidation(validate rest.ValidateObjectFunc) rest.ValidateObjectFunc {
	return func(ctx context.Context, obj runtime.Object) error {
		if errs := r.validateDelete(obj); len(errs) != 0 {
			accessor, err := meta.Accessor(obj)
			if err != nil {
				return apierrors.NewInternalError(err)
			}
			return apierrors.NewConflict(r.DefaultQualifiedResource, accessor.GetName(), errs.ToAggregate())
		}
		if validate != nil {
			return validate(ctx, obj)
		}
		return nil
	}
}

type statusREST struct {
	store *genericregistry.Store
}

func (r *statusREST) New() runtime.Object { return r.store.New() }
func (r *statusREST) Destroy()            {}
func (r *statusREST) NamespaceScoped() bool {
	return r.store.UpdateStrategy.NamespaceScoped()
}
func (r *statusREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return r.store.Get(ctx, name, options)
}
func (r *statusREST) Update(ctx context.Context, name string, info rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return r.store.Update(ctx, name, info, createValidation, updateValidation, forceAllowCreate, options)
}

func keyRootFunc(resource string, clusterScoped bool) func(context.Context) string {
	return func(ctx context.Context) string {
		project, ok := genericapirequest.NamespaceFrom(ctx)
		if clusterScoped || !ok || project == "" {
			return "/registry/ebs/" + resource
		}
		return "/registry/ebs/" + resource + "/" + project
	}
}

func keyFunc(resource string, clusterScoped bool) func(context.Context, string) (string, error) {
	root := keyRootFunc(resource, clusterScoped)
	return func(ctx context.Context, name string) (string, error) {
		if name == "" {
			return "", fmt.Errorf("name parameter required")
		}
		if msgs := path.IsValidPathSegmentName(name); len(msgs) != 0 {
			return "", fmt.Errorf("name parameter invalid: %q: %s", name, strings.Join(msgs, ";"))
		}
		return root(ctx) + "/" + name, nil
	}
}

type strategy struct{ config Config }

func (s *strategy) NamespaceScoped() bool          { return !s.config.ClusterScoped }
func (s *strategy) AllowCreateOnUpdate() bool      { return false }
func (s *strategy) AllowUnconditionalUpdate() bool { return false }
func (s *strategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	if s.config.PrepareCreate != nil {
		s.config.PrepareCreate(obj)
	}
}
func (s *strategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	if s.config.CopyStatus != nil {
		s.config.CopyStatus(obj, old)
	}
}
func (s *strategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return s.config.Validate(obj)
}
func (s *strategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	return s.config.ValidateUpdate(obj, old)
}
func (s *strategy) Canonicalize(runtime.Object) {}
func (s *strategy) ObjectKinds(runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: s.config.Kind}}, false, nil
}
func (s *strategy) GenerateName(base string) string { return base }
func (s *strategy) Recognizes(gvk schema.GroupVersionKind) bool {
	return gvk.Group == "ebs" && gvk.Version == "v1"
}
func (s *strategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (s *strategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}

type statusStrategy struct{ config Config }

func (s *statusStrategy) NamespaceScoped() bool          { return !s.config.ClusterScoped }
func (s *statusStrategy) AllowCreateOnUpdate() bool      { return false }
func (s *statusStrategy) AllowUnconditionalUpdate() bool { return false }
func (s *statusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	s.config.CopySpec(obj, old)
}
func (s *statusStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	return s.config.ValidateStatus(obj, old)
}
func (s *statusStrategy) Canonicalize(runtime.Object) {}
func (s *statusStrategy) ObjectKinds(runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: s.config.Kind}}, false, nil
}
func (s *statusStrategy) GenerateName(base string) string { return base }
func (s *statusStrategy) Recognizes(gvk schema.GroupVersionKind) bool {
	return gvk.Group == "ebs" && gvk.Version == "v1"
}
func (s *statusStrategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (s *statusStrategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
