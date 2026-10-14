package user

import (
	"context"
	"net/mail"
	"sort"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
)

type Storage struct{ User rest.StandardStorage }

func NewStorage() *Storage {
	strategy := &strategy{}
	return &Storage{User: &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &iamv1.User{} },
		NewListFunc:               func() runtime.Object { return &iamv1.UserList{} },
		DefaultQualifiedResource:  iamv1.Resource("users"),
		SingularQualifiedResource: iamv1.Resource("user"),
		CreateStrategy:            strategy, UpdateStrategy: strategy, DeleteStrategy: strategy,
		TableConvertor: rest.NewDefaultTableConvertor(iamv1.Resource("users")),
	}}
}

type strategy struct{}

func (*strategy) NamespaceScoped() bool          { return false }
func (*strategy) AllowCreateOnUpdate() bool      { return false }
func (*strategy) AllowUnconditionalUpdate() bool { return false }
func (*strategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	user := obj.(*iamv1.User)
	if user.Spec.Enabled == nil {
		enabled := true
		user.Spec.Enabled = &enabled
	}
	defaultAndSortScopes(user)
}
func (*strategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	defaultAndSortScopes(obj.(*iamv1.User))
}
func (*strategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateUser(obj.(*iamv1.User))
}
func (*strategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	return validateUser(obj.(*iamv1.User))
}
func (*strategy) Canonicalize(obj runtime.Object) { defaultAndSortScopes(obj.(*iamv1.User)) }
func (*strategy) ObjectKinds(runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	return []schema.GroupVersionKind{{Group: iamv1.GroupName, Version: "v1", Kind: "User"}}, false, nil
}
func (*strategy) GenerateName(base string) string { return base }
func (*strategy) Recognizes(gvk schema.GroupVersionKind) bool {
	return gvk.Group == iamv1.GroupName && gvk.Version == "v1"
}
func (*strategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (*strategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}

func validateUser(user *iamv1.User) field.ErrorList {
	var errs field.ErrorList
	namePath := field.NewPath("metadata", "name")
	if user.Name == "" {
		errs = append(errs, field.Required(namePath, "name is required"))
	} else if messages := utilvalidation.IsDNS1123Label(user.Name); len(messages) > 0 {
		errs = append(errs, field.Invalid(namePath, user.Name, messages[0]))
	}
	if user.Spec.Email != "" {
		if _, err := mail.ParseAddress(user.Spec.Email); err != nil {
			errs = append(errs, field.Invalid(field.NewPath("spec", "email"), user.Spec.Email, "must be a valid email address"))
		}
	}
	scopePath := field.NewPath("spec", "scopes")
	seen := make(map[string]struct{}, len(user.Spec.Scopes))
	for i, scope := range user.Spec.Scopes {
		if scope != "ebs:user" && scope != "ebs:ops" && scope != "ebs:admin" {
			errs = append(errs, field.NotSupported(scopePath.Index(i), scope, []string{"ebs:user", "ebs:ops", "ebs:admin"}))
		}
		if _, exists := seen[scope]; exists {
			errs = append(errs, field.Duplicate(scopePath.Index(i), scope))
		}
		seen[scope] = struct{}{}
	}
	if len(user.Spec.Scopes) == 0 {
		errs = append(errs, field.Required(scopePath, "at least one scope is required"))
	}
	if len(seen) > 1 {
		errs = append(errs, field.Invalid(scopePath, user.Spec.Scopes, "exactly one user scope is allowed"))
	}
	return errs
}

func defaultAndSortScopes(user *iamv1.User) {
	if len(user.Spec.Scopes) == 0 {
		user.Spec.Scopes = []string{"ebs:user"}
		return
	}
	sort.Strings(user.Spec.Scopes)
}
