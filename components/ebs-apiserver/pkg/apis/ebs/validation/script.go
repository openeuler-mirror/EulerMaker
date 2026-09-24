package validation

import (
	"strings"
	"unicode/utf8"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateScript(obj *ebsv1.Script) field.ErrorList {
	var errs field.ErrorList
	if obj.Name == "" {
		errs = append(errs, field.Required(field.NewPath("metadata", "name"), "name is required"))
	} else if messages := apivalidation.NameIsDNSSubdomain(obj.Name, false); len(messages) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), obj.Name, strings.Join(messages, "; ")))
	}
	if obj.Namespace != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "namespace"), "cluster-scoped resource"))
	}
	if obj.GenerateName != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "generateName"), "not supported"))
	}
	content := obj.Spec.Content
	p := field.NewPath("spec", "content")
	switch {
	case content == "":
		errs = append(errs, field.Required(p, "script content is required"))
	case !utf8.ValidString(content) || strings.ContainsRune(content, 0):
		errs = append(errs, field.Invalid(p, "<redacted>", "must be UTF-8 text without NUL"))
	default:
		line, _, _ := strings.Cut(content, "\n")
		interpreter := strings.Fields(strings.TrimPrefix(line, "#!"))
		if !strings.HasPrefix(line, "#!") || len(interpreter) == 0 || !strings.HasPrefix(interpreter[0], "/") || interpreter[0] == "/" || strings.ContainsRune(line, '\r') {
			errs = append(errs, field.Invalid(p, "<redacted>", "first line must be a shebang with an absolute interpreter path and LF line ending"))
		}
	}
	return errs
}
