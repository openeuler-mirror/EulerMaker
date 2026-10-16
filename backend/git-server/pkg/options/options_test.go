package options

import "testing"

func TestSmartHTTPOptions(t *testing.T) {
	defaults, err := Parse(nil)
	if err != nil || defaults.CloneBaseURL != "http://git-server:8080/git" || defaults.CloneWorkers != 20 {
		t.Fatalf("defaults = %#v, %v", defaults, err)
	}
	configured, err := Parse([]string{"--clone-base-url=https://git.example.com/git", "--clone-workers=8"})
	if err != nil || configured.CloneBaseURL != "https://git.example.com/git" || configured.CloneWorkers != 8 {
		t.Fatalf("configured = %#v, %v", configured, err)
	}
	for _, args := range [][]string{
		{"--clone-workers=0"},
		{"--clone-base-url=git://git-server:9418"},
		{"--clone-base-url=https://user:secret@git.example.com/git"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("invalid configuration accepted: %v", args)
		}
	}
}
