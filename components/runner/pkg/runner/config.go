package runner

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"
)

type Config struct {
	Gateway                           string
	ArtifactManager                   string
	MachineCredentialFile             string
	Name                              string
	Type                              string
	Arch                              string
	RootDir                           string
	HeartbeatInterval                 time.Duration
	InsecureSkipVerify                bool
	GatewayCA                         string
	ArtifactManagerCA                 string
	ArtifactManagerInsecureSkipVerify bool
	LogChunkSize                      int64
	LogFlushInterval                  time.Duration
	LogSpoolLimit                     int64
	LogDrainTimeout                   time.Duration
	LogRetryMaxBackoff                time.Duration
	ArtifactMaxFileSize               int64
	ArtifactMaxJobSize                int64
	ArtifactMaxFiles                  int
	ArtifactUploadConcurrency         int
	ArtifactUploadTimeout             time.Duration
	ArtifactRetryMaxBackoff           time.Duration
	ArtifactFailedRetention           time.Duration
}

func LoadConfig(args []string) (Config, error) {
	hostname, _ := os.Hostname()
	cfg := Config{
		Gateway:                   "https://ebs-gateway:8443",
		ArtifactManager:           "http://artifact-manager:8081",
		Name:                      hostname,
		Type:                      "ct",
		Arch:                      runtimeArch(),
		RootDir:                   "/var/lib/ebs-runner",
		HeartbeatInterval:         30 * time.Second,
		LogChunkSize:              256 << 10,
		LogFlushInterval:          500 * time.Millisecond,
		LogSpoolLimit:             4 << 30,
		LogDrainTimeout:           30 * time.Second,
		LogRetryMaxBackoff:        30 * time.Second,
		ArtifactMaxFileSize:       25 << 30,
		ArtifactMaxJobSize:        100 << 30,
		ArtifactMaxFiles:          10000,
		ArtifactUploadConcurrency: 4,
		ArtifactUploadTimeout:     2 * time.Hour,
		ArtifactRetryMaxBackoff:   30 * time.Second,
		ArtifactFailedRetention:   24 * time.Hour,
	}

	fs := flag.NewFlagSet("ebs-runner", flag.ContinueOnError)
	fs.StringVar(&cfg.Gateway, "gateway", cfg.Gateway, "ebs-gateway address")
	fs.StringVar(&cfg.ArtifactManager, "artifact-manager", cfg.ArtifactManager, "artifact-manager address")
	fs.StringVar(&cfg.MachineCredentialFile, "machine-credential-file", cfg.MachineCredentialFile, "MachineAccount credential JSON file")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "runner resource name")
	fs.StringVar(&cfg.Type, "type", cfg.Type, "runner type: ct, vm, or hw")
	fs.StringVar(&cfg.RootDir, "root-dir", cfg.RootDir, "runner root directory")
	fs.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", cfg.HeartbeatInterval, "heartbeat interval")
	fs.BoolVar(&cfg.InsecureSkipVerify, "insecure-skip-verify", cfg.InsecureSkipVerify, "skip gateway TLS verification")
	fs.StringVar(&cfg.GatewayCA, "gateway-ca", cfg.GatewayCA, "gateway CA file")
	fs.StringVar(&cfg.ArtifactManagerCA, "artifact-manager-ca", cfg.ArtifactManagerCA, "artifact-manager CA file")
	fs.BoolVar(&cfg.ArtifactManagerInsecureSkipVerify, "artifact-manager-insecure-skip-verify", false, "skip artifact-manager TLS verification")
	fs.Int64Var(&cfg.LogChunkSize, "log-chunk-size", cfg.LogChunkSize, "log chunk size")
	fs.DurationVar(&cfg.LogFlushInterval, "log-flush-interval", cfg.LogFlushInterval, "log chunk flush interval")
	fs.Int64Var(&cfg.LogSpoolLimit, "log-spool-limit", cfg.LogSpoolLimit, "per-job log spool size limit")
	fs.DurationVar(&cfg.LogDrainTimeout, "log-drain-timeout", cfg.LogDrainTimeout, "log drain timeout")
	fs.DurationVar(&cfg.LogRetryMaxBackoff, "log-retry-max-backoff", cfg.LogRetryMaxBackoff, "maximum log retry backoff")
	fs.Int64Var(&cfg.ArtifactMaxFileSize, "artifact-max-file-size", cfg.ArtifactMaxFileSize, "maximum ordinary artifact file size in bytes")
	fs.Int64Var(&cfg.ArtifactMaxJobSize, "artifact-max-job-size", cfg.ArtifactMaxJobSize, "maximum ordinary artifact total size per job in bytes")
	fs.IntVar(&cfg.ArtifactMaxFiles, "artifact-max-files", cfg.ArtifactMaxFiles, "maximum ordinary artifact files per job")
	fs.IntVar(&cfg.ArtifactUploadConcurrency, "artifact-upload-concurrency", cfg.ArtifactUploadConcurrency, "ordinary artifact upload concurrency per job")
	fs.DurationVar(&cfg.ArtifactUploadTimeout, "artifact-upload-timeout", cfg.ArtifactUploadTimeout, "total ordinary artifact post-run timeout")
	fs.DurationVar(&cfg.ArtifactRetryMaxBackoff, "artifact-retry-max-backoff", cfg.ArtifactRetryMaxBackoff, "maximum ordinary artifact retry backoff")
	fs.DurationVar(&cfg.ArtifactFailedRetention, "artifact-failed-retention", cfg.ArtifactFailedRetention, "retention for failed artifact upload state")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if cfg.Gateway == "" {
		return Config{}, fmt.Errorf("gateway is required")
	}
	if cfg.ArtifactManager == "" {
		return Config{}, fmt.Errorf("artifact manager is required")
	}
	if cfg.MachineCredentialFile == "" {
		return Config{}, fmt.Errorf("machine credential file is required")
	}
	if cfg.Name == "" {
		return Config{}, fmt.Errorf("runner name is required")
	}
	if cfg.Type != "ct" && cfg.Type != "vm" && cfg.Type != "hw" {
		return Config{}, fmt.Errorf("runner type must be one of ct, vm, hw")
	}
	if cfg.ArtifactMaxFileSize <= 0 || cfg.ArtifactMaxJobSize <= 0 || cfg.ArtifactMaxFiles <= 0 || cfg.ArtifactUploadConcurrency <= 0 {
		return Config{}, fmt.Errorf("artifact size, file count, and concurrency limits must be positive")
	}
	if cfg.ArtifactMaxFileSize > cfg.ArtifactMaxJobSize {
		return Config{}, fmt.Errorf("artifact max file size cannot exceed max job size")
	}
	if cfg.ArtifactFailedRetention < 0 {
		return Config{}, fmt.Errorf("artifact failed retention cannot be negative")
	}
	if cfg.ArtifactUploadTimeout <= 0 || cfg.ArtifactRetryMaxBackoff <= 0 {
		return Config{}, fmt.Errorf("artifact upload timeout and retry backoff must be positive")
	}
	if cfg.Arch != "aarch64" && cfg.Arch != "x86_64" {
		return Config{}, fmt.Errorf("unsupported runtime architecture %q", cfg.Arch)
	}
	if cfg.RootDir == "" {
		return Config{}, fmt.Errorf("runner root dir is required")
	}
	if cfg.HeartbeatInterval <= 0 {
		return Config{}, fmt.Errorf("heartbeat interval must be greater than 0")
	}
	if cfg.LogChunkSize <= 0 || cfg.LogFlushInterval <= 0 || cfg.LogSpoolLimit < cfg.LogChunkSize || cfg.LogDrainTimeout <= 0 || cfg.LogRetryMaxBackoff <= 0 {
		return Config{}, fmt.Errorf("invalid log upload configuration")
	}
	return cfg, nil
}

func (c Config) HTTPClient() (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.InsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if c.GatewayCA != "" {
		pem, err := os.ReadFile(c.GatewayCA)
		if err != nil {
			return nil, fmt.Errorf("read gateway ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse gateway ca: no certificates found")
		}
		tlsConfig.RootCAs = pool
	}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport}, nil
}

func (c Config) ArtifactManagerHTTPClient() (*http.Client, error) {
	return tlsHTTPClient(c.ArtifactManagerCA, c.ArtifactManagerInsecureSkipVerify)
}

func tlsHTTPClient(caFile string, insecure bool) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse CA: no certificates found")
		}
		tlsConfig.RootCAs = pool
	}
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport}, nil
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
