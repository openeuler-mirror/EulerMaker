package rpmrepo

import (
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestSelectBatchKeepsOneJobPerSpecAndHonoursJobLimit(t *testing.T) {
	candidates := []candidate{
		{name: "job-d", uid: "4", specName: "glibc", createdAt: 4},
		{name: "job-b", uid: "2", specName: "gcc", createdAt: 2},
		{name: "job-c", uid: "3", specName: "kernel", createdAt: 3},
		{name: "job-a", uid: "1", specName: "gcc", createdAt: 1},
	}
	selection := selectBatch(candidates, 2)
	if len(selection) != 2 || selection[0].name != "job-a" || selection[1].name != "job-c" {
		t.Fatalf("unexpected two-job batch %+v", selection)
	}
	selection = selectBatch(candidates, 20)
	if len(selection) != 3 || selection[2].name != "job-d" {
		t.Fatalf("each spec should appear only once per batch: %+v", selection)
	}
}

func TestRepositoryUIDIsStableAndOrderIndependent(t *testing.T) {
	base := []ebsv1.RepositoryInput{{JobName: "job-a", JobUID: "uid-a", SpecName: "gcc"}, {JobName: "job-b", JobUID: "uid-b", SpecName: "kernel"}}
	first, err := repositoryUID("project", "build-a", "base-1", base)
	if err != nil {
		t.Fatalf("repositoryUID: %v", err)
	}
	reordered, err := repositoryUID("project", "build-a", "base-1", []ebsv1.RepositoryInput{base[1], base[0]})
	if err != nil {
		t.Fatalf("repositoryUID: %v", err)
	}
	if first != reordered {
		t.Fatalf("repositoryUID must not depend on input order: %s != %s", first, reordered)
	}
	changedUIDs := append([]ebsv1.RepositoryInput(nil), base...)
	changedUIDs[0].JobUID = "another-uid"
	withoutUID, err := repositoryUID("project", "build-a", "base-1", changedUIDs)
	if err != nil || withoutUID != first {
		t.Fatalf("repositoryUID must depend on Job names, not UIDs: %s, %v", withoutUID, err)
	}
	if len(first) != 64 || strings.ToLower(first) != first {
		t.Fatalf("repositoryUID must be a lowercase hex digest, got %q", first)
	}
	for _, changed := range []struct {
		project, buildName, base string
		inputs                   []ebsv1.RepositoryInput
	}{
		{"project-2", "build-a", "base-1", base},
		{"project", "build-b", "base-1", base},
		{"project", "build-a", "base-2", base},
		{"project", "build-a", "base-1", []ebsv1.RepositoryInput{base[0]}},
	} {
		other, err := repositoryUID(changed.project, changed.buildName, changed.base, changed.inputs)
		if err != nil {
			t.Fatalf("repositoryUID: %v", err)
		}
		if other == first {
			t.Fatalf("repositoryUID must change with project/build name/base/inputs")
		}
	}
	if _, err := repositoryUID("project", "build-a", "base-1", nil); err == nil {
		t.Fatalf("repositoryUID must reject an empty batch")
	}
}

func TestUnionSortedNamesDeduplicatesAndSorts(t *testing.T) {
	got := unionSortedNames([]string{"b", "a"}, []ebsv1.RepositoryInput{{JobName: "a"}, {JobName: "c"}, {JobName: ""}})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("unionSortedNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unionSortedNames = %v, want %v", got, want)
		}
	}
}

func TestRepositoryRequestsAreOrderedByJobName(t *testing.T) {
	manifests := repositoryRequests([]ebsv1.RepositoryInput{{JobName: "job-b", JobUID: "uid-b"}, {JobName: "job-a", JobUID: "uid-a"}})
	if len(manifests) != 2 || manifests[0].JobName != "job-a" || manifests[1].JobName != "job-b" {
		t.Fatalf("repositoryRequests must order manifests by Job name, got %+v", manifests)
	}
}
