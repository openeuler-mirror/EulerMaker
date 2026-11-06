package validation

import (
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateRpmRepo(obj *ebsv1.RpmRepo) field.ErrorList { return nil }

func ValidateRpmRepoUpdate(newObj, oldObj *ebsv1.RpmRepo) field.ErrorList {
	allErrs := ValidateRpmRepo(newObj)
	if slices.Contains(oldObj.Finalizers, ebsv1.RpmRepoCleanupFinalizer) &&
		!slices.Contains(newObj.Finalizers, ebsv1.RpmRepoCleanupFinalizer) {
		if oldObj.DeletionTimestamp == nil ||
			!meta.IsStatusConditionTrue(oldObj.Status.Conditions, ebsv1.RpmRepoConditionCleanupReady) ||
			(oldObj.Status.Repository != nil && oldObj.Status.Repository.Transition != nil) ||
			(oldObj.Status.Release != nil && oldObj.Status.Release.Transition != nil) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("metadata", "finalizers"),
				"rpmrepo cleanup finalizer requires deletion, CleanupReady=True and drained transitions"))
		}
	}
	return allErrs
}

func ValidateRpmRepoStatusUpdate(newObj, oldObj *ebsv1.RpmRepo) field.ErrorList {
	var allErrs field.ErrorList
	cleanupReady := meta.IsStatusConditionTrue(newObj.Status.Conditions, ebsv1.RpmRepoConditionCleanupReady)
	if meta.IsStatusConditionTrue(oldObj.Status.Conditions, ebsv1.RpmRepoConditionCleanupReady) && !cleanupReady {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("status", "conditions"), "CleanupReady=True cannot be cleared or changed"))
	}
	if cleanupReady && ((newObj.Status.Repository != nil && newObj.Status.Repository.Transition != nil) ||
		(newObj.Status.Release != nil && newObj.Status.Release.Transition != nil)) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("status", "conditions"), "CleanupReady=True requires all transitions to be drained"))
	}
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
			release.Phase != ebsv1.RpmRepoReleaseCreating &&
			release.Phase != ebsv1.RpmRepoReleaseFailed {
			allErrs = append(allErrs, field.Forbidden(path.Child("transition"), "only allowed for Pending, Creating or Failed releases"))
		}
		if release.Phase == ebsv1.RpmRepoReleaseReady && release.ContentURL == "" {
			allErrs = append(allErrs, field.Required(path.Child("contentURL"), "content URL is required for Ready releases"))
		}
	}
	seenConditions := make(map[string]bool, len(newObj.Status.Conditions))
	for i, condition := range newObj.Status.Conditions {
		path := field.NewPath("status", "conditions").Index(i)
		if seenConditions[condition.Type] {
			allErrs = append(allErrs, field.Duplicate(path.Child("type"), condition.Type))
		}
		seenConditions[condition.Type] = true
		if condition.Type != ebsv1.RpmRepoConditionRepositoryReady && condition.Type != ebsv1.RpmRepoConditionPublishSucceed &&
			condition.Type != ebsv1.RpmRepoConditionCleanupReady {
			allErrs = append(allErrs, field.NotSupported(path.Child("type"), condition.Type,
				[]string{ebsv1.RpmRepoConditionRepositoryReady, ebsv1.RpmRepoConditionPublishSucceed, ebsv1.RpmRepoConditionCleanupReady}))
		}
		if condition.Type == ebsv1.RpmRepoConditionCleanupReady && condition.Status != metav1.ConditionTrue {
			allErrs = append(allErrs, field.NotSupported(path.Child("status"), condition.Status, []string{string(metav1.ConditionTrue)}))
		}
		if condition.Status != metav1.ConditionTrue && condition.Status != metav1.ConditionFalse {
			allErrs = append(allErrs, field.NotSupported(path.Child("status"), condition.Status, []string{string(metav1.ConditionTrue), string(metav1.ConditionFalse)}))
		}
		var reasons []string
		switch {
		case condition.Type == ebsv1.RpmRepoConditionCleanupReady && condition.Status == metav1.ConditionTrue:
			reasons = []string{ebsv1.RpmRepoReasonOperationsDrained}
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
