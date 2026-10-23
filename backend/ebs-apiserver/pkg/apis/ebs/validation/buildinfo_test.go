package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestBuildInfoSpecStatusEnums(t *testing.T) {
	old := &ebsv1.BuildInfo{Status: ebsv1.BuildInfoStatus{Phase: ebsv1.BuildInfoProcessing}}
	info := old.DeepCopy()
	info.Status.SpecStatus.Build = make(map[string]ebsv1.SpecBuildStatus)
	info.Status.SpecStatus.Install = make(map[string]ebsv1.SpecInstallStatus)
	for _, value := range append(ebsv1.SpecBuildPhaseValues(), "") {
		info.Status.SpecStatus.Build[value] = ebsv1.SpecBuildStatus{Status: ebsv1.SpecBuildPhase(value)}
	}
	for _, value := range append(ebsv1.SpecInstallPhaseValues(), "") {
		info.Status.SpecStatus.Install[value] = ebsv1.SpecInstallStatus{Status: ebsv1.SpecInstallPhase(value)}
	}
	if errs := ValidateBuildInfoStatusUpdate(info, old); len(errs) != 0 {
		t.Fatalf("valid spec statuses: %v", errs)
	}
}

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
