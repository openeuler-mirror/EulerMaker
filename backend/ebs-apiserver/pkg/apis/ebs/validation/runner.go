package validation

import (
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func ValidateRunner(obj *ebsv1.Runner) field.ErrorList {
	var allErrs field.ErrorList
	if len(obj.Name) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("metadata", "name"), "name is required"))
	}
	if len(obj.Spec.Type) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "type"), "type is required"))
	} else if obj.Spec.Type != "ct" && obj.Spec.Type != "vm" && obj.Spec.Type != "hw" {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "type"), obj.Spec.Type, []string{"ct", "vm", "hw"}))
	}
	if len(obj.Spec.Arch) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "arch"), "arch is required"))
	}
	instanceIDPath := field.NewPath("spec", "instanceId")
	if obj.Spec.InstanceID == "" {
		allErrs = append(allErrs, field.Required(instanceIDPath, "instanceId is required"))
	} else if !uuidV4Pattern.MatchString(obj.Spec.InstanceID) {
		allErrs = append(allErrs, field.Invalid(instanceIDPath, obj.Spec.InstanceID, "must be a canonical lowercase UUID v4"))
	}
	labelsPath := field.NewPath("metadata", "labels")
	if obj.Labels["ebs.io/runner-type"] != obj.Spec.Type {
		allErrs = append(allErrs, field.Invalid(labelsPath.Key("ebs.io/runner-type"), obj.Labels["ebs.io/runner-type"], "must match spec.type"))
	}
	if obj.Labels["ebs.io/runner-arch"] != obj.Spec.Arch {
		allErrs = append(allErrs, field.Invalid(labelsPath.Key("ebs.io/runner-arch"), obj.Labels["ebs.io/runner-arch"], "must match spec.arch"))
	}
	return allErrs
}

func ValidateRunnerUpdate(newObj, oldObj *ebsv1.Runner) field.ErrorList {
	allErrs := ValidateRunner(newObj)
	if newObj.Spec.InstanceID != oldObj.Spec.InstanceID {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "instanceId"), newObj.Spec.InstanceID, "field is immutable"))
	}
	return allErrs
}

func ValidateRunnerDelete(obj *ebsv1.Runner) field.ErrorList {
	if obj.Status.Phase == ebsv1.RunnerOffline || obj.Status.Phase == ebsv1.RunnerEvicted {
		return nil
	}
	return field.ErrorList{field.Forbidden(field.NewPath("status", "phase"), "only Offline or Evicted runners can be deleted")}
}

func ValidateRunnerStatusUpdate(newObj, oldObj *ebsv1.Runner) field.ErrorList {
	var allErrs field.ErrorList
	phase := newObj.Status.Phase
	if phase == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("status", "phase"), "must be Online, Offline or Evicted"))
	} else if !phase.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "phase"), phase, ebsv1.RunnerPhaseValues()))
	}
	return allErrs
}
