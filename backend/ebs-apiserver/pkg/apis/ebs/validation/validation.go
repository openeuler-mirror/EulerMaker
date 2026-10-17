package validation

import (
	"regexp"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

var (
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	uuidV4Pattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	gitRefNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/][A-Za-z0-9._/-]*$`)
	gitCommitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	gitRefDotSegment  = regexp.MustCompile(`(^|/)\.\.?($|/)`)
)

func validateDefaultRef(ref ebsv1.GitRef, refPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if ref == (ebsv1.GitRef{}) {
		return allErrs
	}
	if ref.Type == "" {
		allErrs = append(allErrs, field.Required(refPath.Child("type"), "ref type is required"))
	} else if ref.Type != ebsv1.GitRefBranch && ref.Type != ebsv1.GitRefTag {
		allErrs = append(allErrs, field.NotSupported(refPath.Child("type"), ref.Type, []string{string(ebsv1.GitRefBranch), string(ebsv1.GitRefTag)}))
	}
	if ref.Value == "" {
		allErrs = append(allErrs, field.Required(refPath.Child("value"), "ref value is required"))
	} else if !gitRefNamePattern.MatchString(ref.Value) || ref.Value[0] == '-' || gitRefDotSegment.MatchString(ref.Value) || strings.Contains(ref.Value, "..") {
		allErrs = append(allErrs, field.Invalid(refPath.Child("value"), ref.Value, "must be a safe branch or tag name"))
	}
	return allErrs
}

func validatePackageRepos(repos []ebsv1.PackageRepo, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	validTypes := []string{string(ebsv1.GitRefBranch), string(ebsv1.GitRefTag), string(ebsv1.GitRefCommit)}
	for i, repo := range repos {
		if repo.Ref == (ebsv1.GitRef{}) {
			continue
		}
		refPath := path.Index(i).Child("ref")
		if repo.Ref.Type == "" {
			allErrs = append(allErrs, field.Required(refPath.Child("type"), "ref type is required"))
		} else if repo.Ref.Type != ebsv1.GitRefBranch && repo.Ref.Type != ebsv1.GitRefTag && repo.Ref.Type != ebsv1.GitRefCommit {
			allErrs = append(allErrs, field.NotSupported(refPath.Child("type"), repo.Ref.Type, validTypes))
		}
		if repo.Ref.Value == "" {
			allErrs = append(allErrs, field.Required(refPath.Child("value"), "ref value is required"))
			continue
		}
		switch repo.Ref.Type {
		case ebsv1.GitRefBranch, ebsv1.GitRefTag:
			if !gitRefNamePattern.MatchString(repo.Ref.Value) || repo.Ref.Value[0] == '-' || gitRefDotSegment.MatchString(repo.Ref.Value) || strings.Contains(repo.Ref.Value, "..") {
				allErrs = append(allErrs, field.Invalid(refPath.Child("value"), repo.Ref.Value, "must be a safe branch or tag name"))
			}
		case ebsv1.GitRefCommit:
			if !gitCommitPattern.MatchString(repo.Ref.Value) {
				allErrs = append(allErrs, field.Invalid(refPath.Child("value"), repo.Ref.Value, "must be a full 40-character lowercase commit ID"))
			}
		}
	}
	return allErrs
}

func ValidateJob(obj *ebsv1.Job) field.ErrorList {
	var allErrs field.ErrorList
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

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

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
