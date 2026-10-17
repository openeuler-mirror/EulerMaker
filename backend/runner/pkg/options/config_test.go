package options

import (
	"reflect"
	"runtime"
	"testing"
	"time"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
)

func TestBusinessConfigFlagBindings(t *testing.T) {
	cfg, err := LoadConfig([]string{
		"--name=runner-test", "--machine-credential-file=/etc/ebs/credential.json",
		"--gateway=https://gateway.example", "--gateway-ca=/etc/ebs/gateway.crt", "--insecure-skip-verify",
		"--artifact-access-mode=external", "--artifact-manager-ca=/etc/ebs/artifact.crt",
		"--artifact-manager-insecure-skip-verify",
		"--log-chunk-size=1024", "--log-flush-interval=2s", "--log-spool-limit=4096",
		"--log-drain-timeout=12s", "--log-retry-max-backoff=8s",
		"--artifact-max-file-size=2048", "--artifact-max-job-size=8192", "--artifact-max-files=12",
		"--artifact-upload-concurrency=3", "--artifact-upload-timeout=3m",
		"--artifact-retry-max-backoff=15s", "--artifact-failed-retention=2h",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantGateway := gateway.Config{
		URL:                   "https://gateway.example",
		CA:                    "/etc/ebs/gateway.crt",
		InsecureSkipVerify:    true,
		MachineCredentialFile: "/etc/ebs/credential.json",
	}
	if !reflect.DeepEqual(cfg.Gateway, wantGateway) {
		t.Fatalf("gateway config = %+v, want %+v", cfg.Gateway, wantGateway)
	}
	wantArtifact := artifact.Config{
		AccessMode:         "external",
		CA:                 "/etc/ebs/artifact.crt",
		InsecureSkipVerify: true,
		Log: artifact.LogConfig{
			ChunkSize:       1024,
			FlushInterval:   2 * time.Second,
			SpoolLimit:      4096,
			DrainTimeout:    12 * time.Second,
			RetryMaxBackoff: 8 * time.Second,
		},
		Upload: artifact.UploadConfig{
			MaxFileSize:     2048,
			MaxJobSize:      8192,
			MaxFiles:        12,
			Concurrency:     3,
			Timeout:         3 * time.Minute,
			RetryMaxBackoff: 15 * time.Second,
			FailedRetention: 2 * time.Hour,
		},
	}
	if !reflect.DeepEqual(cfg.Artifact, wantArtifact) {
		t.Fatalf("artifact config = %+v, want %+v", cfg.Artifact, wantArtifact)
	}
}

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

func TestContainerRuntimeConfig(t *testing.T) {
	args := []string{"--machine-credential-file=/credential", "--name=runner-a"}
	cfg, err := LoadConfig(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Container.Runtime != "docker" || cfg.Container.Containerd.Namespace != "eulermaker" || cfg.Container.Containerd.Snapshotter != "overlayfs" {
		t.Fatalf("runtime defaults = %+v", cfg)
	}
	cfg, err = LoadConfig(append(args, "--container-runtime=containerd", "--containerd-address=/run/test.sock",
		"--containerd-namespace=builds", "--containerd-snapshotter=native"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Container.Runtime != "containerd" || cfg.Container.Containerd.Address != "/run/test.sock" ||
		cfg.Container.Containerd.Namespace != "builds" || cfg.Container.Containerd.Snapshotter != "native" {
		t.Fatalf("runtime config = %+v", cfg)
	}
}

func TestArtifactConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := LoadConfig([]string{"--machine-credential-file=/credential", "--name=runner-a"})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Artifact.Upload.MaxFileSize != 25<<30 || cfg.Artifact.Upload.MaxJobSize != 100<<30 || cfg.Artifact.Upload.MaxFiles != 10000 || cfg.Artifact.Upload.Concurrency != 4 {
		t.Fatalf("unexpected artifact defaults: %#v", cfg)
	}
	if cfg.Artifact.Upload.FailedRetention != 24*time.Hour {
		t.Fatalf("failed retention = %s, want 24h", cfg.Artifact.Upload.FailedRetention)
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
