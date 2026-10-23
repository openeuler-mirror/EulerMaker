package validation

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func TestValidateJob(t *testing.T) {
	errs := ValidateJob(validJob())
	assertErrorList(t, errs, 0, nil)
}

func TestValidateJobTolerationEnums(t *testing.T) {
	for _, operator := range append(ebsv1.TolerationOperatorValues(), "") {
		for _, effect := range append(ebsv1.TaintEffectValues(), "") {
			job := validJob()
			job.Spec.Tolerations = []ebsv1.Toleration{{
				Key: "dedicated", Operator: ebsv1.TolerationOperator(operator), Effect: ebsv1.TaintEffect(effect),
			}}
			if errs := ValidateJob(job); len(errs) != 0 {
				t.Fatalf("operator=%q effect=%q: %v", operator, effect, errs)
			}
		}
	}
}

func TestValidateJobScriptRef(t *testing.T) {
	job := validJob()
	if errs := ValidateJob(job); len(errs) != 0 {
		t.Fatalf("empty scriptRefs rejected: %v", errs)
	}
	job.Spec.ScriptRefs = []ebsv1.ScriptRef{{Name: "rpmbuild", UID: "61304b92-72cf-4a41-8bf7-8e0a9d14f6a5", ResourceVersion: "v1:1:1"}}
	if errs := ValidateJob(job); len(errs) != 0 {
		t.Fatalf("valid scriptRef rejected: %v", errs)
	}
	old := job.DeepCopy()
	job.Spec.ScriptRefs[0].ResourceVersion = "v1:1:2"
	if len(ValidateJobUpdate(job, old)) == 0 {
		t.Fatal("scriptRef mutation accepted")
	}
	job.Spec.ScriptRefs = []ebsv1.ScriptRef{{Name: "../bad"}}
	if len(ValidateJob(job)) < 3 {
		t.Fatalf("incomplete scriptRef accepted: %v", ValidateJob(job))
	}
	job.Spec.ScriptRefs = []ebsv1.ScriptRef{{Name: "rpmbuild", UID: "61304b92-72cf-4a41-8bf7-8e0a9d14f6a5", ResourceVersion: "v1:1:1"}, {Name: "other", UID: "61304b92-72cf-4a41-8bf7-8e0a9d14f6a5", ResourceVersion: "v1:1:1"}}
	if errs := ValidateJob(job); len(errs) != 0 {
		t.Fatalf("multiple distinct scriptRefs rejected: %v", errs)
	}
	job.Spec.ScriptRefs[1].Name = "rpmbuild"
	if len(ValidateJob(job)) == 0 {
		t.Fatal("duplicate script names accepted")
	}
}

func TestValidateJobUpdate(t *testing.T) {
	old := validJob()
	errs := ValidateJobUpdate(old.DeepCopy(), old)
	assertErrorList(t, errs, 0, nil)
}

func TestValidateJobArtifactInstance(t *testing.T) {
	job := validJob()
	job.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}
	assertErrorList(t, ValidateJob(job), 0, nil)

	updated := job.DeepCopy()
	updated.Annotations[ebsv1.ProjectArtifactsAnnotation] = "art-02"
	assertErrorList(t, ValidateJobUpdate(updated, job), 1, map[string]field.ErrorType{
		"metadata.annotations[ebs.io/artifacts]": field.ErrorTypeForbidden,
	})
}

func TestValidateJobStatusUpdate(t *testing.T) {
	tests := []struct {
		name       string
		status     ebsv1.JobStatus
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{name: "pending", status: ebsv1.JobStatus{Phase: ebsv1.JobPending, Stage: ebsv1.JobStagePending}},
		{name: "running", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStageRunning}},
		{name: "post run", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun}},
		{name: "build and install results", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun, Build: &ebsv1.JobBuildResult{Status: ebsv1.JobResultSucceeded}, Install: &ebsv1.JobInstallResult{Status: ebsv1.JobResultFailed, MissingDeps: map[string]ebsv1.MissingDep{"dep": {}}}}},
		{name: "rejects install without build", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun, Install: &ebsv1.JobInstallResult{Status: ebsv1.JobResultSucceeded}}, wantErrs: 1, wantFields: map[string]field.ErrorType{"status.install": field.ErrorTypeForbidden}},
		{name: "rejects invalid build result", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun, Build: &ebsv1.JobBuildResult{Status: "Unknown"}}, wantErrs: 1, wantFields: map[string]field.ErrorType{"status.build.status": field.ErrorTypeNotSupported}},
		{
			name:     "rejects unsupported phase",
			status:   ebsv1.JobStatus{Phase: ebsv1.JobPhase("Unknown"), Stage: ebsv1.JobStagePending},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.phase": field.ErrorTypeNotSupported,
			},
		},
		{
			name:     "rejects failed stage",
			status:   ebsv1.JobStatus{Phase: ebsv1.JobFailed, Stage: ebsv1.JobStage("Failed")},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.stage": field.ErrorTypeNotSupported,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &ebsv1.Job{Status: tt.status}
			errs := ValidateJobStatusUpdate(job, validJob())
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func validJob() *ebsv1.Job {
	return &ebsv1.Job{Status: ebsv1.JobStatus{Phase: ebsv1.JobPending, Stage: ebsv1.JobStagePending}}
}
