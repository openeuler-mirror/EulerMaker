package validation

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateBuildInfo(obj *ebsv1.BuildInfo) field.ErrorList { return nil }

func ValidateBuildInfoUpdate(newObj, oldObj *ebsv1.BuildInfo) field.ErrorList {
	return ValidateBuildInfo(newObj)
}

func ValidateBuildInfoStatusUpdate(newObj, oldObj *ebsv1.BuildInfo) field.ErrorList {
	var allErrs field.ErrorList
	phase := newObj.Status.Phase
	if !phase.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "phase"), phase, ebsv1.BuildInfoPhaseValues()))
	}
	if oldObj.Status.Phase.IsTerminal() && phase != oldObj.Status.Phase {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("status", "phase"), "terminal BuildInfo phase is immutable"))
	}
	if phase == ebsv1.BuildInfoFailed {
		valid := false
		for _, condition := range newObj.Status.Conditions {
			if condition.Type == "BuildPayloadInvalid" && condition.Status == metav1.ConditionTrue && condition.Reason == "BuildPayloadParseFailed" {
				valid = true
				break
			}
		}
		if !valid {
			allErrs = append(allErrs, field.Required(field.NewPath("status", "conditions"), "Failed requires BuildPayloadInvalid=True/BuildPayloadParseFailed"))
		}
	}
	return allErrs
}
