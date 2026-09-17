package buildconf

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
)

type Storage struct {
	BuildConf rest.StandardStorage
}

func NewStorage(scheme *runtime.Scheme) *Storage {
	strategy := &strategy{}

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.BuildConf{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.BuildConfList{} },
		DefaultQualifiedResource:  ebsv1.Resource("buildconfs"),
		SingularQualifiedResource: ebsv1.Resource("buildconf"),
		CreateStrategy:            strategy,
		UpdateStrategy:            strategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("buildconfs")),
	}

	return &Storage{
		BuildConf: store,
	}
}

type strategy struct{}

func (s *strategy) NamespaceScoped() bool          { return false }
func (s *strategy) AllowCreateOnUpdate() bool      { return false }
func (s *strategy) AllowUnconditionalUpdate() bool { return false }

func (s *strategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	p := obj.(*ebsv1.BuildConf)
	p.Generation = 1
}

func (s *strategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	// The ES store owns generation; preserve service metadata through BeforeUpdate.
}

func (s *strategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return validation.ValidateBuildConf(obj.(*ebsv1.BuildConf))
}

func (s *strategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateBuildConf(obj.(*ebsv1.BuildConf))
}

func (s *strategy) Canonicalize(obj runtime.Object) {}
func (s *strategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "BuildConf"}}, false, nil
}
func (s *strategy) GenerateName(base string) string { return base }
func (s *strategy) Recognizes(gvk schema.GroupVersionKind) bool {
	return gvk.Group == "ebs" && gvk.Version == "v1"
}
func (s *strategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string { return nil }
func (s *strategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}
