package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func TestPackageRepoCommitID(t *testing.T) {
	for _, tc := range []struct {
		commitID string
		valid    bool
	}{
		{"", true},
		{"0123456789abcdef0123456789abcdef01234567", true},
		{"abc123", false},
		{"refs/heads/main", false},
	} {
		project := validProject()
		project.Spec.PackageRepos = []ebsv1.PackageRepo{{Name: "gcc", CommitID: tc.commitID}}
		snapshot := &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: project.Spec.PackageRepos}}
		for operation, errs := range map[string]field.ErrorList{
			"project create":  ValidateProject(project),
			"project update":  ValidateProjectUpdate(project, validProject()),
			"snapshot create": ValidateSnapshot(snapshot),
			"snapshot update": ValidateSnapshotUpdate(snapshot, &ebsv1.Snapshot{}),
		} {
			if tc.valid {
				assertErrorList(t, errs, 0, nil)
			} else if len(errs) != 1 || errs[0].Field != "spec.packageRepos[0].commitID" {
				t.Fatalf("%s commitID=%q errors=%v", operation, tc.commitID, errs)
			}
		}
	}
}

func TestProjectBuildTargetUniqueness(t *testing.T) {
	tests := []struct {
		name    string
		targets []ebsv1.BuildTarget
		wantErr bool
	}{
		{"duplicate", []ebsv1.BuildTarget{{OS: "os-a", Arch: "x86_64"}, {OS: "os-a", Arch: "x86_64"}}, true},
		{"different arch", []ebsv1.BuildTarget{{OS: "os-a", Arch: "x86_64"}, {OS: "os-a", Arch: "aarch64"}}, false},
		{"different os", []ebsv1.BuildTarget{{OS: "os-a", Arch: "x86_64"}, {OS: "os-b", Arch: "x86_64"}}, false},
		{"distinct pairs", []ebsv1.BuildTarget{{OS: "a-b", Arch: "c"}, {OS: "a", Arch: "b-c"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := validProject()
			project.Spec.BuildTargets = tt.targets
			for operation, errs := range map[string]field.ErrorList{
				"create": ValidateProject(project),
				"update": ValidateProjectUpdate(project, validProject()),
			} {
				if !tt.wantErr {
					if len(errs) != 0 {
						t.Fatalf("%s: unexpected errors: %v", operation, errs)
					}
					continue
				}
				if len(errs) != 1 || errs[0].Type != field.ErrorTypeDuplicate || errs[0].Field != "spec.buildTargets[1]" {
					t.Fatalf("%s: expected duplicate target error, got %v", operation, errs)
				}
			}
		})
	}
}

func TestProjectPackageRepoNameUniqueness(t *testing.T) {
	tests := []struct {
		name       string
		repos      []ebsv1.PackageRepo
		wantFields map[string]field.ErrorType
	}{
		{name: "empty list"},
		{name: "different names with same URL", repos: []ebsv1.PackageRepo{{Name: "a", URL: "https://example.com/repo.git"}, {Name: "b", URL: "https://example.com/repo.git"}}},
		{name: "duplicate with empty refs", repos: []ebsv1.PackageRepo{{Name: "a"}, {Name: "a"}}, wantFields: map[string]field.ErrorType{"spec.packageRepos[1].name": field.ErrorTypeDuplicate}},
		{name: "duplicate with different URL and ref", repos: []ebsv1.PackageRepo{
			{Name: "a", URL: "https://example.com/a.git", Ref: "refs/heads/main"},
			{Name: "a", URL: "https://example.com/b.git", Ref: "refs/tags/v1"},
		}, wantFields: map[string]field.ErrorType{"spec.packageRepos[1].name": field.ErrorTypeDuplicate}},
		{name: "nonadjacent duplicates", repos: []ebsv1.PackageRepo{{Name: "a"}, {Name: "b"}, {Name: "a"}, {Name: "a"}}, wantFields: map[string]field.ErrorType{"spec.packageRepos[2].name": field.ErrorTypeDuplicate, "spec.packageRepos[3].name": field.ErrorTypeDuplicate}},
		{name: "empty names remain required", repos: []ebsv1.PackageRepo{{}, {}}, wantFields: map[string]field.ErrorType{"spec.packageRepos[0].name": field.ErrorTypeRequired, "spec.packageRepos[1].name": field.ErrorTypeRequired}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := validProject()
			project.Spec.PackageRepos = tt.repos
			for operation, errs := range map[string]field.ErrorList{
				"create": ValidateProject(project),
				"update": ValidateProjectUpdate(project, validProject()),
			} {
				if len(errs) != len(tt.wantFields) {
					t.Fatalf("%s: unexpected errors: %v", operation, errs)
				}
				for _, err := range errs {
					if want, ok := tt.wantFields[err.Field]; !ok || err.Type != want {
						t.Errorf("%s: unexpected error: %v", operation, err)
					}
				}
			}
		})
	}
}

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
					PackageRepos: []ebsv1.PackageRepo{{Name: "pkg-a", Ref: "refs/heads/"}},
				},
			},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.packageRepos[0].ref": field.ErrorTypeInvalid,
			},
		},
		{
			name: "requires package repo name",
			project: &ebsv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
				Spec: ebsv1.ProjectSpec{
					BuildTargets: []ebsv1.BuildTarget{validBuildTarget()},
					PackageRepos: []ebsv1.PackageRepo{{
						Ref: "refs/heads/main",
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
		ref     string
		invalid bool
	}{
		{name: "omitted"},
		{name: "branch", ref: "refs/heads/release/main"},
		{name: "tag", ref: "refs/tags/v1.0"},
		{name: "PR", ref: "refs/merge-requests/501/head"},
		{name: "commit", ref: "0123456789012345678901234567890123456789", invalid: true},
		{name: "bare branch", ref: "main", invalid: true},
		{name: "empty component", ref: "refs/tags/", invalid: true},
		{name: "unsafe ref", ref: "refs/heads/bad..branch", invalid: true},
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
					if err.Field != "spec.defaultRef" {
						t.Fatalf("unexpected error: %v", err)
					}
				}
			}
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

func TestValidateProjectUpdateRequiresPackageRepoName(t *testing.T) {
	newProject := validProject()
	newProject.Spec.PackageRepos = []ebsv1.PackageRepo{{
		Ref: "refs/heads/main",
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

func validProject() *ebsv1.Project {
	return &ebsv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project-a"},
		Spec:       validProjectSpec(),
	}
}

func TestValidateProjectArtifactInstance(t *testing.T) {
	content := &ebsv1.ServiceConfigContent{ArtifactManagers: map[string]ebsv1.ServiceEndpoints{
		"default": {},
		"art-01":  {},
		"art-x":   {},
	}}
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		wantErrors  int
	}{
		{"default", nil, 0},
		{"named instance", map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}, 0},
		{"nonnumeric instance", map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-x"}, 0},
		{"missing instance", map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-02"}, 1},
		{"explicit empty instance", map[string]string{ebsv1.ProjectArtifactsAnnotation: ""}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			errs := ValidateProjectArtifactInstance(project, content)
			if len(errs) != tc.wantErrors {
				t.Fatalf("errors = %v, want %d", errs, tc.wantErrors)
			}
			if len(errs) > 0 && errs[0].Field != "metadata.annotations[ebs.io/artifacts]" {
				t.Fatalf("field = %q", errs[0].Field)
			}
		})
	}
}

func validProjectSpec() ebsv1.ProjectSpec {
	return ebsv1.ProjectSpec{
		BuildTargets: []ebsv1.BuildTarget{validBuildTarget()},
	}
}
