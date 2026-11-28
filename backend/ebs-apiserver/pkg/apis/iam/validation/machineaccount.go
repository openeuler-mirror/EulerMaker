package validation

import (
	iamv1 "ebs-api/iam/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func ValidateMachineAccount(account *iamv1.MachineAccount) field.ErrorList {
	var errs field.ErrorList
	if !account.Spec.Type.IsValid() {
		errs = append(errs, field.NotSupported(field.NewPath("spec", "type"), account.Spec.Type, []string{"runner", "service"}))
	}
	if account.Name == "" {
		errs = append(errs, field.Required(field.NewPath("metadata", "name"), "name is required"))
	} else if messages := utilvalidation.IsDNS1123Label(account.Name); len(messages) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), account.Name, messages[0]))
	}
	if account.Spec.TokenTTLSeconds < 300 || account.Spec.TokenTTLSeconds > 86400 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "tokenTTLSeconds"), account.Spec.TokenTTLSeconds, "must be between 300 and 86400"))
	}
	projects := field.NewPath("spec", "projects")
	permissions := field.NewPath("spec", "permissions")
	if account.Spec.Type != iamv1.MachineAccountService {
		if len(account.Spec.Projects) > 0 {
			errs = append(errs, field.Forbidden(projects, "only service accounts can specify projects"))
		}
		if len(account.Spec.Permissions) > 0 {
			errs = append(errs, field.Forbidden(permissions, "only service accounts can specify permissions"))
		}
	}
	seenProjects := make(map[string]bool, len(account.Spec.Projects))
	for i, name := range account.Spec.Projects {
		if messages := utilvalidation.IsDNS1123Label(name); name != iamv1.MachineAccountAllProjects && len(messages) > 0 {
			errs = append(errs, field.Invalid(projects.Index(i), name, messages[0]))
		}
		if seenProjects[name] {
			errs = append(errs, field.Duplicate(projects.Index(i), name))
		}
		seenProjects[name] = true
	}
	if seenProjects[iamv1.MachineAccountAllProjects] && len(seenProjects) > 1 {
		errs = append(errs, field.Invalid(projects, account.Spec.Projects, "* must be used alone"))
	}
	seenPermissions := make(map[iamv1.MachineAccountPermission]bool, len(account.Spec.Permissions))
	for i, permission := range account.Spec.Permissions {
		if !permission.IsValid() {
			errs = append(errs, field.NotSupported(permissions.Index(i), permission, []string{
				string(iamv1.MachinePermissionProjectsUpdate), string(iamv1.MachinePermissionBuildsCreate), string(iamv1.MachinePermissionBuildsAbort),
			}))
		}
		if seenPermissions[permission] {
			errs = append(errs, field.Duplicate(permissions.Index(i), permission))
		}
		seenPermissions[permission] = true
	}
	return errs
}
