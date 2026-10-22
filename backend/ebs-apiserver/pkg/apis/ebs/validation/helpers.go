package validation

import (
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validateDefaultRef(ref string, refPath *field.Path) field.ErrorList {
	if ref != "" && !validFullGitRef(ref) {
		return field.ErrorList{field.Invalid(refPath, ref, "must be a valid full Git ref starting with refs/")}
	}
	return nil
}

func validatePackageRepos(repos []ebsv1.PackageRepo, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for i, repo := range repos {
		allErrs = append(allErrs, validateDefaultRef(repo.Ref, path.Index(i).Child("ref"))...)
		if repo.CommitID != "" && !commitIDPattern.MatchString(repo.CommitID) {
			allErrs = append(allErrs, field.Invalid(path.Index(i).Child("commitID"), repo.CommitID, "must be a full 40-character lowercase hexadecimal commit ID"))
		}
	}
	return allErrs
}

// validFullGitRef checks Git ref-name syntax without normalization or remote access.
func validFullGitRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return false
	}
	for _, ch := range ref {
		if ch < 0x20 || ch == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
