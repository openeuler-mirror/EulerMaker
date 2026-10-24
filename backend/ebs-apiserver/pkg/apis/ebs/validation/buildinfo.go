package validation

import (
	"strings"

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
	statusPath := field.NewPath("status", "specResults")
	names := map[string]string{}
	for key, result := range newObj.Status.SpecResults {
		p := statusPath.Key(key)
		repo, file, found := strings.Cut(key, "/")
		if !found || repo == "" || repo == "." || repo == ".." || strings.ContainsAny(repo, "\\") ||
			strings.ContainsAny(file, "/\\") ||
			(file != "" && !strings.HasSuffix(file, ".spec")) {
			allErrs = append(allErrs, field.Invalid(p, key, "expected repository/root-spec-filename"))
		}
		if file == "" && result.Status != ebsv1.SpecPrepareFailed {
			allErrs = append(allErrs, field.Invalid(p, key, "repository-only entries must be PrepareFailed"))
		}
		if !result.Status.IsValid() {
			allErrs = append(allErrs, field.NotSupported(p.Child("status"), result.Status, ebsv1.SpecResultPhaseValues()))
		}
		if result.Status != ebsv1.SpecPrepareFailed && result.Status != ebsv1.SpecPending && result.SpecName == "" {
			allErrs = append(allErrs, field.Required(p.Child("specName"), "required after successful parsing"))
		}
		if result.SpecName != "" && result.Status != ebsv1.SpecPrepareFailed {
			if previous, exists := names[result.SpecName]; exists {
				allErrs = append(allErrs, field.Invalid(p.Child("specName"), result.SpecName, "already used by "+previous))
			}
			names[result.SpecName] = key
		}
		if result.DispatchCount < 0 {
			allErrs = append(allErrs, field.Invalid(p.Child("dispatchCount"), result.DispatchCount, "must not be negative"))
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
