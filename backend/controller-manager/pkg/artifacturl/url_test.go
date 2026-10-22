package artifacturl

import (
	"fmt"
	"testing"
)

func TestReferenceAndResolve(t *testing.T) {
	ref, err := Reference("/repositories/v1/repo-1/")
	if err != nil || ref != "artifact:///repositories/v1/repo-1/" {
		t.Fatalf("Reference() = %q, %v", ref, err)
	}
	named, err := ReferenceForInstance("art-01", "/repositories/v1/repo-1/")
	if err != nil || named != "artifact://art-01/repositories/v1/repo-1/" {
		t.Fatalf("ReferenceForInstance() = %q, %v", named, err)
	}
	addresses := map[string]string{"default": "http://artifact.example:8081", "art-01": "http://art-01:8081"}
	resolve := func(instance string) (string, error) {
		address, ok := addresses[instance]
		if !ok {
			return "", fmt.Errorf("unknown instance %q", instance)
		}
		return address, nil
	}
	for _, input := range []string{ref, "/repositories/v1/repo-1/"} {
		got, err := Resolve(resolve, input)
		if err != nil || got != "http://artifact.example:8081/repositories/v1/repo-1/" {
			t.Errorf("Resolve(%q) = %q, %v", input, got, err)
		}
	}
	got, err := Resolve(resolve, "artifact://art-01/repositories/v1/repo-1/")
	if err != nil || got != "http://art-01:8081/repositories/v1/repo-1/" {
		t.Fatalf("instance Resolve() = %q, %v", got, err)
	}
	got, err = Resolve(nil, "https://repo.example/repositories/v1/repo-1/")
	if err != nil || got != "https://repo.example/repositories/v1/repo-1/" {
		t.Fatalf("absolute Resolve() = %q, %v", got, err)
	}
}

func TestResolvePreservesManagementPrefix(t *testing.T) {
	got, err := Resolve(func(string) (string, error) {
		return "https://internal.example/management/art%2001/", nil
	}, "artifact://art-01/repositories/project/openEuler%3A24/x86_64/")
	want := "https://internal.example/management/art%2001/repositories/project/openEuler%3A24/x86_64/"
	if err != nil || got != want {
		t.Fatalf("Resolve()=%q, %v; want %q", got, err, want)
	}
}

func TestRejectInvalidReference(t *testing.T) {
	for _, value := range []string{"repositories/v1/repo", "/other/repo", "/repositories/../secret", "/repositories/v1/repo?token=secret"} {
		if _, err := Reference(value); err == nil {
			t.Errorf("Reference(%q) unexpectedly succeeded", value)
		}
	}
	for _, value := range []string{"artifact://repositories/v1/repo", "artifact:///repositories/../secret", "artifact://art-02/repositories/v1/repo/", "ftp://example/repo"} {
		if _, err := Resolve(func(instance string) (string, error) {
			if instance != "default" {
				return "", fmt.Errorf("unknown instance")
			}
			return "http://artifact.example", nil
		}, value); err == nil {
			t.Errorf("Resolve(%q) unexpectedly succeeded", value)
		}
	}
}
