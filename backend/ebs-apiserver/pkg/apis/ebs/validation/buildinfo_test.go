package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestBuildInfoSpecResultsEnums(t *testing.T) {
	old := &ebsv1.BuildInfo{Status: ebsv1.BuildInfoStatus{Phase: ebsv1.BuildInfoProcessing}}
	info := old.DeepCopy()
	info.Status.SpecResults = ebsv1.SpecResults{}
	for _, value := range ebsv1.SpecResultPhaseValues() {
		info.Status.SpecResults["repo/"+value+".spec"] = ebsv1.SpecResult{SpecName: value, Status: ebsv1.SpecResultPhase(value)}
	}
	info.Status.SpecResults["unreadable/"] = ebsv1.SpecResult{Status: ebsv1.SpecPrepareFailed}
	info.Status.SpecResults["bad/bad.spec"] = ebsv1.SpecResult{Status: ebsv1.SpecPrepareFailed}
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

func TestBuildInfoStoppedBeforeParsing(t *testing.T) {
	old := &ebsv1.BuildInfo{Status: ebsv1.BuildInfoStatus{Phase: ebsv1.BuildInfoPending}}
	info := old.DeepCopy()
	info.Status.Phase = ebsv1.BuildInfoCompleted
	info.Status.Conditions = []metav1.Condition{{
		Type: "SnapshotUnavailable", Status: metav1.ConditionTrue,
		Reason: "SnapshotNotFound", Message: "snapshot gone",
	}}
	info.Status.SpecResults = ebsv1.SpecResults{
		"undiscovered/": {Status: ebsv1.SpecBuildFailed, Message: "dispatch stopped; see BuildInfo conditions"},
		"repo/a.spec":   {Status: ebsv1.SpecBuildFailed, Message: "dispatch stopped; see BuildInfo conditions"},
	}
	if errs := ValidateBuildInfoStatusUpdate(info, old); len(errs) != 0 {
		t.Fatalf("stopped build results rejected: %v", errs)
	}
}
