package rpmrepo

import (
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestMaterializationInputBytesCountsOnlyRPMFiles(t *testing.T) {
	manifest := JobUploadManifest{
		State: ManifestCompleted,
		Files: []ManifestFile{
			{RelativePath: "packages/gcc-1.rpm", Size: 100},
			{RelativePath: "packages/gcc-debuginfo-1.RPM", Size: 50},
			{RelativePath: "logs/build.log", Size: 900},
			{RelativePath: "metadata/summary.json", Size: 10},
		},
	}
	if got := materializationInputBytes(manifest); got != 150 {
		t.Fatalf("materializationInputBytes = %d, want 150", got)
	}
}

func TestSelectBatchKeepsOneJobPerSpecAndHonoursLimits(t *testing.T) {
	candidates := []candidate{
		{name: "job-a", uid: "1", specName: "gcc", createdAt: 1, bytes: 10},
		{name: "job-b", uid: "2", specName: "gcc", createdAt: 2, bytes: 10},
		{name: "job-c", uid: "3", specName: "kernel", createdAt: 3, bytes: 10},
		{name: "job-d", uid: "4", specName: "glibc", createdAt: 4, bytes: 10},
	}
	selection := selectBatch(candidates, 3, 25)
	if selection.oversized != nil {
		t.Fatalf("unexpected oversized candidate %+v", selection.oversized)
	}
	if len(selection.inputs) != 2 {
		t.Fatalf("expected 2 inputs within the byte limit, got %d", len(selection.inputs))
	}
	if selection.inputs[0].name != "job-a" || selection.inputs[1].name != "job-c" {
		t.Fatalf("unexpected batch %+v", selection.inputs)
	}

	selection = selectBatch(candidates, 20, 100)
	if len(selection.inputs) != 3 {
		t.Fatalf("同一批次每个 spec 至多一个 Job，expected 3 inputs, got %d", len(selection.inputs))
	}
}

func TestSelectBatchReportsFirstCandidateOverLimit(t *testing.T) {
	candidates := []candidate{
		{name: "job-big", uid: "1", specName: "gcc", createdAt: 1, bytes: 150},
		{name: "job-small", uid: "2", specName: "kernel", createdAt: 2, bytes: 10},
	}
	selection := selectBatch(candidates, 20, 100)
	if selection.oversized == nil || selection.oversized.name != "job-big" {
		t.Fatalf("expected the first candidate to be reported oversized, got %+v", selection.oversized)
	}
	if len(selection.inputs) != 0 {
		t.Fatalf("an oversized first candidate must not form a batch, got %+v", selection.inputs)
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

func TestUnionSortedUIDsDeduplicatesAndSorts(t *testing.T) {
	got := unionSortedUIDs([]string{"b", "a"}, []ebsv1.RepositoryInput{{JobUID: "a"}, {JobUID: "c"}, {JobUID: ""}})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("unionSortedUIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unionSortedUIDs = %v, want %v", got, want)
		}
	}
}

func TestRepositoryRequestsAreOrderedByJobUID(t *testing.T) {
	manifests := repositoryRequests([]ebsv1.RepositoryInput{{JobName: "job-b", JobUID: "uid-b"}, {JobName: "job-a", JobUID: "uid-a"}})
	if len(manifests) != 2 || manifests[0].JobUID != "uid-a" || manifests[1].JobUID != "uid-b" {
		t.Fatalf("repositoryRequests must order manifests by Job UID, got %+v", manifests)
	}
}
