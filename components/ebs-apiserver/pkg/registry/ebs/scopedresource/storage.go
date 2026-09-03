package scopedresource

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/validation/path"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
)

type Config struct {
	Resource       string
	Singular       string
	Kind           string
	New            func() runtime.Object
	NewList        func() runtime.Object
	PrepareCreate  func(runtime.Object)
	CopyStatus     func(runtime.Object, runtime.Object)
	CopySpec       func(runtime.Object, runtime.Object)
	Validate       func(runtime.Object) field.ErrorList
	ValidateUpdate func(runtime.Object, runtime.Object) field.ErrorList
	ValidateStatus func(runtime.Object, runtime.Object) field.ErrorList
}

type Storage struct {
	Resource rest.StandardStorage
	Status   rest.StandardStorage
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
			KeyRootFunc:               keyRootFunc(c.Resource),
			KeyFunc:                   keyFunc(c.Resource),
		}
		if update == strategy {
			store.CreateStrategy = strategy
		}
		return store
	}
	return &Storage{Resource: newStore(strategy), Status: newStore(statusStrategy)}
}

func keyRootFunc(resource string) func(context.Context) string {
	return func(ctx context.Context) string {
		project, ok := genericapirequest.NamespaceFrom(ctx)
		if !ok || project == "" {
			return "/registry/ebs/" + resource
		}
		return "/registry/ebs/" + resource + "/" + project
	}
}

func keyFunc(resource string) func(context.Context, string) (string, error) {
	root := keyRootFunc(resource)
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

func (s *strategy) NamespaceScoped() bool          { return true }
func (s *strategy) AllowCreateOnUpdate() bool      { return false }
func (s *strategy) AllowUnconditionalUpdate() bool { return false }
func (s *strategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	s.config.PrepareCreate(obj)
}
func (s *strategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	s.config.CopyStatus(obj, old)
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

func (s *statusStrategy) NamespaceScoped() bool          { return true }
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
