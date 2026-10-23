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
	if len(obj.Spec.BuildTargets) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets"), "at least one build target is required"))
	}
	seenTargets := make(map[[2]string]struct{}, len(obj.Spec.BuildTargets))
	for i, bt := range obj.Spec.BuildTargets {
		if len(bt.OS) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets").Index(i).Child("os"), "os is required"))
		}
		if len(bt.Arch) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets").Index(i).Child("arch"), "arch is required"))
		}
		if bt.OS != "" && bt.Arch != "" {
			key := [2]string{bt.OS, bt.Arch}
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

func ValidateProjectArtifactInstance(project *ebsv1.Project, content *ebsv1.ServiceConfigContent) field.ErrorList {
	instance, exists := project.Annotations[ebsv1.ProjectArtifactsAnnotation]
	if !exists {
		instance = "default"
	}
	if _, found := content.ArtifactManagers[instance]; !found {
		return field.ErrorList{
			field.Invalid(field.NewPath("metadata", "annotations").Key(ebsv1.ProjectArtifactsAnnotation), instance,
				"instance is not configured in service-config"),
		}
	}
	return nil
}

func ValidateProjectUpdate(newObj, oldObj *ebsv1.Project) field.ErrorList {
	return ValidateProject(newObj)
}

func ValidateProjectStatusUpdate(newObj, oldObj *ebsv1.Project) field.ErrorList {
	var allErrs field.ErrorList
	return allErrs
}
