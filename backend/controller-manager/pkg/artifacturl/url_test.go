package artifacturl

import "testing"

func TestReferenceAndResolve(t *testing.T) {
	ref, err := Reference("/repositories/v1/repo-1/")
	if err != nil || ref != "artifact:///repositories/v1/repo-1/" {
		t.Fatalf("Reference() = %q, %v", ref, err)
	}
	for _, input := range []string{ref, "/repositories/v1/repo-1/"} {
		got, err := Resolve("http://artifact.example:8081", input)
		if err != nil || got != "http://artifact.example:8081/repositories/v1/repo-1/" {
			t.Errorf("Resolve(%q) = %q, %v", input, got, err)
		}
	}
	got, err := Resolve("", "https://repo.example/repositories/v1/repo-1/")
	if err != nil || got != "https://repo.example/repositories/v1/repo-1/" {
		t.Fatalf("absolute Resolve() = %q, %v", got, err)
	}
}

func TestRejectInvalidReference(t *testing.T) {
	for _, value := range []string{"repositories/v1/repo", "/other/repo", "/repositories/../secret", "/repositories/v1/repo?token=secret"} {
		if _, err := Reference(value); err == nil {
			t.Errorf("Reference(%q) unexpectedly succeeded", value)
		}
	}
	for _, value := range []string{"artifact://repositories/v1/repo", "artifact:///repositories/../secret", "ftp://example/repo"} {
		if _, err := Resolve("http://artifact.example", value); err == nil {
			t.Errorf("Resolve(%q) unexpectedly succeeded", value)
		}
	}
}
