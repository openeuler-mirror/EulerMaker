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
	Gateway            string
	Token              string
	Name               string
	Type               string
	Arch               string
	RootDir            string
	HeartbeatInterval  time.Duration
	InsecureSkipVerify bool
	GatewayCA          string
}

func LoadConfig(args []string) (Config, error) {
	hostname, _ := os.Hostname()
	cfg := Config{
		Gateway:           "https://ebs-gateway:8443",
		Name:              hostname,
		Type:              "dc",
		Arch:              runtimeArch(),
		RootDir:           "/var/lib/ebs-runner",
		HeartbeatInterval: 30 * time.Second,
	}

	fs := flag.NewFlagSet("ebs-runner", flag.ContinueOnError)
	fs.StringVar(&cfg.Gateway, "gateway", cfg.Gateway, "ebs-gateway address")
	fs.StringVar(&cfg.Token, "token", cfg.Token, "gateway bearer token")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "runner resource name")
	fs.StringVar(&cfg.Type, "type", cfg.Type, "runner type: dc, vm, or hw")
	fs.StringVar(&cfg.Arch, "arch", cfg.Arch, "runner architecture")
	fs.StringVar(&cfg.RootDir, "root-dir", cfg.RootDir, "runner root directory")
	fs.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", cfg.HeartbeatInterval, "heartbeat interval")
	fs.BoolVar(&cfg.InsecureSkipVerify, "insecure-skip-verify", cfg.InsecureSkipVerify, "skip gateway TLS verification")
	fs.StringVar(&cfg.GatewayCA, "gateway-ca", cfg.GatewayCA, "gateway CA file")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if cfg.Gateway == "" {
		return Config{}, fmt.Errorf("gateway is required")
	}
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("token is required")
	}
	if cfg.Name == "" {
		return Config{}, fmt.Errorf("runner name is required")
	}
	if cfg.Type != "dc" && cfg.Type != "vm" && cfg.Type != "hw" {
		return Config{}, fmt.Errorf("runner type must be one of dc, vm, hw")
	}
	if cfg.Arch == "" {
		return Config{}, fmt.Errorf("runner arch is required")
	}
	if cfg.RootDir == "" {
		return Config{}, fmt.Errorf("runner root dir is required")
	}
	if cfg.HeartbeatInterval <= 0 {
		return Config{}, fmt.Errorf("heartbeat interval must be greater than 0")
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
