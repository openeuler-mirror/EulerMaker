package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func TestProjectBuildTargetUniqueness(t *testing.T) {
	tests := []struct {
		name    string
		targets []ebsv1.BuildTarget
		wantErr bool
	}{
		{"duplicate", []ebsv1.BuildTarget{{Os: "os-a", Arch: "x86_64"}, {Os: "os-a", Arch: "x86_64"}}, true},
		{"different options still duplicate", []ebsv1.BuildTarget{{Os: "os-a", Arch: "x86_64"}, {Os: "os-a", Arch: "x86_64", PublishFlag: true}}, true},
		{"different arch", []ebsv1.BuildTarget{{Os: "os-a", Arch: "x86_64"}, {Os: "os-a", Arch: "aarch64"}}, false},
		{"different os", []ebsv1.BuildTarget{{Os: "os-a", Arch: "x86_64"}, {Os: "os-b", Arch: "x86_64"}}, false},
		{"distinct pairs", []ebsv1.BuildTarget{{Os: "a-b", Arch: "c"}, {Os: "a", Arch: "b-c"}}, false},
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
			{Name: "a", URL: "https://example.com/a.git", Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "main"}},
			{Name: "a", URL: "https://example.com/b.git", Ref: ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1"}},
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
