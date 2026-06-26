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

func TestLoadConfigRequiresToken(t *testing.T) {
	_, err := LoadConfig([]string{"--token=", "--name=runner-a", "--arch=x86_64"})
	if err == nil {
		t.Fatalf("expected token error")
	}
}

func TestLoadConfigDoesNotReadTokenFromEnvironment(t *testing.T) {
	t.Setenv("EBS_TOKEN", "env-token")
	_, err := LoadConfig([]string{"--name=runner-a", "--arch=x86_64"})
	if err == nil {
		t.Fatalf("expected token error")
	}
}
