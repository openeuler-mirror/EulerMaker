package resource

import (
	"strings"
	"testing"
)

func TestResolveAliasesAndPaths(t *testing.T) {
	for _, alias := range []string{"job", "jobs", "Job", "JOB"} {
		definition, err := Resolve(alias)
		if err != nil || definition.Kind != "Job" {
			t.Fatalf("resolve %q: %#v %v", alias, definition, err)
		}
	}
	job, _ := Resolve("job")
	path, err := job.ObjectPath("project-a", "job-a")
	if err != nil || path != "/apis/ebs/v1/projects/project-a/jobs/job-a" {
		t.Fatalf("unexpected path %q: %v", path, err)
	}
	if _, err := job.CollectionPath(""); err == nil {
		t.Fatal("expected missing Project error")
	}
}

func TestReadManifestsInjectsProjectAndValidates(t *testing.T) {
	input := `apiVersion: ebs/v1
kind: Job
metadata:
  name: job-a
spec:
  priority: 10
---
apiVersion: ebs/v1
kind: Project
metadata:
  name: project-a
`
	manifests, err := ReadManifests("-", "project-a", true, strings.NewReader(input))
	if err != nil {
		t.Fatalf("read manifests: %v", err)
	}
	if len(manifests) != 2 || manifests[0].Project != "project-a" {
		t.Fatalf("unexpected manifests: %#v", manifests)
	}
	metadata := manifests[0].Object["metadata"].(map[string]any)
	if metadata["namespace"] != "project-a" {
		t.Fatalf("Project was not injected: %#v", metadata)
	}
}

func TestReadManifestsRejectsUnknownFieldAndNamespaceConflict(t *testing.T) {
	unknown := "apiVersion: ebs/v1\nkind: Job\nmetadata:\n  name: job-a\nspec:\n  unknown: true\n"
	if _, err := ReadManifests("-", "project-a", true, strings.NewReader(unknown)); err == nil {
		t.Fatal("expected unknown field error")
	}
	conflict := "apiVersion: ebs/v1\nkind: Job\nmetadata:\n  name: job-a\n  namespace: other\n"
	if _, err := ReadManifests("-", "project-a", false, strings.NewReader(conflict)); err == nil {
		t.Fatal("expected namespace conflict")
	}
}
