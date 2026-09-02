package validation

import (
	"regexp"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
)

var (
	packageNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]*(:[A-Za-z0-9][A-Za-z0-9+._-]*)*$`)
	architecturePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

func ValidateProject(obj *ebsv1.Project) field.ErrorList {
	var allErrs field.ErrorList
	if len(obj.Name) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("metadata", "name"), "name is required"))
	} else if errs := validation.IsDNS1123Label(obj.Name); len(errs) > 0 {
		for _, e := range errs {
			allErrs = append(allErrs, field.Invalid(field.NewPath("metadata", "name"), obj.Name, e))
		}
	}
	if len(obj.Spec.BuildTargets) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets"), "at least one build target is required"))
	}
	for i, bt := range obj.Spec.BuildTargets {
		if len(bt.Os) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets").Index(i).Child("os"), "os is required"))
		}
		if len(bt.Arch) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets").Index(i).Child("arch"), "arch is required"))
		}
	}
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
	var allErrs field.ErrorList
	if len(obj.Spec.SpecCommits) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "specCommits"), "specCommits is required"))
	}
	if len(obj.Spec.BuildTargets) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets"), "at least one build target is required"))
	}
	if len(obj.Spec.PackageRepos) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "packageRepos"), "at least one package repo is required"))
	}
	return allErrs
}

func ValidateSnapshotUpdate(newObj, oldObj *ebsv1.Snapshot) field.ErrorList {
	return ValidateSnapshot(newObj)
}

func ValidateBuild(obj *ebsv1.Build) field.ErrorList {
	var allErrs field.ErrorList
	if len(obj.Spec.SnapshotName) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "snapshotName"), "snapshotName is required"))
	}
	if len(obj.Spec.BuildType) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildType"), "buildType is required"))
	}
	if len(obj.Spec.Packages) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "packages"), "at least one package is required"))
	}
	if len(obj.Spec.BuildTarget.Os) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTarget", "os"), "os is required"))
	}
	if len(obj.Spec.BuildTarget.Arch) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTarget", "arch"), "arch is required"))
	}
	return allErrs
}

func ValidateBuildUpdate(newObj, oldObj *ebsv1.Build) field.ErrorList {
	return ValidateBuild(newObj)
}

func ValidateBuildStatusUpdate(newObj, oldObj *ebsv1.Build) field.ErrorList {
	var allErrs field.ErrorList
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
func ValidateRpmRepoStatusUpdate(newObj, oldObj *ebsv1.RpmRepo) field.ErrorList { return nil }

func ValidateBuildResource(obj *ebsv1.BuildResource) field.ErrorList {
	var allErrs field.ErrorList
	if obj.Name == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("metadata", "name"), "name is required"))
	}
	if obj.Namespace == "" {
		allErrs = append(allErrs, field.Required(field.NewPath("metadata", "namespace"), "namespace is required"))
	} else if obj.Name != "" && obj.Name != obj.Namespace {
		allErrs = append(allErrs, field.Invalid(field.NewPath("metadata", "name"), obj.Name, "must equal metadata.namespace"))
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
	return allErrs
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
	return allErrs
}

func ValidateRunnerUpdate(newObj, oldObj *ebsv1.Runner) field.ErrorList {
	return ValidateRunner(newObj)
}

func ValidateRunnerStatusUpdate(newObj, oldObj *ebsv1.Runner) field.ErrorList {
	var allErrs field.ErrorList
	validPhases := []string{"Registering", "Booting", "Running", "Idle", "Offline"}
	phase := newObj.Status.Phase
	if phase != "" {
		valid := false
		for _, p := range validPhases {
			if phase == p {
				valid = true
				break
			}
		}
		if !valid {
			allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "phase"), phase, validPhases))
		}
	}
	return allErrs
}
