package gitserver

import "testing"

func TestRepositoryKey(t *testing.T) {
	for _, raw := range []string{
		"https://EXAMPLE.com.:443/team/repo",
		"http://example.com:80/team/repo.git/",
		"git@example.com:team/repo.git",
		"ssh://user@example.com:22/team/repo.git",
		"git://example.com:9418/team//%72epo.git",
	} {
		key, err := RepositoryKey(raw)
		if err != nil || key != "example.com/team/repo.git" {
			t.Fatalf("%q: %q %v", raw, key, err)
		}
	}
	for _, raw := range []string{"https://example.com:8443/team/repo.git", "https://example.com/Team/repo.git"} {
		key, err := RepositoryKey(raw)
		if err != nil || key == "example.com/team/repo.git" {
			t.Fatalf("distinct identity %q: %q %v", raw, key, err)
		}
	}
	for _, raw := range []string{"https://user:secret@example.com/repo", "https://example.com/team/%2Fetc", "https://example.com/../repo", "file:///tmp/repo"} {
		if _, err := RepositoryKey(raw); err == nil {
			t.Fatalf("accepted invalid URL %q", raw)
		}
	}
}
