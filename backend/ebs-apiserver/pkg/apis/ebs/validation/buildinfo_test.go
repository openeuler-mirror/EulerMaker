package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestBuildInfoFailedStatusValidation(t *testing.T) {
	oldInfo := &ebsv1.BuildInfo{Status: ebsv1.BuildInfoStatus{Phase: ebsv1.BuildInfoProcessing}}
	newInfo := oldInfo.DeepCopy()
	newInfo.Status.Phase = ebsv1.BuildInfoFailed
	if errs := ValidateBuildInfoStatusUpdate(newInfo, oldInfo); len(errs) == 0 {
		t.Fatal("Failed without BuildPayloadInvalid condition was accepted")
	}
	newInfo.Status.Conditions = []metav1.Condition{{Type: "BuildPayloadInvalid", Status: metav1.ConditionTrue, Reason: "BuildPayloadParseFailed"}}
	if errs := ValidateBuildInfoStatusUpdate(newInfo, oldInfo); len(errs) != 0 {
		t.Fatalf("valid Failed status rejected: %v", errs)
	}
	backToProcessing := newInfo.DeepCopy()
	backToProcessing.Status.Phase = ebsv1.BuildInfoProcessing
	if errs := ValidateBuildInfoStatusUpdate(backToProcessing, newInfo); len(errs) == 0 {
		t.Fatal("terminal Failed phase could be changed")
	}
}
