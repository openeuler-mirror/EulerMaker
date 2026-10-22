package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func TestValidateBuildTarget(t *testing.T) {
	content := &ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{
		"os": {Arches: map[string]ebsv1.BuildTargetArch{"arch": {}}},
	}}
	for _, tc := range []struct {
		os, arch   string
		wantErrors int
	}{
		{"os", "arch", 0},
		{"other", "arch", 1},
		{"os", "other", 1},
	} {
		t.Run(tc.os+"/"+tc.arch, func(t *testing.T) {
			build := &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildTarget: ebsv1.BuildTarget{Os: tc.os, Arch: tc.arch}}}
			errs := ValidateBuildTarget(build, content)
			if len(errs) != tc.wantErrors {
				t.Fatalf("errors = %v, want %d", errs, tc.wantErrors)
			}
			if len(errs) > 0 && errs[0].Field != "spec.buildTarget" {
				t.Fatalf("field = %q", errs[0].Field)
			}
		})
	}
}

func TestValidateBuildPackages(t *testing.T) {
	project := &ebsv1.Project{Spec: ebsv1.ProjectSpec{PackageRepos: []ebsv1.PackageRepo{{Name: "gcc"}, {Name: "bash"}}}}
	build := &ebsv1.Build{Spec: ebsv1.BuildSpec{Packages: []string{"gcc", "bash", "gcc"}}}
	if errs := ValidateBuildPackages(build, project); len(errs) != 0 {
		t.Fatalf("configured packages: %v", errs)
	}
	build.Spec.Packages = append(build.Spec.Packages, "missing", "")
	errs := ValidateBuildPackages(build, project)
	if len(errs) != 2 || errs[0].Field != "spec.packages[3]" || errs[1].Field != "spec.packages[4]" {
		t.Fatalf("package errors = %v", errs)
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

func TestValidateBuildGitRef(t *testing.T) {
	for _, ref := range []string{
		"refs/merge-requests/501/head", "refs/heads/release/v1", "refs/tags/v1.0", "refs/heads/中文",
	} {
		t.Run(ref, func(t *testing.T) {
			b := validBuild()
			b.Spec.BuildType = "single"
			b.Labels[ebsv1.BuildTypeLabel] = "single"
			b.Annotations = map[string]string{ebsv1.BuildGitRefAnnotation: ref}
			assertErrorList(t, ValidateBuild(b), 0, nil)
		})
	}
	for _, ref := range []string{
		"", "HEAD", "FETCH_HEAD", "main", "--upload-pack=bad", " refs/heads/main", "refs/heads/main ",
		"refs/", "refs//main", "refs/heads/main/", "refs/heads/.hidden", "refs/heads/foo.lock/bar",
		"refs/heads/main.lock", "refs/heads/main.", "refs/heads/a..b", "refs/heads/main@{1}",
		"refs/heads/main:refs/heads/other", "refs/heads/*", "refs/heads/a?", "refs/heads/a[0]",
		"refs/heads/main^{commit}", "refs/heads/main~1", "refs/heads/a\\b", "refs/heads/a\nb", "refs/heads/a\x7fb",
	} {
		t.Run(ref, func(t *testing.T) {
			b := validBuild()
			b.Spec.BuildType = "single"
			b.Labels[ebsv1.BuildTypeLabel] = "single"
			b.Annotations = map[string]string{ebsv1.BuildGitRefAnnotation: ref}
			errs := ValidateBuild(b)
			path := field.NewPath("metadata", "annotations").Key(ebsv1.BuildGitRefAnnotation).String()
			if len(errs) != 1 || errs[0].Field != path {
				t.Fatalf("errors = %v, want one error at %s", errs, path)
			}
		})
	}
}

func TestBuildGitRefScope(t *testing.T) {
	for _, typ := range []string{"full", "incremental", "specified", "single"} {
		for _, packages := range [][]string{nil, {"gcc"}, {"gcc", "gcc"}, {"gcc", "bash"}} {
			b := validBuild()
			b.Spec.BuildType, b.Spec.Packages = typ, packages
			b.Labels[ebsv1.BuildTypeLabel] = typ
			b.Annotations = map[string]string{ebsv1.BuildGitRefAnnotation: "refs/merge-requests/501/head"}
			errs := ValidateBuild(b)
			wantValid := typ == "single" && len(packages) == 1
			if (len(errs) == 0) != wantValid {
				t.Fatalf("type=%s packages=%v errors=%v", typ, packages, errs)
			}
			delete(b.Annotations, ebsv1.BuildGitRefAnnotation)
			if typ == "single" && len(packages) > 0 {
				assertErrorList(t, ValidateBuild(b), 0, nil)
			}
		}
	}
}

func TestBuildGitRefImmutable(t *testing.T) {
	for _, tc := range []struct {
		name, oldRef, newRef string
		valid                bool
	}{
		{name: "unchanged", oldRef: "refs/heads/main", newRef: "refs/heads/main", valid: true},
		{name: "absent", valid: true},
		{name: "add", newRef: "refs/heads/main"},
		{name: "remove", oldRef: "refs/heads/main"},
		{name: "change", oldRef: "refs/heads/main", newRef: "refs/heads/next"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := validBuild()
			old.Spec.BuildType = "single"
			old.Labels[ebsv1.BuildTypeLabel] = "single"
			old.Annotations = map[string]string{}
			if tc.oldRef != "" {
				old.Annotations[ebsv1.BuildGitRefAnnotation] = tc.oldRef
			}
			next := old.DeepCopy()
			delete(next.Annotations, ebsv1.BuildGitRefAnnotation)
			if tc.newRef != "" {
				next.Annotations[ebsv1.BuildGitRefAnnotation] = tc.newRef
			}
			next.Annotations["example.com/note"] = "editable"
			errs := ValidateBuildUpdate(next, old)
			if tc.valid {
				assertErrorList(t, errs, 0, nil)
			} else {
				path := field.NewPath("metadata", "annotations").Key(ebsv1.BuildGitRefAnnotation).String()
				assertErrorList(t, errs, 1, map[string]field.ErrorType{path: field.ErrorTypeInvalid})
			}
		})
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
