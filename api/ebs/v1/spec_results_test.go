package v1

import (
	"reflect"
	"testing"
)

func TestSpecResultRepo(t *testing.T) {
	for _, tc := range []struct {
		key, repo string
	}{
		{"gcc/gcc.spec", "gcc"},
		{"gcc/gcc-extra.spec", "gcc"},
		{"unreadable/", "unreadable"},
	} {
		if got := SpecResultRepo(tc.key); got != tc.repo {
			t.Errorf("repository for %q = %q, want %q", tc.key, got, tc.repo)
		}
	}
}

func TestSpecResultsFailedPackages(t *testing.T) {
	results := SpecResults{
		"gcc/gcc.spec":         {SpecName: "gcc", Status: SpecInstallFailed},
		"gcc/gcc-extra.spec":   {SpecName: "gcc-extra", Status: SpecBuildFailed},
		"bash/bash.spec":       {Status: SpecPrepareFailed},
		"missing/missing.spec": {SpecName: "missing", Status: SpecRpmDependsMissing},
		"unreadable/":          {Status: SpecBuildFailed},
		"arch/arch.spec":       {SpecName: "arch", Status: SpecArchUnsupported},
		"ok/ok.spec":           {SpecName: "ok", Status: SpecSucceeded},
		"pending/pending.spec": {Status: SpecPending},
		"ready/ready.spec":     {SpecName: "ready", Status: SpecPrepared},
		"running/run.spec":     {SpecName: "running", Status: SpecRunning},
	}
	want := []string{"arch", "bash", "gcc", "missing", "unreadable"}
	if got := results.FailedPackages(); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed repositories = %v, want sorted unique names %v", got, want)
	}
}

func TestSpecResultsEntries(t *testing.T) {
	parsed := SpecResult{SpecName: "actual-rpm-name", Status: SpecPrepared}
	results := SpecResults{
		"source/source.spec": parsed,
		"bad/bad.spec":       {SpecName: "failed-parse", Status: SpecPrepareFailed},
		"unknown/":           {Status: SpecPrepareFailed},
	}
	if got := results.Entries(); !reflect.DeepEqual(got, map[string]SpecResult{"actual-rpm-name": parsed}) {
		t.Fatalf("parsed entries = %+v", got)
	}
	if got, ok := results.Lookup(parsed.SpecName); !ok || !reflect.DeepEqual(got, parsed) {
		t.Fatalf("lookup = %+v, found=%t", got, ok)
	}
	if got := results.Entry(parsed.SpecName); !reflect.DeepEqual(got, parsed) {
		t.Fatalf("entry = %+v", got)
	}
	for _, name := range []string{"source", "failed-parse", "absent"} {
		if got, ok := results.Lookup(name); ok || !reflect.DeepEqual(got, SpecResult{}) {
			t.Errorf("lookup %q = %+v, found=%t", name, got, ok)
		}
	}
	if results.Len() != 3 {
		t.Fatalf("len = %d, want all source entries", results.Len())
	}
}

func TestSpecResultsSet(t *testing.T) {
	failed := SpecResult{SpecName: "failed-parse", Status: SpecPrepareFailed}
	results := SpecResults{
		"source/source.spec": {SpecName: "actual-rpm-name", Status: SpecPrepared},
		"bad/bad.spec":       failed,
	}
	results.Set("actual-rpm-name", SpecResult{Status: SpecRunning, DispatchCount: 1})
	want := SpecResult{SpecName: "actual-rpm-name", Status: SpecRunning, DispatchCount: 1}
	if got := results["source/source.spec"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("updated source entry = %+v, want %+v", got, want)
	}
	results.Set("failed-parse", SpecResult{Status: SpecRunning})
	results.Set("absent", SpecResult{Status: SpecRunning})
	if !reflect.DeepEqual(results, SpecResults{"source/source.spec": want, "bad/bad.spec": failed}) {
		t.Fatalf("Set changed unregistered or failed entries: %+v", results)
	}
}
