package runner

import (
	"runtime"
	"testing"
)

func TestRuntimeArchMapsGoArch(t *testing.T) {
	got := runtimeArch()
	switch runtime.GOARCH {
	case "amd64":
		if got != "x86_64" {
			t.Fatalf("expected x86_64, got %s", got)
		}
	case "arm64":
		if got != "aarch64" {
			t.Fatalf("expected aarch64, got %s", got)
		}
	default:
		if got != runtime.GOARCH {
			t.Fatalf("expected %s, got %s", runtime.GOARCH, got)
		}
	}
}

func TestLoadConfigRequiresMachineCredentialFile(t *testing.T) {
	_, err := LoadConfig([]string{"--machine-credential-file=", "--name=runner-a"})
	if err == nil {
		t.Fatalf("expected machine credential file error")
	}
}
