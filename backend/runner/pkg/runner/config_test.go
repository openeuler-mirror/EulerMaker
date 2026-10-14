package runner

import (
	"runtime"
	"testing"
	"time"
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

func TestArtifactConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := LoadConfig([]string{"--machine-credential-file=/credential", "--name=runner-a"})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ArtifactMaxFileSize != 25<<30 || cfg.ArtifactMaxJobSize != 100<<30 || cfg.ArtifactMaxFiles != 10000 || cfg.ArtifactUploadConcurrency != 4 {
		t.Fatalf("unexpected artifact defaults: %#v", cfg)
	}
	if cfg.ArtifactFailedRetention != 24*time.Hour {
		t.Fatalf("failed retention = %s, want 24h", cfg.ArtifactFailedRetention)
	}
	if _, err := LoadConfig([]string{"--machine-credential-file=/credential", "--name=runner-a", "--artifact-upload-concurrency=0"}); err == nil {
		t.Fatal("expected invalid artifact concurrency")
	}
}

func TestLoadConfigRequiresMachineCredentialFile(t *testing.T) {
	_, err := LoadConfig([]string{"--machine-credential-file=", "--name=runner-a"})
	if err == nil {
		t.Fatalf("expected machine credential file error")
	}
}
