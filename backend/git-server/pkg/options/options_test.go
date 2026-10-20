package options

import "testing"

func TestDataDirOptions(t *testing.T) {
	for _, dataDir := range []string{"", "/tmp/git-repositories"} {
		var args []string
		want := "/var/lib/ebs-git"
		if dataDir != "" {
			args = []string{"--data-dir=" + dataDir}
			want = dataDir
		}
		got, err := Parse(args)
		if err != nil || got.DataDir != want {
			t.Fatalf("Parse(%v) dataDir = %q, error = %v; want %q", args, got.DataDir, err, want)
		}
	}
}

func TestSmartHTTPOptions(t *testing.T) {
	defaults, err := Parse(nil)
	if err != nil || defaults.CloneBaseURL != "http://git-server:8080/git" || defaults.CloneWorkers != 100 {
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
