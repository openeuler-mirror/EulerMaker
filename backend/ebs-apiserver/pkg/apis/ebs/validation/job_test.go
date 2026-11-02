package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func TestValidateJob(t *testing.T) {
	errs := ValidateJob(validJob())
	assertErrorList(t, errs, 0, nil)
}

func TestValidateJobExecutionType(t *testing.T) {
	for _, value := range append(ebsv1.ExecutionTypeValues(), "") {
		job := validJob()
		job.Spec.Runtime = ebsv1.ExecutionType(value)
		if errs := ValidateJob(job); len(errs) != 0 {
			t.Fatalf("runtime %q: %v", value, errs)
		}
	}
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

func TestFrozenJobPreservesSnapshotAndArchiveFinalizer(t *testing.T) {
	job := validJob()
	job.Annotations = map[string]string{ebsv1.JobArchiveActionAnnotation: "retain"}
	now := metav1.Now()
	job.DeletionTimestamp = &now
	job.Status.Phase = ebsv1.JobSucceeded
	job.Finalizers = []string{ebsv1.JobArchiveFinalizer, "example.test/other"}
	for _, change := range []func(*ebsv1.Job){
		func(j *ebsv1.Job) { j.Spec.Priority++ },
		func(j *ebsv1.Job) { j.Labels = map[string]string{ebsv1.JobBuildNameLabel: "another"} },
		func(j *ebsv1.Job) { j.Annotations[ebsv1.JobArchiveActionAnnotation] = "delete" },
		func(j *ebsv1.Job) { j.Finalizers = []string{"example.test/other"} },
	} {
		next := job.DeepCopy()
		change(next)
		if errs := ValidateJobUpdate(next, job); len(errs) == 0 {
			t.Fatalf("frozen Job mutation accepted: %+v", next)
		}
	}
	next := job.DeepCopy()
	next.Finalizers = []string{ebsv1.JobArchiveFinalizer}
	if errs := ValidateJobUpdate(next, job); len(errs) != 0 {
		t.Fatalf("other finalizer cannot finish: %v", errs)
	}
}

func TestDeletingJobFreezesOnTerminalPhase(t *testing.T) {
	job := validJob()
	now := metav1.Now()
	job.DeletionTimestamp = &now
	job.Status.Phase = ebsv1.JobRunning
	job.Finalizers = []string{ebsv1.JobArchiveFinalizer}
	next := job.DeepCopy()
	next.Spec.Priority++
	if errs := ValidateJobUpdate(next, job); len(errs) != 0 {
		t.Fatalf("nonterminal Job was frozen: %v", errs)
	}
	next = job.DeepCopy()
	next.Status.Phase = ebsv1.JobAborted
	next.Status.EndTime = now
	if errs := ValidateJobStatusUpdate(next, job); len(errs) != 0 {
		t.Fatalf("deleting Job could not become terminal: %v", errs)
	}
	job = next
	next = job.DeepCopy()
	next.Spec.Priority++
	if errs := ValidateJobUpdate(next, job); len(errs) == 0 {
		t.Fatal("terminal deleting Job must be frozen without a version annotation")
	}
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
		{name: "install failed result", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun, Result: &ebsv1.JobResult{Status: ebsv1.JobResultInstallFailed, MissingDeps: map[string]ebsv1.MissingDep{"dep": {}}}}},
		{name: "missing dependencies require install failure", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun, Result: &ebsv1.JobResult{Status: ebsv1.JobResultSucceeded, MissingDeps: map[string]ebsv1.MissingDep{"dep": {}}}}, wantErrs: 1, wantFields: map[string]field.ErrorType{"status.result.missingDeps": field.ErrorTypeForbidden}},
		{name: "rejects invalid build result", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun, Result: &ebsv1.JobResult{Status: "Unknown"}}, wantErrs: 1, wantFields: map[string]field.ErrorType{"status.result.status": field.ErrorTypeNotSupported}},
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
