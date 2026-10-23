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
	statusPath := field.NewPath("status", "specStatus")
	for name, build := range newObj.Status.SpecStatus.Build {
		if build.Status != "" && !build.Status.IsValid() {
			allErrs = append(allErrs, field.NotSupported(statusPath.Child("build").Key(name).Child("status"),
				build.Status, ebsv1.SpecBuildPhaseValues()))
		}
	}
	for name, install := range newObj.Status.SpecStatus.Install {
		if install.Status != "" && !install.Status.IsValid() {
			allErrs = append(allErrs, field.NotSupported(statusPath.Child("install").Key(name).Child("status"),
				install.Status, ebsv1.SpecInstallPhaseValues()))
		}
	}
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
