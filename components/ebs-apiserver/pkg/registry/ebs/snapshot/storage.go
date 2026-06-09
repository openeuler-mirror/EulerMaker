package snapshot

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

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
)

type Storage struct {
	Snapshot rest.StandardStorage
	Status   rest.StandardStorage
}

func NewStorage(scheme *runtime.Scheme) *Storage {
	strategy := &strategy{}
	statusStrategy := &statusStrategy{}

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Snapshot{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.SnapshotList{} },
		DefaultQualifiedResource:  ebsv1.Resource("snapshots"),
		SingularQualifiedResource: ebsv1.Resource("snapshot"),
		CreateStrategy:            strategy,
		UpdateStrategy:            strategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("snapshots")),
		KeyRootFunc:               keyRootFunc,
		KeyFunc:                   keyFunc,
	}

	statusStore := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Snapshot{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.SnapshotList{} },
		DefaultQualifiedResource:  ebsv1.Resource("snapshots"),
		SingularQualifiedResource: ebsv1.Resource("snapshot"),
		UpdateStrategy:            statusStrategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("snapshots")),
		KeyRootFunc:               keyRootFunc,
		KeyFunc:                   keyFunc,
	}

	return &Storage{
		Snapshot: store,
		Status:   statusStore,
	}
}

func keyRootFunc(ctx context.Context) string {
	project, ok := genericapirequest.NamespaceFrom(ctx)
	if !ok || project == "" {
		return "/registry/ebs/snapshots"
	}
	return "/registry/ebs/snapshots/" + project
}

func keyFunc(ctx context.Context, name string) (string, error) {
	if len(name) == 0 {
		return "", fmt.Errorf("name parameter required")
	}
	if msgs := path.IsValidPathSegmentName(name); len(msgs) != 0 {
		return "", fmt.Errorf("name parameter invalid: %q: %s", name, strings.Join(msgs, ";"))
	}
	return keyRootFunc(ctx) + "/" + name, nil
}

type strategy struct{}

func (s *strategy) NamespaceScoped() bool          { return true }
func (s *strategy) AllowCreateOnUpdate() bool      { return false }
func (s *strategy) AllowUnconditionalUpdate() bool { return false }

func (s *strategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	snap := obj.(*ebsv1.Snapshot)
	snap.Status = ebsv1.SnapshotStatus{Phase: "Created"}
}

func (s *strategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newSnap := obj.(*ebsv1.Snapshot)
	oldSnap := old.(*ebsv1.Snapshot)
	newSnap.Status = oldSnap.Status
}

func (s *strategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return validation.ValidateSnapshot(obj.(*ebsv1.Snapshot))
}

func (s *strategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateSnapshotUpdate(obj.(*ebsv1.Snapshot), old.(*ebsv1.Snapshot))
}

func (s *strategy) Canonicalize(obj runtime.Object) {}
func (s *strategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Snapshot"}}, false, nil
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
	newSnap := obj.(*ebsv1.Snapshot)
	oldSnap := old.(*ebsv1.Snapshot)
	newSnap.Spec = oldSnap.Spec
}

func (s *statusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return field.ErrorList{}
}

func (s *statusStrategy) Canonicalize(obj runtime.Object) {}
func (s *statusStrategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Snapshot"}}, false, nil
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
