package validation

import (
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateBuild(obj *ebsv1.Build) field.ErrorList {
	var allErrs field.ErrorList
	namePath := field.NewPath("metadata", "name")
	if obj.Name == "" {
		allErrs = append(allErrs, field.Required(namePath, "Build name is required"))
	} else if !uuidPattern.MatchString(obj.Name) {
		allErrs = append(allErrs, field.Invalid(namePath, obj.Name, "must be a canonical lowercase UUID"))
	}
	if len(obj.Spec.BuildType) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildType"), "buildType is required"))
	}
	if len(obj.Spec.Packages) == 0 && obj.Spec.BuildType != "full" && obj.Spec.BuildType != "incremental" {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "packages"), "at least one package is required"))
	}
	if len(obj.Spec.BuildTarget.Os) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTarget", "os"), "os is required"))
	}
	if len(obj.Spec.BuildTarget.Arch) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTarget", "arch"), "arch is required"))
	}
	labelsPath := field.NewPath("metadata", "labels")
	if value, ok := obj.Labels[ebsv1.BuildTargetOSLabel]; !ok || len(value) == 0 {
		allErrs = append(allErrs, field.Required(labelsPath.Key(ebsv1.BuildTargetOSLabel), "target OS label is required"))
	} else if value != obj.Spec.BuildTarget.Os {
		allErrs = append(allErrs, field.Invalid(labelsPath.Key(ebsv1.BuildTargetOSLabel), value, "must match spec.buildTarget.os"))
	}
	if value, ok := obj.Labels[ebsv1.BuildTargetArchLabel]; !ok || len(value) == 0 {
		allErrs = append(allErrs, field.Required(labelsPath.Key(ebsv1.BuildTargetArchLabel), "target architecture label is required"))
	} else if value != obj.Spec.BuildTarget.Arch {
		allErrs = append(allErrs, field.Invalid(labelsPath.Key(ebsv1.BuildTargetArchLabel), value, "must match spec.buildTarget.arch"))
	}
	if value, ok := obj.Labels[ebsv1.BuildTypeLabel]; !ok || len(value) == 0 {
		allErrs = append(allErrs, field.Required(labelsPath.Key(ebsv1.BuildTypeLabel), "build type label is required"))
	} else if value != obj.Spec.BuildType {
		allErrs = append(allErrs, field.Invalid(labelsPath.Key(ebsv1.BuildTypeLabel), value, "must match spec.buildType"))
	}
	return allErrs
}

func ValidateBuildUpdate(newObj, oldObj *ebsv1.Build) field.ErrorList {
	allErrs := ValidateBuild(newObj)
	if !apiequality.Semantic.DeepEqual(newObj.Spec, oldObj.Spec) {
		previous, next := oldObj.Spec, newObj.Spec
		previous.Packages, next.Packages = nil, nil
		if oldObj.Spec.BuildType != "incremental" || oldObj.Status.Phase != ebsv1.BuildPending || !apiequality.Semantic.DeepEqual(previous, next) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec"), newObj.Spec, "field is immutable except incremental packages while Pending"))
		}
	}
	return allErrs
}

func ValidateBuildStatusUpdate(newObj, oldObj *ebsv1.Build) field.ErrorList {
	var allErrs field.ErrorList
	if oldObj.Status.Phase.IsTerminal() && newObj.Status.Phase != oldObj.Status.Phase {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("status", "phase"), "terminal Build phase is immutable"))
	}
	if !newObj.Status.Phase.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "phase"), newObj.Status.Phase, ebsv1.BuildPhaseValues()))
	}
	return allErrs
}
