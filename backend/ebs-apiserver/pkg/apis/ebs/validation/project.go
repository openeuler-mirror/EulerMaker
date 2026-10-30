package validation

import (
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
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
	distroPath := field.NewPath("spec", "distro")
	if obj.Spec.Distro == "" {
		allErrs = append(allErrs, field.Required(distroPath, "distro is required"))
	} else {
		for _, message := range validation.IsValidLabelValue(obj.Spec.Distro) {
			allErrs = append(allErrs, field.Invalid(distroPath, obj.Spec.Distro, message))
		}
	}
	archesPath := field.NewPath("spec", "buildArches")
	if len(obj.Spec.BuildArches) == 0 {
		allErrs = append(allErrs, field.Required(archesPath, "at least one architecture is required"))
	}
	seenArches := make(map[string]bool, len(obj.Spec.BuildArches))
	for i, arch := range obj.Spec.BuildArches {
		if arch == "" {
			allErrs = append(allErrs, field.Required(archesPath.Index(i), "arch is required"))
		} else {
			for _, message := range validation.IsValidLabelValue(arch) {
				allErrs = append(allErrs, field.Invalid(archesPath.Index(i), arch, message))
			}
		}
		if seenArches[arch] {
			allErrs = append(allErrs, field.Duplicate(archesPath.Index(i), arch))
		}
		seenArches[arch] = true
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

func ValidateProjectArtifactInstance(project *ebsv1.Project, content *ebsv1.ServiceConfigContent) field.ErrorList {
	instance, exists := project.Annotations[ebsv1.ProjectArtifactsAnnotation]
	if !exists {
		instance = "default"
	}
	if _, found := content.ArtifactManagers[instance]; !found {
		return field.ErrorList{
			field.Invalid(field.NewPath("metadata", "annotations").Key(ebsv1.ProjectArtifactsAnnotation), instance,
				"instance is not configured in services"),
		}
	}
	return nil
}

func ValidateProjectUpdate(newObj, oldObj *ebsv1.Project) field.ErrorList {
	errs := ValidateProject(newObj)
	if newObj.Spec.Distro != oldObj.Spec.Distro {
		errs = append(errs, field.Invalid(field.NewPath("spec", "distro"), newObj.Spec.Distro, "field is immutable"))
	}
	return errs
}

func ValidateProjectStatusUpdate(newObj, oldObj *ebsv1.Project) field.ErrorList {
	var allErrs field.ErrorList
	return allErrs
}
