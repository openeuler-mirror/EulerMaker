package validation

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateRpmRepo(obj *ebsv1.RpmRepo) field.ErrorList { return nil }

func ValidateRpmRepoUpdate(newObj, oldObj *ebsv1.RpmRepo) field.ErrorList {
	return ValidateRpmRepo(newObj)
}

func ValidateRpmRepoStatusUpdate(newObj, oldObj *ebsv1.RpmRepo) field.ErrorList {
	var allErrs field.ErrorList
	if repository := newObj.Status.Repository; repository != nil {
		path := field.NewPath("status", "repository")
		if (repository.RepositoryUID == "") != (repository.ContentURL == "") {
			allErrs = append(allErrs, field.Invalid(path, repository, "repositoryUID and contentURL must be provided together"))
		}
		if len(repository.SourceJobNames) > 0 && (repository.RepositoryUID == "" || repository.ContentURL == "") {
			allErrs = append(allErrs, field.Invalid(path.Child("sourceJobNames"), repository.SourceJobNames, "requires repositoryUID and contentURL"))
		}
		seen := make(map[string]bool, len(repository.SourceJobNames)+len(repository.SkippedJobNames))
		for _, name := range repository.SourceJobNames {
			seen[name] = true
		}
		for i, name := range repository.SkippedJobNames {
			itemPath := path.Child("skippedJobNames").Index(i)
			if name == "" {
				allErrs = append(allErrs, field.Required(itemPath, "Job name is required"))
			} else if seen[name] {
				allErrs = append(allErrs, field.Duplicate(itemPath, name))
			}
			seen[name] = true
		}
		if transition := repository.Transition; transition != nil {
			if transition.RepositoryUID == "" {
				allErrs = append(allErrs, field.Required(path.Child("transition", "repositoryUID"), "repository UID is required"))
			}
			if len(transition.Inputs) == 0 {
				allErrs = append(allErrs, field.Required(path.Child("transition", "inputs"), "at least one input is required"))
			}
		}
	}
	if release := newObj.Status.Release; release != nil {
		path := field.NewPath("status", "release")
		if !release.Phase.IsValid() {
			allErrs = append(allErrs, field.NotSupported(path.Child("phase"), release.Phase, ebsv1.RpmRepoReleasePhaseValues()))
		}
		if release.Transition != nil && release.Phase != ebsv1.RpmRepoReleasePending &&
			release.Phase != ebsv1.RpmRepoReleaseCreating && release.Phase != ebsv1.RpmRepoReleasePrepared {
			allErrs = append(allErrs, field.Forbidden(path.Child("transition"), "only allowed for Pending, Creating or Prepared releases"))
		}
		if release.Phase == ebsv1.RpmRepoReleaseReady && release.ContentURL == "" {
			allErrs = append(allErrs, field.Required(path.Child("contentURL"), "content URL is required for Ready releases"))
		}
	}
	for i, condition := range newObj.Status.Conditions {
		path := field.NewPath("status", "conditions").Index(i)
		if condition.Type != ebsv1.RpmRepoConditionRepositoryReady && condition.Type != ebsv1.RpmRepoConditionPublishSucceed {
			allErrs = append(allErrs, field.NotSupported(path.Child("type"), condition.Type, []string{ebsv1.RpmRepoConditionRepositoryReady, ebsv1.RpmRepoConditionPublishSucceed}))
		}
		if condition.Status != metav1.ConditionTrue && condition.Status != metav1.ConditionFalse {
			allErrs = append(allErrs, field.NotSupported(path.Child("status"), condition.Status, []string{string(metav1.ConditionTrue), string(metav1.ConditionFalse)}))
		}
		var reasons []string
		switch {
		case condition.Type == ebsv1.RpmRepoConditionRepositoryReady && condition.Status == metav1.ConditionTrue:
			reasons = []string{ebsv1.RpmRepoReasonRepositoryCreated}
		case condition.Type == ebsv1.RpmRepoConditionRepositoryReady && condition.Status == metav1.ConditionFalse:
			reasons = []string{ebsv1.RpmRepoReasonRepositoryCreationFailed}
		case condition.Type == ebsv1.RpmRepoConditionPublishSucceed && condition.Status == metav1.ConditionTrue:
			reasons = []string{ebsv1.RpmRepoReasonReleaseActivated}
		case condition.Type == ebsv1.RpmRepoConditionPublishSucceed && condition.Status == metav1.ConditionFalse:
			reasons = []string{ebsv1.RpmRepoReasonReleaseFailed, ebsv1.RpmRepoReasonRepositoryCreationFailed, ebsv1.RpmRepoReasonNoPublishableArtifacts, ebsv1.RpmRepoReasonBuildAborted, ebsv1.RpmRepoReasonBuildInfoFailed}
		}
		validReason := false
		for _, reason := range reasons {
			validReason = validReason || condition.Reason == reason
		}
		if len(reasons) > 0 && !validReason {
			allErrs = append(allErrs, field.NotSupported(path.Child("reason"), condition.Reason,
				reasons))
		}
	}
	return allErrs
}
