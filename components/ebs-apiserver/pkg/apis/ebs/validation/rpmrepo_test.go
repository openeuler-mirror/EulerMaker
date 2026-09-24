package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestValidateRpmRepoStatusUpdate(t *testing.T) {
	valid := func() *ebsv1.RpmRepo {
		return &ebsv1.RpmRepo{Status: ebsv1.RpmRepoStatus{
			Repository: &ebsv1.RpmRepoRepositoryStatus{
				RepositoryUID: "base", ContentURL: "/repositories/v1/base/",
				Transition: &ebsv1.RepositoryTransition{
					RepositoryUID: "next", BaseRepositoryUID: "base",
					Inputs: []ebsv1.RepositoryInput{{JobName: "job", JobUID: "uid", SpecName: "spec"}},
				},
			},
		}}
	}
	for _, tc := range []struct {
		name      string
		mutate    func(*ebsv1.RpmRepo)
		wantField string
	}{
		{"empty", func(o *ebsv1.RpmRepo) { o.Status = ebsv1.RpmRepoStatus{} }, ""},
		{"empty-repository", func(o *ebsv1.RpmRepo) { o.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{} }, ""},
		{"baseline", func(o *ebsv1.RpmRepo) { o.Status.Repository.Transition = nil }, ""},
		{"in-flight", func(o *ebsv1.RpmRepo) {}, ""},
		{"consumed", func(o *ebsv1.RpmRepo) { o.Status.Repository.SourceJobUIDs = []string{"uid"} }, ""},
		{"skipped", func(o *ebsv1.RpmRepo) { o.Status.Repository.SkippedJobUIDs = []string{"bad-uid"} }, ""},
		{"empty-skipped-uid", func(o *ebsv1.RpmRepo) { o.Status.Repository.SkippedJobUIDs = []string{""} }, "status.repository.skippedJobUIDs[0]"},
		{"duplicate-skipped-uid", func(o *ebsv1.RpmRepo) { o.Status.Repository.SkippedJobUIDs = []string{"bad-uid", "bad-uid"} }, "status.repository.skippedJobUIDs[1]"},
		{"skipped-source-overlap", func(o *ebsv1.RpmRepo) {
			o.Status.Repository.SourceJobUIDs = []string{"uid"}
			o.Status.Repository.SkippedJobUIDs = []string{"uid"}
		}, "status.repository.skippedJobUIDs[0]"},
		{"uid-only", func(o *ebsv1.RpmRepo) { o.Status.Repository.ContentURL = "" }, "status.repository"},
		{"url-only", func(o *ebsv1.RpmRepo) { o.Status.Repository.RepositoryUID = "" }, "status.repository"},
		{"consumed-without-version", func(o *ebsv1.RpmRepo) {
			o.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{SourceJobUIDs: []string{"uid"}}
		}, "status.repository.sourceJobUIDs"},
		{"missing-transition-uid", func(o *ebsv1.RpmRepo) { o.Status.Repository.Transition.RepositoryUID = "" }, "status.repository.transition.repositoryUID"},
		{"missing-inputs", func(o *ebsv1.RpmRepo) { o.Status.Repository.Transition.Inputs = nil }, "status.repository.transition.inputs"},
		{"empty-phase", func(o *ebsv1.RpmRepo) { o.Status.Release = &ebsv1.RpmRepoReleaseStatus{} }, "status.release.phase"},
		{"skipped-without-content-url", func(o *ebsv1.RpmRepo) {
			o.Status = ebsv1.RpmRepoStatus{Release: &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}}
		}, ""},
		{"invalid-phase", func(o *ebsv1.RpmRepo) { o.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: "Unknown"} }, "status.release.phase"},
		{"ready-without-url", func(o *ebsv1.RpmRepo) {
			o.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseReady}
		}, "status.release.contentURL"},
		{"failed-batch-retained", func(o *ebsv1.RpmRepo) {
			o.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
			o.Status.Conditions = []metav1.Condition{
				{Type: ebsv1.RpmRepoConditionRepositoryReady, Status: metav1.ConditionFalse, Reason: ebsv1.RpmRepoReasonRepositoryCreationFailed},
				{Type: ebsv1.RpmRepoConditionPublishSucceed, Status: metav1.ConditionFalse, Reason: ebsv1.RpmRepoReasonRepositoryCreationFailed},
			}
		}, ""},
		{"published-condition", func(o *ebsv1.RpmRepo) {
			o.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionPublishSucceed, Status: metav1.ConditionTrue, Reason: ebsv1.RpmRepoReasonReleaseActivated}}
		}, ""},
		{"rejected-aborted-phase", func(o *ebsv1.RpmRepo) { o.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: "Aborted"} }, "status.release.phase"},
		{"aborted-condition", func(o *ebsv1.RpmRepo) {
			o.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
			o.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionPublishSucceed, Status: metav1.ConditionFalse, Reason: ebsv1.RpmRepoReasonBuildAborted}}
		}, ""},
		{"condition-type", func(o *ebsv1.RpmRepo) {
			o.Status.Conditions = []metav1.Condition{{Type: "Other", Status: metav1.ConditionTrue, Reason: ebsv1.RpmRepoReasonReleaseActivated}}
		}, "status.conditions[0].type"},
		{"condition-status", func(o *ebsv1.RpmRepo) {
			o.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionPublishSucceed, Status: "Unknown", Reason: ebsv1.RpmRepoReasonReleaseActivated}}
		}, "status.conditions[0].status"},
		{"condition-reason", func(o *ebsv1.RpmRepo) {
			o.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionPublishSucceed, Status: metav1.ConditionTrue, Reason: "Other"}}
		}, "status.conditions[0].reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := valid()
			tc.mutate(obj)
			errs := ValidateRpmRepoStatusUpdate(obj, valid())
			if tc.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			for _, err := range errs {
				if err.Field == tc.wantField {
					return
				}
			}
			t.Fatalf("expected error for %s, got %v", tc.wantField, errs)
		})
	}
	for _, phase := range ebsv1.RpmRepoReleasePhaseValues() {
		for _, checkpoint := range []bool{false, true} {
			obj := valid()
			obj.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePhase(phase), ContentURL: "/repositories/project/os/arch/"}
			if checkpoint {
				obj.Status.Release.Transition = &ebsv1.ReleaseTransition{SourceRepositoryUID: "base"}
			}
			terminal := phase == "Ready" || phase == "Failed" || phase == "Skipped"
			errs := ValidateRpmRepoStatusUpdate(obj, valid())
			if (len(errs) != 0) != (terminal && checkpoint) {
				t.Fatalf("phase=%s checkpoint=%v: %v", phase, checkpoint, errs)
			}
		}
	}
}

func TestRpmRepoConditionCombinations(t *testing.T) {
	allowed := map[string]bool{
		"RepositoryReady/True/RepositoryCreated":         true,
		"RepositoryReady/False/RepositoryCreationFailed": true,
		"PublishSucceed/True/ReleaseActivated":           true,
		"PublishSucceed/False/ReleaseFailed":             true,
		"PublishSucceed/False/RepositoryCreationFailed":  true,
		"PublishSucceed/False/NoPublishableArtifacts":    true,
		"PublishSucceed/False/BuildAborted":              true,
	}
	for _, typ := range []string{ebsv1.RpmRepoConditionRepositoryReady, ebsv1.RpmRepoConditionPublishSucceed, "Other", ""} {
		for _, status := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, "Unknown", ""} {
			for _, reason := range []string{ebsv1.RpmRepoReasonRepositoryCreated, ebsv1.RpmRepoReasonRepositoryCreationFailed, ebsv1.RpmRepoReasonReleaseActivated, ebsv1.RpmRepoReasonReleaseFailed, ebsv1.RpmRepoReasonNoPublishableArtifacts, ebsv1.RpmRepoReasonBuildAborted, "RepositoryPublished", "RepositoryPublishFailed", "RepositoryPublishAborted", "RepositoryFailed", "Other", ""} {
				key := typ + "/" + string(status) + "/" + reason
				t.Run(key, func(t *testing.T) {
					obj := &ebsv1.RpmRepo{Status: ebsv1.RpmRepoStatus{Conditions: []metav1.Condition{{Type: typ, Status: status, Reason: reason}}}}
					errs := ValidateRpmRepoStatusUpdate(obj, &ebsv1.RpmRepo{})
					if (len(errs) == 0) != allowed[key] {
						t.Fatalf("allowed=%v, errors=%v", allowed[key], errs)
					}
				})
			}
		}
	}
}
