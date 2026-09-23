package validation

import (
	"regexp"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

var (
	uuidPattern         = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	packageNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]*(:[A-Za-z0-9][A-Za-z0-9+._-]*)*$`)
	architecturePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	uuidV4Pattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	gitRefNamePattern   = regexp.MustCompile(`^[A-Za-z0-9._/][A-Za-z0-9._/-]*$`)
	gitCommitPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	gitRefDotSegment    = regexp.MustCompile(`(^|/)\.\.?($|/)`)
)

func ValidateProject(obj *ebsv1.Project) field.ErrorList {
	var allErrs field.ErrorList
	if value, exists := obj.Labels[ebsv1.ProjectTypeLabel]; exists && value != ebsv1.ProjectTypeCommunity && value != ebsv1.ProjectTypePersonal {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("metadata", "labels").Key(ebsv1.ProjectTypeLabel), value, []string{ebsv1.ProjectTypeCommunity, ebsv1.ProjectTypePersonal}))
	}
	namePath := field.NewPath("metadata", "name")
	if len(obj.Name) == 0 {
		allErrs = append(allErrs, field.Required(namePath, "name is required"))
	} else if errs := validation.IsDNS1123Label(obj.Name); len(errs) > 0 {
		for _, e := range errs {
			allErrs = append(allErrs, field.Invalid(namePath, obj.Name, e))
		}
	} else if obj.Name == "default" {
		allErrs = append(allErrs, field.Forbidden(namePath, "default is reserved for global resources"))
	}
	if len(obj.Spec.BuildTargets) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets"), "at least one build target is required"))
	}
	seenTargets := make(map[[2]string]struct{}, len(obj.Spec.BuildTargets))
	for i, bt := range obj.Spec.BuildTargets {
		if len(bt.Os) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets").Index(i).Child("os"), "os is required"))
		}
		if len(bt.Arch) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets").Index(i).Child("arch"), "arch is required"))
		}
		if bt.Os != "" && bt.Arch != "" {
			key := [2]string{bt.Os, bt.Arch}
			if _, exists := seenTargets[key]; exists {
				allErrs = append(allErrs, field.Duplicate(field.NewPath("spec", "buildTargets").Index(i), key))
			}
			seenTargets[key] = struct{}{}
		}
	}
	packageReposPath := field.NewPath("spec", "packageRepos")
	allErrs = append(allErrs, validateDefaultRef(obj.Spec.DefaultRef, field.NewPath("spec", "defaultRef"))...)
	seenRepoNames := make(map[string]struct{}, len(obj.Spec.PackageRepos))
	for i, repo := range obj.Spec.PackageRepos {
		if repo.Name == "" {
			allErrs = append(allErrs, field.Required(packageReposPath.Index(i).Child("name"), "name is required"))
			continue
		}
		if _, exists := seenRepoNames[repo.Name]; exists {
			allErrs = append(allErrs, field.Duplicate(packageReposPath.Index(i).Child("name"), repo.Name))
		}
		seenRepoNames[repo.Name] = struct{}{}
	}
	allErrs = append(allErrs, validatePackageRepos(obj.Spec.PackageRepos, packageReposPath)...)
	return allErrs
}

func ValidateProjectUpdate(newObj, oldObj *ebsv1.Project) field.ErrorList {
	return ValidateProject(newObj)
}

func ValidateProjectStatusUpdate(newObj, oldObj *ebsv1.Project) field.ErrorList {
	var allErrs field.ErrorList
	return allErrs
}

func ValidateSnapshot(obj *ebsv1.Snapshot) field.ErrorList {
	allErrs := validateDefaultRef(obj.Spec.DefaultRef, field.NewPath("spec", "defaultRef"))
	return append(allErrs, validatePackageRepos(obj.Spec.PackageRepos, field.NewPath("spec", "packageRepos"))...)
}

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

func ValidateSnapshotUpdate(newObj, oldObj *ebsv1.Snapshot) field.ErrorList {
	return ValidateSnapshot(newObj)
}

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
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec"), newObj.Spec, "field is immutable"))
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

func ValidateBuildInfo(obj *ebsv1.BuildInfo) field.ErrorList { return nil }
func ValidateBuildInfoUpdate(newObj, oldObj *ebsv1.BuildInfo) field.ErrorList {
	return ValidateBuildInfo(newObj)
}
func ValidateBuildInfoStatusUpdate(newObj, oldObj *ebsv1.BuildInfo) field.ErrorList { return nil }

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
		if len(repository.SourceJobUIDs) > 0 && (repository.RepositoryUID == "" || repository.ContentURL == "") {
			allErrs = append(allErrs, field.Invalid(path.Child("sourceJobUIDs"), repository.SourceJobUIDs, "requires repositoryUID and contentURL"))
		}
		seen := make(map[string]bool, len(repository.SourceJobUIDs)+len(repository.SkippedJobUIDs))
		for _, uid := range repository.SourceJobUIDs {
			seen[uid] = true
		}
		for i, uid := range repository.SkippedJobUIDs {
			itemPath := path.Child("skippedJobUIDs").Index(i)
			if uid == "" {
				allErrs = append(allErrs, field.Required(itemPath, "Job UID is required"))
			} else if seen[uid] {
				allErrs = append(allErrs, field.Duplicate(itemPath, uid))
			}
			seen[uid] = true
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
			reasons = []string{ebsv1.RpmRepoReasonReleaseFailed, ebsv1.RpmRepoReasonRepositoryCreationFailed, ebsv1.RpmRepoReasonNoPublishableArtifacts, ebsv1.RpmRepoReasonBuildAborted}
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

func ValidateBuildResource(obj *ebsv1.BuildResource) field.ErrorList {
	var allErrs field.ErrorList
	if obj.Name == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("metadata", "name"), "name is required"))
	}
	if obj.Namespace == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("metadata", "namespace"), "namespace is required"))
	}

	specPath := field.NewPath("spec")
	defaultConfigured := !resourceRequirementsEmpty(obj.Spec.Default)
	var defaultErrs field.ErrorList
	if !defaultConfigured {
		defaultErrs = append(defaultErrs, field.Required(specPath.Child("default"), "a complete table default is required"))
	} else {
		defaultErrs = validateBuildResources(obj.Spec.Default, specPath.Child("default"), true)
		if len(defaultErrs) == 0 {
			effectiveDefault := mergeResourceRequirements(ebsv1.ResourceRequirements{}, obj.Spec.Default)
			defaultErrs = append(defaultErrs, validateEffectiveResourceLimits(effectiveDefault, specPath.Child("default"))...)
		}
	}
	allErrs = append(allErrs, defaultErrs...)
	if len(obj.Spec.Packages) == 0 && !(obj.Namespace == "default" && obj.Name == "default" && defaultConfigured) {
		allErrs = append(allErrs, field.Required(specPath.Child("packages"), "at least one package is required"))
	}
	for packageName, config := range obj.Spec.Packages {
		packagePath := specPath.Child("packages").Key(packageName)
		if !packageNamePattern.MatchString(packageName) {
			allErrs = append(allErrs, field.Invalid(packagePath, packageName, "must be a valid spec package name"))
		}
		packageDefault := !resourceRequirementsEmpty(config.Default)
		if !packageDefault && len(config.Arches) == 0 {
			allErrs = append(allErrs, field.Required(packagePath, "default or at least one architecture is required"))
		}
		packageBase := mergeResourceRequirements(ebsv1.ResourceRequirements{}, obj.Spec.Default)
		packageBaseValid := len(defaultErrs) == 0
		if packageDefault {
			packageErrs := validateBuildResources(config.Default, packagePath.Child("default"), false)
			allErrs = append(allErrs, packageErrs...)
			packageBase = mergeResourceRequirements(packageBase, config.Default)
			packageBaseValid = packageBaseValid && len(packageErrs) == 0
			if packageBaseValid {
				effectiveErrs := validateEffectiveResourceLimits(packageBase, packagePath.Child("default"))
				allErrs = append(allErrs, effectiveErrs...)
				packageBaseValid = len(effectiveErrs) == 0
			}
		}
		for arch, resources := range config.Arches {
			archPath := packagePath.Child("arches").Key(arch)
			if !architecturePattern.MatchString(arch) {
				allErrs = append(allErrs, field.Invalid(archPath, arch, "must match ^[a-z0-9][a-z0-9._-]{0,62}$"))
			}
			if resourceRequirementsEmpty(resources) {
				allErrs = append(allErrs, field.Required(archPath, "at least one resource override is required"))
				continue
			}
			archErrs := validateBuildResources(resources, archPath, false)
			allErrs = append(allErrs, archErrs...)
			if packageBaseValid && len(archErrs) == 0 {
				effective := mergeResourceRequirements(packageBase, resources)
				allErrs = append(allErrs, validateEffectiveResourceLimits(effective, archPath)...)
			}
		}
	}
	return allErrs
}

func ValidateBuildResourceUpdate(newObj, oldObj *ebsv1.BuildResource) field.ErrorList {
	return ValidateBuildResource(newObj)
}

func resourceRequirementsEmpty(resources ebsv1.ResourceRequirements) bool {
	return len(resources.Requests) == 0 && len(resources.Limits) == 0
}

func mergeResourceRequirements(base, override ebsv1.ResourceRequirements) ebsv1.ResourceRequirements {
	merged := ebsv1.ResourceRequirements{Requests: map[string]string{}, Limits: map[string]string{}}
	for name, value := range base.Requests {
		merged.Requests[name] = value
		if _, hasLimit := base.Limits[name]; !hasLimit {
			merged.Limits[name] = value
		}
	}
	for name, value := range base.Limits {
		merged.Limits[name] = value
	}
	for name, value := range override.Requests {
		merged.Requests[name] = value
		if _, hasLimit := override.Limits[name]; !hasLimit {
			merged.Limits[name] = value
		}
	}
	for name, value := range override.Limits {
		merged.Limits[name] = value
	}
	return merged
}

func validateBuildResources(resources ebsv1.ResourceRequirements, path *field.Path, complete bool) field.ErrorList {
	var allErrs field.ErrorList
	requests, requestErrs := validateResourceMap(resources.Requests, path.Child("requests"), complete)
	allErrs = append(allErrs, requestErrs...)
	limits, limitErrs := validateResourceMap(resources.Limits, path.Child("limits"), false)
	allErrs = append(allErrs, limitErrs...)
	for _, name := range []string{"cpu", "memory"} {
		request, requestOK := requests[name]
		limit, limitOK := limits[name]
		if requestOK && limitOK && limit.Cmp(request) < 0 {
			allErrs = append(allErrs, field.Invalid(path.Child("limits").Key(name), resources.Limits[name], "must be greater than or equal to request"))
		}
	}
	return allErrs
}

func validateEffectiveResourceLimits(resources ebsv1.ResourceRequirements, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for _, name := range []string{"cpu", "memory"} {
		request, requestErr := resource.ParseQuantity(resources.Requests[name])
		limit, limitErr := resource.ParseQuantity(resources.Limits[name])
		if requestErr == nil && limitErr == nil && limit.Cmp(request) < 0 {
			allErrs = append(allErrs, field.Invalid(path.Child("limits").Key(name), resources.Limits[name], "effective limit must be greater than or equal to effective request"))
		}
	}
	return allErrs
}

func validateResourceMap(values map[string]string, path *field.Path, required bool) (map[string]resource.Quantity, field.ErrorList) {
	parsed := make(map[string]resource.Quantity, 2)
	var allErrs field.ErrorList
	for name := range values {
		if name != "cpu" && name != "memory" {
			allErrs = append(allErrs, field.NotSupported(path.Key(name), name, []string{"cpu", "memory"}))
		}
	}
	for _, name := range []string{"cpu", "memory"} {
		value, ok := values[name]
		if !ok {
			if required {
				allErrs = append(allErrs, field.Required(path.Key(name), name+" is required"))
			}
			continue
		}
		quantity, err := resource.ParseQuantity(value)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(path.Key(name), value, "must be a valid resource quantity"))
			continue
		}
		if quantity.Sign() <= 0 {
			allErrs = append(allErrs, field.Invalid(path.Key(name), value, "must be greater than zero"))
			continue
		}
		parsed[name] = quantity
	}
	return parsed, allErrs
}

func ValidateJob(obj *ebsv1.Job) field.ErrorList {
	var allErrs field.ErrorList
	return allErrs
}

func ValidateJobUpdate(newObj, oldObj *ebsv1.Job) field.ErrorList {
	return ValidateJob(newObj)
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
