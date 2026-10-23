package validation

import (
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateBuild(obj *ebsv1.Build) field.ErrorList {
	allErrs := validateBuildGitRef(obj)
	namePath := field.NewPath("metadata", "name")
	if obj.Name == "" {
		allErrs = append(allErrs, field.Required(namePath, "Build name is required"))
	} else if !uuidPattern.MatchString(obj.Name) {
		allErrs = append(allErrs, field.Invalid(namePath, obj.Name, "must be a canonical lowercase UUID"))
	}
	if len(obj.Spec.BuildType) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildType"), "buildType is required"))
	} else if !obj.Spec.BuildType.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "buildType"), obj.Spec.BuildType, ebsv1.BuildTypeValues()))
	}
	if len(obj.Spec.Packages) == 0 && obj.Spec.BuildType != ebsv1.BuildTypeFull && obj.Spec.BuildType != ebsv1.BuildTypeIncremental {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "packages"), "at least one package is required"))
	}
	if len(obj.Spec.BuildTarget.OS) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTarget", "os"), "os is required"))
	}
	if len(obj.Spec.BuildTarget.Arch) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTarget", "arch"), "arch is required"))
	}
	labelsPath := field.NewPath("metadata", "labels")
	if value, ok := obj.Labels[ebsv1.BuildTargetOSLabel]; !ok || len(value) == 0 {
		allErrs = append(allErrs, field.Required(labelsPath.Key(ebsv1.BuildTargetOSLabel), "target OS label is required"))
	} else if value != obj.Spec.BuildTarget.OS {
		allErrs = append(allErrs, field.Invalid(labelsPath.Key(ebsv1.BuildTargetOSLabel), value, "must match spec.buildTarget.os"))
	}
	if value, ok := obj.Labels[ebsv1.BuildTargetArchLabel]; !ok || len(value) == 0 {
		allErrs = append(allErrs, field.Required(labelsPath.Key(ebsv1.BuildTargetArchLabel), "target architecture label is required"))
	} else if value != obj.Spec.BuildTarget.Arch {
		allErrs = append(allErrs, field.Invalid(labelsPath.Key(ebsv1.BuildTargetArchLabel), value, "must match spec.buildTarget.arch"))
	}
	if value, ok := obj.Labels[ebsv1.BuildTypeLabel]; !ok || len(value) == 0 {
		allErrs = append(allErrs, field.Required(labelsPath.Key(ebsv1.BuildTypeLabel), "build type label is required"))
	} else if value != string(obj.Spec.BuildType) {
		allErrs = append(allErrs, field.Invalid(labelsPath.Key(ebsv1.BuildTypeLabel), value, "must match spec.buildType"))
	}
	return allErrs
}

func validateBuildGitRef(build *ebsv1.Build) field.ErrorList {
	ref, exists := build.Annotations[ebsv1.BuildGitRefAnnotation]
	if !exists {
		return nil
	}
	path := field.NewPath("metadata", "annotations").Key(ebsv1.BuildGitRefAnnotation)
	var errs field.ErrorList
	if build.Spec.BuildType != ebsv1.BuildTypeSingle || len(build.Spec.Packages) != 1 {
		errs = append(errs, field.Invalid(path, ref, "requires buildType single and exactly one packages entry"))
	}
	if ref == "" {
		errs = append(errs, field.Required(path, "a full Git ref is required"))
	} else if !validFullGitRef(ref) {
		errs = append(errs, field.Invalid(path, ref, "must be a valid full Git ref starting with refs/"))
	}
	return errs
}

func ValidateBuildTarget(build *ebsv1.Build, content *ebsv1.BuildTargetContent) field.ErrorList {
	if _, exists := content.Targets[build.Spec.BuildTarget.OS].Arches[build.Spec.BuildTarget.Arch]; !exists {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "buildTarget"), build.Spec.BuildTarget,
			"target is not configured in build-target Config")}
	}
	return nil
}

func ValidateBuildPackages(build *ebsv1.Build, project *ebsv1.Project) field.ErrorList {
	names := make(map[string]struct{}, len(project.Spec.PackageRepos))
	for _, repo := range project.Spec.PackageRepos {
		names[repo.Name] = struct{}{}
	}
	var errs field.ErrorList
	for i, name := range build.Spec.Packages {
		if _, exists := names[name]; !exists {
			errs = append(errs, field.Invalid(field.NewPath("spec", "packages").Index(i), name,
				"package does not exist in Project.spec.packageRepos"))
		}
	}
	return errs
}

func ValidateBuildUpdate(newObj, oldObj *ebsv1.Build) field.ErrorList {
	allErrs := ValidateBuild(newObj)
	newRef, newExists := newObj.Annotations[ebsv1.BuildGitRefAnnotation]
	oldRef, oldExists := oldObj.Annotations[ebsv1.BuildGitRefAnnotation]
	if newExists != oldExists || newRef != oldRef {
		path := field.NewPath("metadata", "annotations").Key(ebsv1.BuildGitRefAnnotation)
		allErrs = append(allErrs, field.Invalid(path, newRef, "field is immutable"))
	}
	if !apiequality.Semantic.DeepEqual(newObj.Spec, oldObj.Spec) {
		previous, next := oldObj.Spec, newObj.Spec
		previous.Packages, next.Packages = nil, nil
		if oldObj.Spec.BuildType != ebsv1.BuildTypeIncremental || oldObj.Status.Phase != ebsv1.BuildPending || !apiequality.Semantic.DeepEqual(previous, next) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec"), newObj.Spec, "field is immutable except incremental packages while Pending"))
		}
	}
	return allErrs
}

func ValidateBuildStatusUpdate(newObj, oldObj *ebsv1.Build) field.ErrorList {
	var allErrs field.ErrorList
	if stage := newObj.Status.Stage; stage != "" && !stage.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "stage"), stage, ebsv1.BuildStageValues()))
	}
	if oldObj.Status.Phase.IsTerminal() && newObj.Status.Phase != oldObj.Status.Phase {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("status", "phase"), "terminal Build phase is immutable"))
	}
	if !newObj.Status.Phase.IsValid() {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "phase"), newObj.Status.Phase, ebsv1.BuildPhaseValues()))
	}
	return allErrs
}
