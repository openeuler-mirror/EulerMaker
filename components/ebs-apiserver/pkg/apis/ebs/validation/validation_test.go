package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
)

func TestValidateProject(t *testing.T) {
	tests := []struct {
		name       string
		project    *ebsv1.Project
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:    "valid",
			project: validProject(),
		},
		{
			name: "requires name",
			project: &ebsv1.Project{
				Spec: validProjectSpec(),
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"metadata.name": field.ErrorTypeRequired,
			},
		},
		{
			name: "requires dns1123 name",
			project: &ebsv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "Invalid_Name"},
				Spec:       validProjectSpec(),
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"metadata.name": field.ErrorTypeInvalid,
			},
		},
		{
			name: "requires build targets",
			project: &ebsv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.buildTargets": field.ErrorTypeRequired,
			},
		},
		{
			name: "requires build target os and arch",
			project: &ebsv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
				Spec: ebsv1.ProjectSpec{
					BuildTargets: []ebsv1.BuildTarget{{}},
				},
			},
			wantErrs: 2,
			wantFields: map[string]field.ErrorType{
				"spec.buildTargets[0].osVariant":    field.ErrorTypeRequired,
				"spec.buildTargets[0].architecture": field.ErrorTypeRequired,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateProject(tt.project)
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateProjectUpdate(t *testing.T) {
	errs := ValidateProjectUpdate(&ebsv1.Project{}, validProject())
	assertErrorList(t, errs, 2, map[string]field.ErrorType{
		"metadata.name":     field.ErrorTypeRequired,
		"spec.buildTargets": field.ErrorTypeRequired,
	})
}

func TestValidateProjectStatusUpdate(t *testing.T) {
	errs := ValidateProjectStatusUpdate(&ebsv1.Project{}, validProject())
	assertErrorList(t, errs, 0, nil)
}

func TestValidateSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   *ebsv1.Snapshot
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:     "valid",
			snapshot: validSnapshot(),
		},
		{
			name:     "requires spec commits and build targets",
			snapshot: &ebsv1.Snapshot{},
			wantErrs: 2,
			wantFields: map[string]field.ErrorType{
				"spec.specCommits":  field.ErrorTypeRequired,
				"spec.buildTargets": field.ErrorTypeRequired,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateSnapshot(tt.snapshot)
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateSnapshotUpdate(t *testing.T) {
	errs := ValidateSnapshotUpdate(&ebsv1.Snapshot{}, validSnapshot())
	assertErrorList(t, errs, 2, map[string]field.ErrorType{
		"spec.specCommits":  field.ErrorTypeRequired,
		"spec.buildTargets": field.ErrorTypeRequired,
	})
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
			name:     "requires snapshot name and build type",
			build:    &ebsv1.Build{},
			wantErrs: 2,
			wantFields: map[string]field.ErrorType{
				"spec.snapshotName": field.ErrorTypeRequired,
				"spec.buildType":    field.ErrorTypeRequired,
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

func TestValidateBuildUpdate(t *testing.T) {
	errs := ValidateBuildUpdate(&ebsv1.Build{}, validBuild())
	assertErrorList(t, errs, 2, map[string]field.ErrorType{
		"spec.snapshotName": field.ErrorTypeRequired,
		"spec.buildType":    field.ErrorTypeRequired,
	})
}

func TestValidateBuildStatusUpdate(t *testing.T) {
	errs := ValidateBuildStatusUpdate(&ebsv1.Build{}, validBuild())
	assertErrorList(t, errs, 0, nil)
}

func TestValidateJob(t *testing.T) {
	errs := ValidateJob(validJob())
	assertErrorList(t, errs, 0, nil)
}

func TestValidateJobUpdate(t *testing.T) {
	errs := ValidateJobUpdate(&ebsv1.Job{}, validJob())
	assertErrorList(t, errs, 0, nil)
}

func TestValidateJobStatusUpdate(t *testing.T) {
	errs := ValidateJobStatusUpdate(&ebsv1.Job{}, validJob())
	assertErrorList(t, errs, 0, nil)
}

func TestValidateRunner(t *testing.T) {
	tests := []struct {
		name       string
		runner     *ebsv1.Runner
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:   "valid dc",
			runner: validRunner("dc", "x86_64"),
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
			name:     "requires name type and arch",
			runner:   &ebsv1.Runner{},
			wantErrs: 3,
			wantFields: map[string]field.ErrorType{
				"metadata.name": field.ErrorTypeRequired,
				"spec.type":     field.ErrorTypeRequired,
				"spec.arch":     field.ErrorTypeRequired,
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
			name:      "valid unchanged immutable fields",
			newRunner: validRunner("dc", "x86_64"),
			oldRunner: validRunner("dc", "x86_64"),
		},
		{
			name:      "type and arch are immutable",
			newRunner: validRunner("vm", "aarch64"),
			oldRunner: validRunner("dc", "x86_64"),
			wantErrs:  2,
			wantFields: map[string]field.ErrorType{
				"spec.type": field.ErrorTypeForbidden,
				"spec.arch": field.ErrorTypeForbidden,
			},
		},
		{
			name:      "also validates new object",
			newRunner: &ebsv1.Runner{},
			oldRunner: validRunner("dc", "x86_64"),
			wantErrs:  5,
			wantFields: map[string]field.ErrorType{
				"spec.type":     field.ErrorTypeRequired,
				"spec.arch":     field.ErrorTypeRequired,
				"metadata.name": field.ErrorTypeRequired,
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
		phase      string
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{name: "allows empty phase"},
		{name: "allows registering", phase: "Registering"},
		{name: "allows booting", phase: "Booting"},
		{name: "allows running", phase: "Running"},
		{name: "allows idle", phase: "Idle"},
		{name: "allows offline", phase: "Offline"},
		{
			name:     "rejects unsupported phase",
			phase:    "Unknown",
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"status.phase": field.ErrorTypeNotSupported,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newRunner := validRunner("dc", "x86_64")
			newRunner.Status.Phase = tt.phase
			errs := ValidateRunnerStatusUpdate(newRunner, validRunner("dc", "x86_64"))
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

func validProject() *ebsv1.Project {
	return &ebsv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
		Spec:       validProjectSpec(),
	}
}

func validProjectSpec() ebsv1.ProjectSpec {
	return ebsv1.ProjectSpec{
		BuildTargets: []ebsv1.BuildTarget{validBuildTarget()},
	}
}

func validSnapshot() *ebsv1.Snapshot {
	return &ebsv1.Snapshot{
		Spec: ebsv1.SnapshotSpec{
			SpecCommits: map[string]ebsv1.SpecCommit{
				"pkg-a": {CommitId: "abc123"},
			},
			BuildTargets: []ebsv1.BuildTarget{validBuildTarget()},
		},
	}
}

func validBuild() *ebsv1.Build {
	return &ebsv1.Build{
		Spec: ebsv1.BuildSpec{
			SnapshotName: "snapshot-a",
			BuildType:    "full",
		},
	}
}

func validJob() *ebsv1.Job {
	return &ebsv1.Job{}
}

func validRunner(runnerType, arch string) *ebsv1.Runner {
	return &ebsv1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-a"},
		Spec: ebsv1.RunnerSpec{
			Type: runnerType,
			Arch: arch,
		},
	}
}

func validBuildTarget() ebsv1.BuildTarget {
	return ebsv1.BuildTarget{
		OsVariant:    "openEuler-22.03-LTS",
		Architecture: "x86_64",
	}
}
