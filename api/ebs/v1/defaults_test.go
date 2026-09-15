package v1

import (
	"encoding/json"
	"testing"
)

func TestSetDefaultsProjectPackageRefs(t *testing.T) {
	for _, branch := range []GitRef{{}, {Type: GitRefBranch, Value: "openEuler-24.03-LTS-SP4"}, {Type: GitRefTag, Value: "v1"}} {
		for _, input := range []string{`{"name":"gcc"}`, `{"name":"gcc","ref":null}`, `{"name":"gcc","ref":{}}`} {
			t.Run(string(branch.Type)+branch.Value+input, func(t *testing.T) {
				var repo PackageRepo
				if err := json.Unmarshal([]byte(input), &repo); err != nil {
					t.Fatal(err)
				}
				project := &Project{Spec: ProjectSpec{DefaultRef: branch, PackageRepos: []PackageRepo{repo}}}
				SetDefaults_Project(project)
				want := GitRef{}
				if got := project.Spec.PackageRepos[0].Ref; got != want {
					t.Fatalf("ref=%+v, want %+v", got, want)
				}
				project.Spec.DefaultRef = GitRef{Type: GitRefBranch, Value: "another-branch"}
				SetDefaults_Project(project)
				if project.Spec.PackageRepos[0].Ref != want {
					t.Fatal("existing ref changed with project branch")
				}
			})
		}
	}
}

func TestSetDefaultsProjectPreservesExplicitAndPartialRefs(t *testing.T) {
	for _, ref := range []GitRef{
		{Type: GitRefBranch, Value: "main"},
		{Type: GitRefTag, Value: "v1"},
		{Type: GitRefCommit, Value: "0123456789012345678901234567890123456789"},
		{Type: GitRefBranch}, {Value: "main"}, {Type: "Invalid", Value: "main"},
	} {
		project := &Project{Spec: ProjectSpec{DefaultRef: GitRef{Type: GitRefBranch, Value: "master"}, PackageRepos: []PackageRepo{{Name: "gcc", Ref: ref}}}}
		SetDefaults_Project(project)
		if project.Spec.PackageRepos[0].Ref != ref {
			t.Fatalf("explicit ref %+v was changed", ref)
		}
	}
}

func TestSetDefaultsProjectDoesNotNormalizeName(t *testing.T) {
	project := &Project{}
	project.Name = "Invalid_Name"

	SetDefaults_Project(project)

	if project.Name != "Invalid_Name" {
		t.Fatalf("project name was rewritten to %q", project.Name)
	}
	if project.Spec.DisplayName != "Invalid_Name" {
		t.Fatalf("displayName = %q", project.Spec.DisplayName)
	}
	if project.Spec.DefaultRef != (GitRef{Type: GitRefBranch, Value: "master"}) {
		t.Fatalf("defaultRef = %+v", project.Spec.DefaultRef)
	}
}

func TestProjectDefaultRefJSON(t *testing.T) {
	for _, input := range []string{`{}`, `{"defaultRef":null}`, `{"defaultRef":{}}`, `{"defaultRef":{"type":"Tag","value":"v1"}}`} {
		var project Project
		if err := json.Unmarshal([]byte(input), &project.Spec); err != nil {
			t.Fatal(err)
		}
		want := project.Spec.DefaultRef
		if want == (GitRef{}) {
			want = GitRef{Type: GitRefBranch, Value: "master"}
		}
		SetDefaults_Project(&project)
		data, err := json.Marshal(project)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Project
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Spec.DefaultRef != want {
			t.Fatalf("defaultRef=%+v, want %+v", decoded.Spec.DefaultRef, want)
		}
	}
	var project Project
	if err := json.Unmarshal([]byte(`{"spec":{"defaultRef":"master"}}`), &project); err == nil {
		t.Fatal("legacy string defaultRef must be rejected")
	}
}
