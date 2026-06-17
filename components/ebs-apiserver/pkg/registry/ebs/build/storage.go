package build

import (
	"context"
	"fmt"
	"net/http"
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
	Build  rest.StandardStorage
	Status rest.StandardStorage
	Abort  rest.Storage
}

type abort struct{}

func (a *abort) NamespaceScoped() bool { return true }
func (a *abort) New() runtime.Object   { return &ebsv1.Build{} }
func (a *abort) Connect(ctx context.Context, id string, options runtime.Object, r rest.Responder) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "aborted"}`))
	}), nil
}
func (a *abort) NewConnectOptions() (runtime.Object, bool, string) { return nil, false, "" }
func (a *abort) Destroy()                                          {}

func NewStorage(scheme *runtime.Scheme) *Storage {
	strategy := &strategy{}
	statusStrategy := &statusStrategy{}

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Build{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.BuildList{} },
		DefaultQualifiedResource:  ebsv1.Resource("builds"),
		SingularQualifiedResource: ebsv1.Resource("build"),
		CreateStrategy:            strategy,
		UpdateStrategy:            strategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("builds")),

		KeyRootFunc: keyRootFunc,
		KeyFunc:     keyFunc,
	}

	statusStore := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &ebsv1.Build{} },
		NewListFunc:               func() runtime.Object { return &ebsv1.BuildList{} },
		DefaultQualifiedResource:  ebsv1.Resource("builds"),
		SingularQualifiedResource: ebsv1.Resource("build"),
		UpdateStrategy:            statusStrategy,
		DeleteStrategy:            strategy,
		TableConvertor:            rest.NewDefaultTableConvertor(ebsv1.Resource("builds")),

		KeyRootFunc: keyRootFunc,
		KeyFunc:     keyFunc,
	}

	return &Storage{
		Build:  store,
		Status: statusStore,
		Abort:  &abort{},
	}
}

func keyRootFunc(ctx context.Context) string {
	ns, ok := genericapirequest.NamespaceFrom(ctx)
	if !ok || ns == "" {
		return "/registry/ebs/builds"
	}
	return "/registry/ebs/builds/" + ns
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
	b := obj.(*ebsv1.Build)
	b.Status = ebsv1.BuildStatus{Phase: "Pending"}
}

func (s *strategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newB := obj.(*ebsv1.Build)
	oldB := old.(*ebsv1.Build)
	newB.Status = oldB.Status
}

func (s *strategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return validation.ValidateBuild(obj.(*ebsv1.Build))
}

func (s *strategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateBuildUpdate(obj.(*ebsv1.Build), old.(*ebsv1.Build))
}

func (s *strategy) Canonicalize(obj runtime.Object) {}
func (s *strategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Build"}}, false, nil
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
	newB := obj.(*ebsv1.Build)
	oldB := old.(*ebsv1.Build)
	newB.Spec = oldB.Spec
}

func (s *statusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return validation.ValidateBuildStatusUpdate(obj.(*ebsv1.Build), old.(*ebsv1.Build))
}

func (s *statusStrategy) Canonicalize(obj runtime.Object) {}
func (s *statusStrategy) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: "ebs", Version: "v1", Kind: "Build"}}, false, nil
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
