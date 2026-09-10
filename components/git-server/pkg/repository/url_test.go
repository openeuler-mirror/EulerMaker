package repository

import "testing"

func TestParseURL(t *testing.T) {
	tests := []struct {
		input string
		key   string
	}{
		{"https://gitee.com/src-openeuler/gcc.git", "gitee.com/src-openeuler/gcc.git"},
		{"git@gitee.com:src-openeuler/gcc.git", "gitee.com/src-openeuler/gcc.git"},
		{"ssh://git@gitee.com:22/src-openeuler/gcc", "gitee.com/src-openeuler/gcc.git"},
		{"https://gitee.com:8443/team/repo.git", "gitee.com~8443/team/repo.git"},
		{"https://git.example.com/team/a%20b.git", "git.example.com/team/a%20b.git"},
	}
	for _, tt := range tests {
		got, err := ParseURL(tt.input)
		if err != nil {
			t.Fatalf("ParseURL(%q): %v", tt.input, err)
		}
		if got.Key != tt.key {
			t.Errorf("ParseURL(%q).Key=%q, want %q", tt.input, got.Key, tt.key)
		}
	}
}

func TestParseURLRejectsUnsafeValues(t *testing.T) {
	for _, input := range []string{
		"file:///srv/repo.git",
		"https://user:password@gitee.com/team/repo.git",
		"https://gitee.com/team/%2E%2E/repo.git",
		"https://gitee.com/team/../repo.git",
		"https://gitee.com/team/./repo.git",
		"https://gitee.com/team/%2F/repo.git",
		"https://gitee.com/repo.git?token=x",
	} {
		if _, err := ParseURL(input); err == nil {
			t.Errorf("ParseURL(%q) unexpectedly succeeded", input)
		}
	}
}
