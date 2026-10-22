package options

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/container"
	"ebs-runner/pkg/gateway"
)

type Config struct {
	Name              string
	Type              string
	Arch              string
	RootDir           string
	HeartbeatInterval time.Duration

	Gateway   gateway.Config
	Container container.Config
	Artifact  artifact.Config
}

func LoadConfig(args []string) (Config, error) {
	hostname, _ := os.Hostname()
	cfg := Config{
		Name:              hostname,
		Type:              "ct",
		Arch:              runtimeArch(),
		RootDir:           "/var/lib/ebs-runner",
		HeartbeatInterval: 30 * time.Second,
		Gateway: gateway.Config{
			URL: "https://ebs-gateway:8443",
		},
		Container: container.Config{
			Runtime: "docker",
			Containerd: container.ContainerdConfig{
				Address:     "/run/containerd/containerd.sock",
				Namespace:   "eulermaker",
				Snapshotter: "overlayfs",
			},
		},
		Artifact: artifact.Config{
			AccessMode: "internal",
			Log: artifact.LogConfig{
				ChunkSize:       256 << 10,
				FlushInterval:   500 * time.Millisecond,
				SpoolLimit:      4 << 30,
				DrainTimeout:    30 * time.Second,
				RetryMaxBackoff: 30 * time.Second,
			},
			Upload: artifact.UploadConfig{
				MaxFileSize:     25 << 30,
				MaxJobSize:      100 << 30,
				MaxFiles:        10000,
				Concurrency:     4,
				Timeout:         2 * time.Hour,
				RetryMaxBackoff: 30 * time.Second,
				FailedRetention: 24 * time.Hour,
			},
		},
	}

	fs := flag.NewFlagSet("ebs-runner", flag.ContinueOnError)
	fs.StringVar(&cfg.Gateway.URL, "gateway", cfg.Gateway.URL, "ebs-gateway address")
	fs.StringVar(&cfg.Artifact.AccessMode, "artifact-access-mode", cfg.Artifact.AccessMode, "Runner service access mode for artifacts and Git: internal or external")
	fs.StringVar(&cfg.Gateway.MachineCredentialFile, "machine-credential-file", cfg.Gateway.MachineCredentialFile, "MachineAccount credential JSON file")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "runner resource name")
	fs.StringVar(&cfg.Type, "type", cfg.Type, "runner type: ct, vm, or hw")
	fs.StringVar(&cfg.RootDir, "root-dir", cfg.RootDir, "runner root directory")
	fs.StringVar(&cfg.Container.Runtime, "container-runtime", cfg.Container.Runtime, "CT runtime: docker or containerd")
	fs.StringVar(&cfg.Container.Containerd.Address, "containerd-address", cfg.Container.Containerd.Address, "containerd socket address")
	fs.StringVar(&cfg.Container.Containerd.Namespace, "containerd-namespace", cfg.Container.Containerd.Namespace, "containerd namespace for Runner containers")
	fs.StringVar(&cfg.Container.Containerd.Snapshotter, "containerd-snapshotter", cfg.Container.Containerd.Snapshotter, "containerd rootfs snapshotter")
	fs.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", cfg.HeartbeatInterval, "heartbeat interval")
	fs.BoolVar(&cfg.Gateway.InsecureSkipVerify, "insecure-skip-verify", cfg.Gateway.InsecureSkipVerify, "skip gateway TLS verification")
	fs.StringVar(&cfg.Gateway.CA, "gateway-ca", cfg.Gateway.CA, "gateway CA file")
	fs.StringVar(&cfg.Artifact.CA, "artifact-manager-ca", cfg.Artifact.CA, "artifact-manager CA file")
	fs.BoolVar(&cfg.Artifact.InsecureSkipVerify, "artifact-manager-insecure-skip-verify", false, "skip artifact-manager TLS verification")
	fs.Int64Var(&cfg.Artifact.Log.ChunkSize, "log-chunk-size", cfg.Artifact.Log.ChunkSize, "log chunk size")
	fs.DurationVar(&cfg.Artifact.Log.FlushInterval, "log-flush-interval", cfg.Artifact.Log.FlushInterval, "log chunk flush interval")
	fs.Int64Var(&cfg.Artifact.Log.SpoolLimit, "log-spool-limit", cfg.Artifact.Log.SpoolLimit, "per-job log spool size limit")
	fs.DurationVar(&cfg.Artifact.Log.DrainTimeout, "log-drain-timeout", cfg.Artifact.Log.DrainTimeout, "log drain timeout")
	fs.DurationVar(&cfg.Artifact.Log.RetryMaxBackoff, "log-retry-max-backoff", cfg.Artifact.Log.RetryMaxBackoff, "maximum log retry backoff")
	fs.Int64Var(&cfg.Artifact.Upload.MaxFileSize, "artifact-max-file-size", cfg.Artifact.Upload.MaxFileSize, "maximum ordinary artifact file size in bytes")
	fs.Int64Var(&cfg.Artifact.Upload.MaxJobSize, "artifact-max-job-size", cfg.Artifact.Upload.MaxJobSize, "maximum ordinary artifact total size per job in bytes")
	fs.IntVar(&cfg.Artifact.Upload.MaxFiles, "artifact-max-files", cfg.Artifact.Upload.MaxFiles, "maximum ordinary artifact files per job")
	fs.IntVar(&cfg.Artifact.Upload.Concurrency, "artifact-upload-concurrency", cfg.Artifact.Upload.Concurrency, "ordinary artifact upload concurrency per job")
	fs.DurationVar(&cfg.Artifact.Upload.Timeout, "artifact-upload-timeout", cfg.Artifact.Upload.Timeout, "total ordinary artifact post-run timeout")
	fs.DurationVar(&cfg.Artifact.Upload.RetryMaxBackoff, "artifact-retry-max-backoff", cfg.Artifact.Upload.RetryMaxBackoff, "maximum ordinary artifact retry backoff")
	fs.DurationVar(&cfg.Artifact.Upload.FailedRetention, "artifact-failed-retention", cfg.Artifact.Upload.FailedRetention, "retention for failed artifact upload state")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if cfg.Gateway.URL == "" {
		return Config{}, fmt.Errorf("gateway is required")
	}
	if cfg.Artifact.AccessMode != "internal" && cfg.Artifact.AccessMode != "external" {
		return Config{}, fmt.Errorf("artifact access mode must be internal or external")
	}
	if cfg.Gateway.MachineCredentialFile == "" {
		return Config{}, fmt.Errorf("machine credential file is required")
	}
	if cfg.Name == "" {
		return Config{}, fmt.Errorf("runner name is required")
	}
	if cfg.Type != "ct" && cfg.Type != "vm" && cfg.Type != "hw" {
		return Config{}, fmt.Errorf("runner type must be one of ct, vm, hw")
	}
	if cfg.Container.Runtime != "docker" && cfg.Container.Runtime != "containerd" {
		return Config{}, fmt.Errorf("container runtime must be docker or containerd")
	}
	if cfg.Artifact.Upload.MaxFileSize <= 0 || cfg.Artifact.Upload.MaxJobSize <= 0 ||
		cfg.Artifact.Upload.MaxFiles <= 0 || cfg.Artifact.Upload.Concurrency <= 0 {
		return Config{}, fmt.Errorf("artifact size, file count, and concurrency limits must be positive")
	}
	if cfg.Artifact.Upload.MaxFileSize > cfg.Artifact.Upload.MaxJobSize {
		return Config{}, fmt.Errorf("artifact max file size cannot exceed max job size")
	}
	if cfg.Artifact.Upload.FailedRetention < 0 {
		return Config{}, fmt.Errorf("artifact failed retention cannot be negative")
	}
	if cfg.Artifact.Upload.Timeout <= 0 || cfg.Artifact.Upload.RetryMaxBackoff <= 0 {
		return Config{}, fmt.Errorf("artifact upload timeout and retry backoff must be positive")
	}
	if cfg.RootDir == "" {
		return Config{}, fmt.Errorf("runner root dir is required")
	}
	if cfg.HeartbeatInterval <= 0 {
		return Config{}, fmt.Errorf("heartbeat interval must be greater than 0")
	}
	if cfg.Artifact.Log.ChunkSize <= 0 || cfg.Artifact.Log.FlushInterval <= 0 ||
		cfg.Artifact.Log.SpoolLimit < cfg.Artifact.Log.ChunkSize ||
		cfg.Artifact.Log.DrainTimeout <= 0 || cfg.Artifact.Log.RetryMaxBackoff <= 0 {
		return Config{}, fmt.Errorf("invalid log upload configuration")
	}
	return cfg, nil
}

func runtimeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}
