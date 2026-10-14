package machineaccount

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
)

type Storage struct{ MachineAccount rest.StandardStorage }

func NewStorage() *Storage {
	strategy := &strategy{}
	return &Storage{MachineAccount: &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &iamv1.MachineAccount{} },
		NewListFunc:               func() runtime.Object { return &iamv1.MachineAccountList{} },
		DefaultQualifiedResource:  iamv1.Resource("machineaccounts"),
		SingularQualifiedResource: iamv1.Resource("machineaccount"),
		CreateStrategy:            strategy, UpdateStrategy: strategy, DeleteStrategy: strategy,
		TableConvertor: rest.NewDefaultTableConvertor(iamv1.Resource("machineaccounts")),
	}}
}

type strategy struct{}

func (*strategy) NamespaceScoped() bool          { return false }
func (*strategy) AllowCreateOnUpdate() bool      { return false }
func (*strategy) AllowUnconditionalUpdate() bool { return false }
func (*strategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	account := obj.(*iamv1.MachineAccount)
	if account.Spec.TokenTTLSeconds == 0 {
		account.Spec.TokenTTLSeconds = 3600
	}
}
func (*strategy) PrepareForUpdate(context.Context, runtime.Object, runtime.Object) {}
func (*strategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	account := obj.(*iamv1.MachineAccount)
	var errs field.ErrorList
	if account.Name == "" {
		errs = append(errs, field.Required(field.NewPath("metadata", "name"), "name is required"))
	} else if messages := utilvalidation.IsDNS1123Label(account.Name); len(messages) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), account.Name, messages[0]))
	}
	if account.Spec.TokenTTLSeconds < 300 || account.Spec.TokenTTLSeconds > 86400 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "tokenTTLSeconds"), account.Spec.TokenTTLSeconds, "must be between 300 and 86400"))
	}
	return errs
}
func (s *strategy) ValidateUpdate(_ context.Context, obj, _ runtime.Object) field.ErrorList {
	return s.Validate(context.Background(), obj)
}
func (*strategy) Canonicalize(runtime.Object) {}
func (*strategy) ObjectKinds(runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: iamv1.GroupName, Version: "v1", Kind: "MachineAccount"}}, false, nil
}
func (*strategy) GenerateName(base string) string { return base }
func (*strategy) Recognizes(gvk schema.GroupVersionKind) bool {
	return gvk.Group == iamv1.GroupName && gvk.Version == "v1"
}
func (*strategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (*strategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
