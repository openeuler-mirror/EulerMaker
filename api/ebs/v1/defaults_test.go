package v1

import (
	"encoding/json"
	"testing"
)

func TestProjectTypeDefault(t *testing.T) {
	for _, value := range []string{"missing", "personal", "community", "", "invalid"} {
		p := &Project{}
		want := value
		if value == "missing" {
			want = ProjectTypePersonal
		} else {
			p.Labels = map[string]string{ProjectTypeLabel: value}
		}
		SetDefaults_Project(p)
		if p.Labels[ProjectTypeLabel] != want {
			t.Fatalf("%q defaulted to %q", value, p.Labels[ProjectTypeLabel])
		}
	}
}

func TestSetDefaultsProjectPackageRefs(t *testing.T) {
	for _, branch := range []string{"", "refs/heads/openEuler-24.03-LTS-SP4", "refs/tags/v1"} {
		for _, input := range []string{`{"name":"gcc"}`, `{"name":"gcc","ref":null}`, `{"name":"gcc","ref":""}`} {
			t.Run(branch+input, func(t *testing.T) {
				var repo PackageRepo
				if err := json.Unmarshal([]byte(input), &repo); err != nil {
					t.Fatal(err)
				}
				project := &Project{Spec: ProjectSpec{DefaultRef: branch, PackageRepos: []PackageRepo{repo}}}
				SetDefaults_Project(project)
				want := ""
				if got := project.Spec.PackageRepos[0].Ref; got != want {
					t.Fatalf("ref=%+v, want %+v", got, want)
				}
				project.Spec.DefaultRef = "refs/heads/another-branch"
				SetDefaults_Project(project)
				if project.Spec.PackageRepos[0].Ref != want {
					t.Fatal("existing ref changed with project branch")
				}
			})
		}
	}
}

func TestSetDefaultsProjectPreservesExplicitRefs(t *testing.T) {
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", "refs/merge-requests/501/head"} {
		project := &Project{Spec: ProjectSpec{DefaultRef: "refs/heads/master", PackageRepos: []PackageRepo{{Name: "gcc", Ref: ref}}}}
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
	if project.Spec.DefaultRef != "refs/heads/master" {
		t.Fatalf("defaultRef = %+v", project.Spec.DefaultRef)
	}
}

func TestProjectDefaultRefJSON(t *testing.T) {
	for _, input := range []string{`{}`, `{"defaultRef":null}`, `{"defaultRef":""}`, `{"defaultRef":"refs/tags/v1"}`} {
		var project Project
		if err := json.Unmarshal([]byte(input), &project.Spec); err != nil {
			t.Fatal(err)
		}
		want := project.Spec.DefaultRef
		if want == "" {
			want = "refs/heads/master"
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
}
