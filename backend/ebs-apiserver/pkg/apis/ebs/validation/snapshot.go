package validation

import (
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func ValidateSnapshot(obj *ebsv1.Snapshot) field.ErrorList {
	allErrs := validateDefaultRef(obj.Spec.DefaultRef, field.NewPath("spec", "defaultRef"))
	return append(allErrs, validatePackageRepos(obj.Spec.PackageRepos, field.NewPath("spec", "packageRepos"))...)
}

func ValidateSnapshotUpdate(newObj, oldObj *ebsv1.Snapshot) field.ErrorList {
	return ValidateSnapshot(newObj)
}
