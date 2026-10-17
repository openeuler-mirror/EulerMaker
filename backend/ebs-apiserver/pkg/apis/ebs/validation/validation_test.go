package validation

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func assertErrorList(t *testing.T, errs field.ErrorList, wantErrs int, wantFields map[string]field.ErrorType) {
	t.Helper()

	if len(errs) != wantErrs {
		t.Fatalf("expected %d errors, got %d: %v", wantErrs, len(errs), errs)
	}

	for wantField, wantType := range wantFields {
		if !hasFieldError(errs, wantField, wantType) {
			t.Fatalf("expected %s error for field %q, got: %v", wantType, wantField, errs)
		}
	}
}

func hasFieldError(errs field.ErrorList, wantField string, wantType field.ErrorType) bool {
	for _, err := range errs {
		if err.Field == wantField && err.Type == wantType {
			return true
		}
	}
	return false
}

func validBuildTarget() ebsv1.BuildTarget {
	return ebsv1.BuildTarget{
		Os:   "openEuler-22.03-LTS",
		Arch: "x86_64",
	}
}
