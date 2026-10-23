package validation

import (
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateJob(obj *ebsv1.Job) field.ErrorList {
	var allErrs field.ErrorList
	if executionType := obj.Spec.Runtime; executionType != "" && !executionType.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "runtime"), executionType, ebsv1.ExecutionTypeValues()))
	}
	for i, toleration := range obj.Spec.Tolerations {
		path := field.NewPath("spec", "tolerations").Index(i)
		if toleration.Operator != "" && !toleration.Operator.IsValid() {
			allErrs = append(allErrs, field.NotSupported(path.Child("operator"), toleration.Operator,
				ebsv1.TolerationOperatorValues()))
		}
		if toleration.Effect != "" && !toleration.Effect.IsValid() {
			allErrs = append(allErrs, field.NotSupported(path.Child("effect"), toleration.Effect, ebsv1.TaintEffectValues()))
		}
	}
	if instance := obj.Annotations[ebsv1.ProjectArtifactsAnnotation]; instance != "" {
		for _, reason := range validation.IsDNS1123Label(instance) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("metadata", "annotations").Key(ebsv1.ProjectArtifactsAnnotation), instance, reason))
		}
	}
	path := field.NewPath("spec", "scriptRefs")
	seenNames := make(map[string]struct{}, len(obj.Spec.ScriptRefs))
	for i, ref := range obj.Spec.ScriptRefs {
		refPath := path.Index(i)
		if ref.Name == "" {
			allErrs = append(allErrs, field.Required(refPath.Child("name"), "Script name is required"))
		} else {
			if _, exists := seenNames[ref.Name]; exists {
				allErrs = append(allErrs, field.Duplicate(refPath.Child("name"), ref.Name))
			}
			seenNames[ref.Name] = struct{}{}
			for _, reason := range validation.IsDNS1123Subdomain(ref.Name) {
				allErrs = append(allErrs, field.Invalid(refPath.Child("name"), ref.Name, reason))
			}
		}
		if ref.UID == "" {
			allErrs = append(allErrs, field.Required(refPath.Child("uid"), "Script UID is required"))
		} else if !uuidPattern.MatchString(ref.UID) {
			allErrs = append(allErrs, field.Invalid(refPath.Child("uid"), ref.UID, "must be a UUID"))
		}
		if ref.ResourceVersion == "" {
			allErrs = append(allErrs, field.Required(refPath.Child("resourceVersion"), "Script resourceVersion is required"))
		}
	}
	return allErrs
}

func ValidateJobUpdate(newObj, oldObj *ebsv1.Job) field.ErrorList {
	allErrs := ValidateJob(newObj)
	if newObj.Annotations[ebsv1.ProjectArtifactsAnnotation] != oldObj.Annotations[ebsv1.ProjectArtifactsAnnotation] {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("metadata", "annotations").Key(ebsv1.ProjectArtifactsAnnotation), "artifact instance is immutable"))
	}
	if !apiequality.Semantic.DeepEqual(newObj.Spec.ScriptRefs, oldObj.Spec.ScriptRefs) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "scriptRefs"), "Script observations are immutable"))
	}
	return allErrs
}

func ValidateJobStatusUpdate(newObj, oldObj *ebsv1.Job) field.ErrorList {
	var allErrs field.ErrorList
	switch oldObj.Status.Phase {
	case ebsv1.JobSucceeded, ebsv1.JobFailed, ebsv1.JobAborted:
		if !apiequality.Semantic.DeepEqual(newObj.Status, oldObj.Status) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("status"), "terminal Job status is immutable"))
		}
	}
	if !newObj.Status.Phase.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "phase"), newObj.Status.Phase, ebsv1.JobPhaseValues()))
	}
	if !newObj.Status.Stage.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "stage"), newObj.Status.Stage, ebsv1.JobStageValues()))
	}
	if result := newObj.Status.Build; result != nil && !result.Status.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "build", "status"), result.Status, []string{string(ebsv1.JobResultSucceeded), string(ebsv1.JobResultFailed)}))
	}
	if result := newObj.Status.Install; result != nil {
		if newObj.Status.Build == nil || newObj.Status.Build.Status != ebsv1.JobResultSucceeded {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("status", "install"), "install result requires a successful build result"))
		}
		if !result.Status.IsValid() {
			allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "install", "status"), result.Status, []string{string(ebsv1.JobResultSucceeded), string(ebsv1.JobResultFailed)}))
		}
		if result.Status == ebsv1.JobResultSucceeded && len(result.MissingDeps) > 0 {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("status", "install", "missingDeps"), "successful install check cannot have missing dependencies"))
		}
	}
	return allErrs
}
