package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

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

func TestValidateBuildEmptyPackagesByType(t *testing.T) {
	for _, buildType := range []string{"full", "incremental", "single", "specified"} {
		t.Run(buildType, func(t *testing.T) {
			b := validBuild()
			b.Spec.BuildType = buildType
			b.Labels[ebsv1.BuildTypeLabel] = buildType
			b.Spec.Packages = nil
			errs := ValidateBuild(b)
			if buildType == "full" || buildType == "incremental" {
				if len(errs) != 0 {
					t.Fatalf("empty packages rejected: %v", errs)
				}
			} else if len(errs) != 1 || errs[0].Field != "spec.packages" {
				t.Fatalf("expected packages error: %v", errs)
			}
		})
	}
}

func TestValidateBuild(t *testing.T) {
	tests := []struct {
		name       string
		build      *ebsv1.Build
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:  "valid",
			build: validBuild(),
		},
		{
			name:     "requires mandatory build fields",
			build:    &ebsv1.Build{},
			wantErrs: 8,
			wantFields: map[string]field.ErrorType{
				"metadata.name":                       field.ErrorTypeRequired,
				"spec.buildType":                      field.ErrorTypeRequired,
				"spec.packages":                       field.ErrorTypeRequired,
				"spec.buildTarget.os":                 field.ErrorTypeRequired,
				"spec.buildTarget.arch":               field.ErrorTypeRequired,
				"metadata.labels[ebs.io/target-os]":   field.ErrorTypeRequired,
				"metadata.labels[ebs.io/target-arch]": field.ErrorTypeRequired,
				"metadata.labels[ebs.io/build-type]":  field.ErrorTypeRequired,
			},
		},
		{
			name: "rejects labels inconsistent with build target",
			build: func() *ebsv1.Build {
				build := validBuild()
				build.Labels[ebsv1.BuildTargetOSLabel] = "another-os"
				build.Labels[ebsv1.BuildTargetArchLabel] = "aarch64"
				build.Labels[ebsv1.BuildTypeLabel] = "incremental"
				return build
			}(),
			wantErrs: 3,
			wantFields: map[string]field.ErrorType{
				"metadata.labels[ebs.io/target-os]":   field.ErrorTypeInvalid,
				"metadata.labels[ebs.io/target-arch]": field.ErrorTypeInvalid,
				"metadata.labels[ebs.io/build-type]":  field.ErrorTypeInvalid,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateBuild(tt.build)
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateBuildName(t *testing.T) {
	for _, name := range []string{
		"123e4567-e89b-42d3-a456-426614174000",
		"123e4567-e89b-12d3-a456-426614174000",
		"01900000-0000-7000-8000-000000000000",
	} {
		t.Run(name, func(t *testing.T) {
			b := validBuild()
			b.Name = name
			assertErrorList(t, ValidateBuild(b), 0, nil)
			assertErrorList(t, ValidateBuildUpdate(b.DeepCopy(), b), 0, nil)
		})
	}
	for _, name := range []string{
		"", "build-001", "123E4567-E89B-42D3-A456-426614174000",
		"123e4567e89b42d3a456426614174000",
		"{123e4567-e89b-42d3-a456-426614174000}",
		"urn:uuid:123e4567-e89b-42d3-a456-426614174000",
		"123e4567-e89b-42d3-a456-42661417400g",
		"123e4567-e89b-42d3-a456-426614174000-extra",
		"123e4567-e89b-42d3-a456-426614174000\n",
	} {
		t.Run("invalid/"+name, func(t *testing.T) {
			b := validBuild()
			b.Name = name
			errorType := field.ErrorTypeInvalid
			if name == "" {
				errorType = field.ErrorTypeRequired
			}
			want := map[string]field.ErrorType{"metadata.name": errorType}
			assertErrorList(t, ValidateBuild(b), 1, want)
			assertErrorList(t, ValidateBuildUpdate(b, validBuild()), 1, want)
		})
	}
}

func TestValidateBuildUpdate(t *testing.T) {
	errs := ValidateBuildUpdate(&ebsv1.Build{}, validBuild())
	assertErrorList(t, errs, 9, map[string]field.ErrorType{
		"metadata.name":                       field.ErrorTypeRequired,
		"spec":                                field.ErrorTypeInvalid,
		"spec.buildType":                      field.ErrorTypeRequired,
		"spec.packages":                       field.ErrorTypeRequired,
		"spec.buildTarget.os":                 field.ErrorTypeRequired,
		"spec.buildTarget.arch":               field.ErrorTypeRequired,
		"metadata.labels[ebs.io/target-os]":   field.ErrorTypeRequired,
		"metadata.labels[ebs.io/target-arch]": field.ErrorTypeRequired,
		"metadata.labels[ebs.io/build-type]":  field.ErrorTypeRequired,
	})

	oldBuild := validBuild()
	newBuild := oldBuild.DeepCopy()
	newBuild.Spec.Packages = append(newBuild.Spec.Packages, "pkg-b")
	errs = ValidateBuildUpdate(newBuild, oldBuild)
	assertErrorList(t, errs, 1, map[string]field.ErrorType{
		"spec": field.ErrorTypeInvalid,
	})

	errs = ValidateBuildUpdate(oldBuild.DeepCopy(), oldBuild)
	assertErrorList(t, errs, 0, nil)
}

func TestValidateBuildStatusUpdate(t *testing.T) {
	for _, phase := range []ebsv1.BuildPhase{
		ebsv1.BuildPending, ebsv1.BuildPrepared, ebsv1.BuildProcessing,
		ebsv1.BuildSuccess, ebsv1.BuildFailed, ebsv1.BuildAborted, ebsv1.BuildSkipped,
	} {
		t.Run(string(phase), func(t *testing.T) {
			errs := ValidateBuildStatusUpdate(&ebsv1.Build{Status: ebsv1.BuildStatus{Phase: phase}}, validBuild())
			assertErrorList(t, errs, 0, nil)
		})
	}

	errs := ValidateBuildStatusUpdate(&ebsv1.Build{Status: ebsv1.BuildStatus{Phase: ebsv1.BuildPhase("Aborting")}}, validBuild())
	assertErrorList(t, errs, 1, map[string]field.ErrorType{"status.phase": field.ErrorTypeNotSupported})
}

func TestIncrementalPackagesMayChangeOnlyWhilePending(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   ebsv1.BuildPhase
		wantErr bool
	}{
		{name: "pending", phase: ebsv1.BuildPending},
		{name: "prepared", phase: ebsv1.BuildPrepared, wantErr: true},
		{name: "processing", phase: ebsv1.BuildProcessing, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := validBuild()
			old.Spec.BuildType = "incremental"
			old.Labels[ebsv1.BuildTypeLabel] = "incremental"
			old.Status.Phase = tc.phase
			next := old.DeepCopy()
			next.Spec.Packages = []string{"changed"}
			errs := ValidateBuildUpdate(next, old)
			if tc.wantErr {
				assertErrorList(t, errs, 1, map[string]field.ErrorType{"spec": field.ErrorTypeInvalid})
			} else {
				assertErrorList(t, errs, 0, nil)
			}
		})
	}
	old := validBuild()
	old.Spec.BuildType = "incremental"
	old.Labels[ebsv1.BuildTypeLabel] = "incremental"
	next := old.DeepCopy()
	next.Spec.BuildTarget.Arch = "aarch64"
	next.Labels[ebsv1.BuildTargetArchLabel] = "aarch64"
	assertErrorList(t, ValidateBuildUpdate(next, old), 1, map[string]field.ErrorType{"spec": field.ErrorTypeInvalid})
}

func TestTerminalBuildPhaseImmutable(t *testing.T) {
	for _, oldPhase := range []ebsv1.BuildPhase{ebsv1.BuildSuccess, ebsv1.BuildFailed, ebsv1.BuildAborted, ebsv1.BuildSkipped} {
		for _, phase := range ebsv1.BuildPhaseValues() {
			old := validBuild()
			old.Status.Phase = oldPhase
			next := old.DeepCopy()
			next.Status.Phase = ebsv1.BuildPhase(phase)
			next.Status.Stage = "updated"
			errs := ValidateBuildStatusUpdate(next, old)
			if (len(errs) == 0) != (next.Status.Phase == oldPhase) {
				t.Fatalf("%s -> %s: %v", oldPhase, phase, errs)
			}
		}
	}
}

func TestValidateJob(t *testing.T) {
	errs := ValidateJob(validJob())
	assertErrorList(t, errs, 0, nil)
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

func TestValidateRunner(t *testing.T) {
	tests := []struct {
		name       string
		runner     *ebsv1.Runner
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:   "valid ct",
			runner: validRunner("ct", "x86_64"),
		},
		{
			name:   "valid vm",
			runner: validRunner("vm", "x86_64"),
		},
		{
			name:   "valid hw",
			runner: validRunner("hw", "x86_64"),
		},
		{
			name:     "requires name type arch and instance id",
			runner:   &ebsv1.Runner{},
			wantErrs: 4,
			wantFields: map[string]field.ErrorType{
				"metadata.name":   field.ErrorTypeRequired,
				"spec.type":       field.ErrorTypeRequired,
				"spec.arch":       field.ErrorTypeRequired,
				"spec.instanceId": field.ErrorTypeRequired,
			},
		},
		{
			name:     "rejects unsupported type",
			runner:   validRunner("container", "x86_64"),
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.type": field.ErrorTypeNotSupported,
			},
		},
		{
			name: "rejects labels that do not match spec",
			runner: func() *ebsv1.Runner {
				runner := validRunner("ct", "x86_64")
				runner.Labels["ebs.io/runner-type"] = "vm"
				runner.Labels["ebs.io/runner-arch"] = "aarch64"
				return runner
			}(),
			wantErrs: 2,
			wantFields: map[string]field.ErrorType{
				"metadata.labels[ebs.io/runner-type]": field.ErrorTypeInvalid,
				"metadata.labels[ebs.io/runner-arch]": field.ErrorTypeInvalid,
			},
		},
		{
			name: "rejects non-v4 instance id",
			runner: &ebsv1.Runner{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-a", Labels: map[string]string{"ebs.io/runner-type": "ct", "ebs.io/runner-arch": "x86_64"}},
				Spec: ebsv1.RunnerSpec{
					InstanceID: "5d65d05e-37b6-1e7b-bfcb-264930f4436b",
					Type:       "ct",
					Arch:       "x86_64",
				},
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.instanceId": field.ErrorTypeInvalid,
			},
		},
		{
			name: "rejects uppercase instance id",
			runner: &ebsv1.Runner{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-a", Labels: map[string]string{"ebs.io/runner-type": "ct", "ebs.io/runner-arch": "x86_64"}},
				Spec: ebsv1.RunnerSpec{
					InstanceID: "5D65D05E-37B6-4E7B-BFCB-264930F4436B",
					Type:       "ct",
					Arch:       "x86_64",
				},
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.instanceId": field.ErrorTypeInvalid,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateRunner(tt.runner)
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateRunnerUpdate(t *testing.T) {
	tests := []struct {
		name       string
		newRunner  *ebsv1.Runner
		oldRunner  *ebsv1.Runner
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:      "valid unchanged fields",
			newRunner: validRunner("ct", "x86_64"),
			oldRunner: validRunner("ct", "x86_64"),
		},
		{
			name:      "type and arch may change",
			newRunner: validRunner("vm", "aarch64"),
			oldRunner: validRunner("ct", "x86_64"),
		},
		{
			name: "instance id is immutable",
			newRunner: &ebsv1.Runner{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-a", Labels: map[string]string{"ebs.io/runner-type": "ct", "ebs.io/runner-arch": "x86_64"}},
				Spec: ebsv1.RunnerSpec{
					InstanceID: "dc12a241-34c4-45b0-92cf-58ab1234b9c2",
					Type:       "ct",
					Arch:       "x86_64",
				},
			},
			oldRunner: validRunner("ct", "x86_64"),
			wantErrs:  1,
			wantFields: map[string]field.ErrorType{
				"spec.instanceId": field.ErrorTypeInvalid,
			},
		},
		{
			name:      "also validates new object",
			newRunner: &ebsv1.Runner{},
			oldRunner: validRunner("ct", "x86_64"),
			wantErrs:  5,
			wantFields: map[string]field.ErrorType{
				"spec.type":       field.ErrorTypeRequired,
				"spec.arch":       field.ErrorTypeRequired,
				"spec.instanceId": field.ErrorTypeRequired,
				"metadata.name":   field.ErrorTypeRequired,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateRunnerUpdate(tt.newRunner, tt.oldRunner)
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateRunnerStatusUpdate(t *testing.T) {
	tests := []struct {
		name       string
		phase      ebsv1.RunnerPhase
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{name: "allows online", phase: ebsv1.RunnerOnline},
		{name: "allows offline", phase: ebsv1.RunnerOffline},
		{name: "allows evicted", phase: ebsv1.RunnerEvicted},
		{
			name:     "rejects empty phase",
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.phase": field.ErrorTypeRequired,
			},
		},
		{
			name:     "rejects legacy phase",
			phase:    ebsv1.RunnerPhase("Running"),
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.phase": field.ErrorTypeNotSupported,
			},
		},
		{
			name:     "rejects unsupported phase",
			phase:    ebsv1.RunnerPhase("Unknown"),
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.phase": field.ErrorTypeNotSupported,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newRunner := validRunner("ct", "x86_64")
			newRunner.Status.Phase = tt.phase
			errs := ValidateRunnerStatusUpdate(newRunner, validRunner("ct", "x86_64"))
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

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

func validBuild() *ebsv1.Build {
	return &ebsv1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "123e4567-e89b-42d3-a456-426614174000", Labels: map[string]string{
			ebsv1.BuildTargetOSLabel:   "openEuler-22.03-LTS",
			ebsv1.BuildTargetArchLabel: "x86_64",
			ebsv1.BuildTypeLabel:       "full",
		}},
		Spec: ebsv1.BuildSpec{
			BuildType:   "full",
			Packages:    []string{"pkg-a"},
			BuildTarget: validBuildTarget(),
		},
	}
}

func validJob() *ebsv1.Job {
	return &ebsv1.Job{Status: ebsv1.JobStatus{Phase: ebsv1.JobPending, Stage: ebsv1.JobStagePending}}
}

func validRunner(runnerType, arch string) *ebsv1.Runner {
	return &ebsv1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-a", Labels: map[string]string{
			"ebs.io/runner-type": runnerType,
			"ebs.io/runner-arch": arch,
		}},
		Spec: ebsv1.RunnerSpec{
			InstanceID: "5d65d05e-37b6-4e7b-bfcb-264930f4436b",
			Type:       runnerType,
			Arch:       arch,
		},
	}
}

func validBuildTarget() ebsv1.BuildTarget {
	return ebsv1.BuildTarget{
		Os:   "openEuler-22.03-LTS",
		Arch: "x86_64",
	}
}
