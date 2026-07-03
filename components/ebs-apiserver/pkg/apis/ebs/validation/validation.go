package validation

import (
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
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
		if len(bt.OsVariant) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets").Index(i).Child("osVariant"), "osVariant is required"))
		}
		if len(bt.Architecture) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec", "buildTargets").Index(i).Child("architecture"), "architecture is required"))
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
	return allErrs
}

func ValidateBuildUpdate(newObj, oldObj *ebsv1.Build) field.ErrorList {
	return ValidateBuild(newObj)
}

func ValidateBuildStatusUpdate(newObj, oldObj *ebsv1.Build) field.ErrorList {
	var allErrs field.ErrorList
	return allErrs
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
	} else if obj.Spec.Type != "dc" && obj.Spec.Type != "vm" && obj.Spec.Type != "hw" {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec", "type"), obj.Spec.Type, []string{"dc", "vm", "hw"}))
	}
	if len(obj.Spec.Arch) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "arch"), "arch is required"))
	}
	return allErrs
}

func ValidateRunnerUpdate(newObj, oldObj *ebsv1.Runner) field.ErrorList {
	var allErrs field.ErrorList
	if newObj.Spec.Type != oldObj.Spec.Type {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "type"), "type is immutable"))
	}
	if newObj.Spec.Arch != oldObj.Spec.Arch {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "arch"), "arch is immutable"))
	}
	allErrs = append(allErrs, ValidateRunner(newObj)...)
	return allErrs
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
