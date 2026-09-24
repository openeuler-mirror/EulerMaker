package validation

import (
	_ "crypto/sha256"
	"strings"

	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateBuildConf(obj *ebsv1.BuildConf) field.ErrorList {
	var errs field.ErrorList
	if obj.Name != "default" {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), obj.Name, "must be default"))
	}
	if obj.Namespace != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "namespace"), "cluster-scoped resource"))
	}
	if obj.GenerateName != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "generateName"), "not supported"))
	}
	if obj.Spec.Targets == nil {
		errs = append(errs, field.Required(field.NewPath("spec", "targets"), "an object is required; use {} for no targets"))
	}
	for os, target := range obj.Spec.Targets {
		path := field.NewPath("spec", "targets").Key(os)
		if os == "" || len(validation.IsValidLabelValue(os)) > 0 {
			errs = append(errs, field.Invalid(path, os, "must be a nonempty label value"))
		}
		if len(target.Arches) == 0 {
			errs = append(errs, field.Required(path.Child("arches"), "at least one architecture is required"))
		}
		for arch, config := range target.Arches {
			p := path.Child("arches").Key(arch)
			if !architecturePattern.MatchString(arch) {
				errs = append(errs, field.Invalid(p, arch, "invalid architecture"))
			}
			_, err := reference.ParseNormalizedNamed(config.Image)
			if err != nil || strings.ContainsAny(config.Image, " \t\r\n") {
				errs = append(errs, field.Invalid(p.Child("image"), config.Image, "invalid container image reference"))
			}
		}
	}
	return errs
}
