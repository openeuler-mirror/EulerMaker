package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
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
			name: "rejects reserved default name",
			project: &ebsv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec:       validProjectSpec(),
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"metadata.name": field.ErrorTypeForbidden,
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
				"spec.buildTargets[0].os":   field.ErrorTypeRequired,
				"spec.buildTargets[0].arch": field.ErrorTypeRequired,
			},
		},
		{
			name: "validates package refs",
			project: &ebsv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
				Spec: ebsv1.ProjectSpec{
					BuildTargets: []ebsv1.BuildTarget{validBuildTarget()},
					PackageRepos: []ebsv1.PackageRepo{{Name: "pkg-a", Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch}}},
				},
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.packageRepos[0].ref.value": field.ErrorTypeRequired,
			},
		},
		{
			name: "requires package repo name",
			project: &ebsv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
				Spec: ebsv1.ProjectSpec{
					BuildTargets: []ebsv1.BuildTarget{validBuildTarget()},
					PackageRepos: []ebsv1.PackageRepo{{
						Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"},
					}},
				},
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.packageRepos[0].name": field.ErrorTypeRequired,
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

func TestValidateProjectDefaultRef(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ref     ebsv1.GitRef
		invalid bool
	}{
		{name: "omitted"},
		{name: "branch", ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "release/main"}},
		{name: "tag", ref: ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1.0"}},
		{name: "commit", ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "0123456789012345678901234567890123456789"}, invalid: true},
		{name: "missing type", ref: ebsv1.GitRef{Value: "main"}, invalid: true},
		{name: "missing value", ref: ebsv1.GitRef{Type: ebsv1.GitRefTag}, invalid: true},
		{name: "unsafe ref", ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "--bad"}, invalid: true},
		{name: "unknown type", ref: ebsv1.GitRef{Type: "Other", Value: "main"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := validProject()
			obj.Spec.DefaultRef = tc.ref
			snapshot := &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{DefaultRef: tc.ref}}
			for _, errs := range []field.ErrorList{ValidateProject(obj), ValidateProjectUpdate(obj, validProject()), ValidateSnapshot(snapshot), ValidateSnapshotUpdate(snapshot, &ebsv1.Snapshot{})} {
				if (len(errs) > 0) != tc.invalid {
					t.Fatalf("errors=%v, want invalid=%v", errs, tc.invalid)
				}
				for _, err := range errs {
					if err.Field != "spec.defaultRef.type" && err.Field != "spec.defaultRef.value" {
						t.Fatalf("unexpected error: %v", err)
					}
				}
			}
		})
	}
}

func TestSnapshotAllowsEmptyPackageRef(t *testing.T) {
	obj := &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{
		DefaultRef:   ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1"},
		PackageRepos: []ebsv1.PackageRepo{{Name: "gcc"}},
	}}
	errs := ValidateSnapshot(obj)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors=%v", errs)
	}
	if obj.Spec.PackageRepos[0].Ref != (ebsv1.GitRef{}) {
		t.Fatal("validation mutated package ref")
	}
}

func TestValidateProjectUpdate(t *testing.T) {
	errs := ValidateProjectUpdate(&ebsv1.Project{}, validProject())
	assertErrorList(t, errs, 2, map[string]field.ErrorType{
		"metadata.name":     field.ErrorTypeRequired,
		"spec.buildTargets": field.ErrorTypeRequired,
	})
}

func TestValidateProjectUpdateRequiresPackageRepoName(t *testing.T) {
	newProject := validProject()
	newProject.Spec.PackageRepos = []ebsv1.PackageRepo{{
		Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"},
	}}
	err := ValidateProjectUpdate(newProject, validProject())
	assertErrorList(t, err, 1, map[string]field.ErrorType{
		"spec.packageRepos[0].name": field.ErrorTypeRequired,
	})
}

func TestValidateProjectType(t *testing.T) {
	for _, value := range []string{"personal", "community", "", "invalid"} {
		p := validProject()
		p.Labels = map[string]string{ebsv1.ProjectTypeLabel: value}
		valid := value == "personal" || value == "community"
		if errs := ValidateProject(p); (len(errs) == 0) != valid {
			t.Fatalf("create %q: %v", value, errs)
		}
		if errs := ValidateProjectUpdate(p, validProject()); (len(errs) == 0) != valid {
			t.Fatalf("update %q: %v", value, errs)
		}
	}
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
			name:     "valid with empty spec commits",
			snapshot: &ebsv1.Snapshot{},
		},
		{
			name: "valid package refs",
			snapshot: &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "openEuler-24.03-LTS"}},
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1.0.0"}},
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "0123456789abcdef0123456789abcdef01234567"}},
			}}},
		},
		{
			name:     "allows empty ref",
			snapshot: &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{{}}}},
		},
		{
			name: "rejects unsupported ref type",
			snapshot: &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{{
				Ref: ebsv1.GitRef{Type: "PullRequest", Value: "123"},
			}}}},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.packageRepos[0].ref.type": field.ErrorTypeNotSupported,
			},
		},
		{
			name: "rejects unsafe ref values",
			snapshot: &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "../main"}},
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "abc123"}},
			}}},
			wantErrs: 2,
			wantFields: map[string]field.ErrorType{
				"spec.packageRepos[0].ref.value": field.ErrorTypeInvalid,
				"spec.packageRepos[1].ref.value": field.ErrorTypeInvalid,
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
	assertErrorList(t, errs, 0, nil)
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

func TestTerminalBuildPhaseImmutable(t *testing.T) {
	for _, oldPhase := range []ebsv1.BuildPhase{ebsv1.BuildSuccess, ebsv1.BuildFailed, ebsv1.BuildAborted, ebsv1.BuildSkipped} {
		for _, phase := range ebsv1.BuildPhaseValues() {
			old := validBuild()
			old.Status.Phase = oldPhase
			next := old.DeepCopy()
			next.Status.Phase = ebsv1.BuildPhase(phase)
			next.Status.Repo = "updated"
			errs := ValidateBuildStatusUpdate(next, old)
			if (len(errs) == 0) != (next.Status.Phase == oldPhase) {
				t.Fatalf("%s -> %s: %v", oldPhase, phase, errs)
			}
		}
	}
}

func TestValidateBuildResource(t *testing.T) {
	tests := []struct {
		name       string
		object     *ebsv1.BuildResource
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{name: "valid project table with extensible architecture", object: validBuildResource("project-a", "riscv64")},
		{
			name: "allows multibuild package names",
			object: &ebsv1.BuildResource{ObjectMeta: metav1.ObjectMeta{Name: "project-a", Namespace: "project-a"}, Spec: ebsv1.BuildResourceSpec{
				Default:  validResourceRequirements(),
				Packages: map[string]ebsv1.PackageResourceConfig{"kernel:kernel-rt": {Default: validResourceRequirements()}},
			}},
		},
		{
			name: "allows table limits to default to requests",
			object: &ebsv1.BuildResource{ObjectMeta: metav1.ObjectMeta{Name: "project-a", Namespace: "project-a"}, Spec: ebsv1.BuildResourceSpec{
				Default:  ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "4", "memory": "8Gi"}},
				Packages: map[string]ebsv1.PackageResourceConfig{"gcc": {Default: ebsv1.ResourceRequirements{Requests: map[string]string{"memory": "12Gi"}}}},
			}},
		},
		{
			name: "allows package and architecture partial overrides",
			object: &ebsv1.BuildResource{ObjectMeta: metav1.ObjectMeta{Name: "project-a", Namespace: "project-a"}, Spec: ebsv1.BuildResourceSpec{
				Default: validResourceRequirements(),
				Packages: map[string]ebsv1.PackageResourceConfig{"gcc": {
					Default: ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "3"}},
					Arches:  map[string]ebsv1.ResourceRequirements{"riscv64": {Requests: map[string]string{"memory": "6Gi"}}},
				}},
			}},
		},
		{
			name: "valid bootstrap default with table default only",
			object: &ebsv1.BuildResource{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: ebsv1.BuildResourceSpec{
				Default: validResourceRequirements(), Packages: map[string]ebsv1.PackageResourceConfig{},
			}},
		},
		{
			name:     "requires identity and packages",
			object:   &ebsv1.BuildResource{},
			wantErrs: 4,
			wantFields: map[string]field.ErrorType{
				"metadata.name": field.ErrorTypeRequired, "metadata.namespace": field.ErrorTypeRequired,
				"spec.default": field.ErrorTypeRequired, "spec.packages": field.ErrorTypeRequired,
			},
		},
		{name: "allows a name different from project", object: func() *ebsv1.BuildResource {
			object := validBuildResource("project-a", "x86_64")
			object.Name = "custom-table"
			return object
		}()},
		{
			name:       "rejects invalid architecture",
			object:     validBuildResource("project-a", "RISC V"),
			wantErrs:   1,
			wantFields: map[string]field.ErrorType{"spec.packages[gcc].arches[RISC V]": field.ErrorTypeInvalid},
		},
		{
			name: "rejects invalid partial request",
			object: &ebsv1.BuildResource{ObjectMeta: metav1.ObjectMeta{Name: "project-a", Namespace: "project-a"}, Spec: ebsv1.BuildResourceSpec{
				Default:  validResourceRequirements(),
				Packages: map[string]ebsv1.PackageResourceConfig{"gcc": {Default: ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "0"}}}},
			}},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.packages[gcc].default.requests[cpu]": field.ErrorTypeInvalid,
			},
		},
		{
			name: "rejects unknown resources and lower limits",
			object: &ebsv1.BuildResource{ObjectMeta: metav1.ObjectMeta{Name: "project-a", Namespace: "project-a"}, Spec: ebsv1.BuildResourceSpec{
				Default: validResourceRequirements(),
				Packages: map[string]ebsv1.PackageResourceConfig{"gcc": {Default: ebsv1.ResourceRequirements{
					Requests: map[string]string{"cpu": "4", "memory": "8Gi", "gpu": "1"},
					Limits:   map[string]string{"cpu": "2", "memory": "4Gi"},
				}}},
			}},
			wantErrs: 3,
			wantFields: map[string]field.ErrorType{
				"spec.packages[gcc].default.requests[gpu]":  field.ErrorTypeNotSupported,
				"spec.packages[gcc].default.limits[cpu]":    field.ErrorTypeInvalid,
				"spec.packages[gcc].default.limits[memory]": field.ErrorTypeInvalid,
			},
		},
	}
	tests[6].object.Name = "other"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorList(t, ValidateBuildResource(tt.object), tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateBuildResourceUpdate(t *testing.T) {
	object := validBuildResource("project-a", "aarch64")
	assertErrorList(t, ValidateBuildResourceUpdate(object, object.DeepCopy()), 0, nil)
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
	tests := []struct {
		name       string
		status     ebsv1.JobStatus
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{name: "pending", status: ebsv1.JobStatus{Phase: ebsv1.JobPending, Stage: ebsv1.JobStagePending}},
		{name: "running", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStageRunning}},
		{name: "post run", status: ebsv1.JobStatus{Phase: ebsv1.JobRunning, Stage: ebsv1.JobStagePostRun}},
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
		Status: ebsv1.SnapshotStatus{
			PackageRepoStatuses: map[string]ebsv1.PackageRepoStatus{
				"pkg-a": {
					CloneURL: "git://git-server:9418/gitee.com/src-openeuler/pkg-a.git",
					CommitID: "abc123",
				},
			},
		},
	}
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

func validBuildResource(project, arch string) *ebsv1.BuildResource {
	return &ebsv1.BuildResource{
		ObjectMeta: metav1.ObjectMeta{Name: project, Namespace: project},
		Spec: ebsv1.BuildResourceSpec{Default: validResourceRequirements(), Packages: map[string]ebsv1.PackageResourceConfig{
			"gcc": {Arches: map[string]ebsv1.ResourceRequirements{arch: validResourceRequirements()}},
		}},
	}
}

func validResourceRequirements() ebsv1.ResourceRequirements {
	return ebsv1.ResourceRequirements{
		Requests: map[string]string{"cpu": "4", "memory": "8Gi"},
		Limits:   map[string]string{"cpu": "8", "memory": "16Gi"},
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
