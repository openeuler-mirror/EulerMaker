package v1

import "testing"

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
	if project.Spec.SpecBranch != "master" {
		t.Fatalf("specBranch = %q", project.Spec.SpecBranch)
	}
}
