package validation

import (
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

var (
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
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

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
