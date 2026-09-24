package validation

import (
	"strings"
	"unicode/utf8"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateConfig(obj *ebsv1.Config) field.ErrorList {
	var errs field.ErrorList
	name := field.NewPath("metadata", "name")
	if obj.Name == "" {
		errs = append(errs, field.Required(name, "name is required"))
	} else if messages := apivalidation.NameIsDNSSubdomain(obj.Name, false); len(messages) > 0 {
		errs = append(errs, field.Invalid(name, obj.Name, strings.Join(messages, "; ")))
	}
	if obj.Namespace != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "namespace"), "cluster-scoped resource"))
	}
	if obj.GenerateName != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "generateName"), "not supported"))
	}
	visibility := field.NewPath("spec", "visibility")
	if obj.Spec.Visibility != ebsv1.ConfigVisibilityPublic && obj.Spec.Visibility != ebsv1.ConfigVisibilityOpsOnly {
		errs = append(errs, field.NotSupported(visibility, obj.Spec.Visibility, []string{string(ebsv1.ConfigVisibilityPublic), string(ebsv1.ConfigVisibilityOpsOnly)}))
	}
	content := field.NewPath("spec", "content")
	if obj.Spec.Content == "" {
		errs = append(errs, field.Required(content, "content is required"))
	} else if !utf8.ValidString(obj.Spec.Content) || strings.ContainsRune(obj.Spec.Content, 0) {
		errs = append(errs, field.Invalid(content, "<redacted>", "must be UTF-8 text without NUL"))
	}
	return errs
}
