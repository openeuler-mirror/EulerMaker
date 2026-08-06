package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

type Config struct {
	Port                int
	APIServerAddr       string
	JWTSecretFile       string
	MaxRequestBodyBytes int64
	InsecureSkipVerify  bool
	APIServerCA         string
	RateLimitPerSec     float64
	RateLimitBurst      int
	LogLevel            string
}

func LoadConfig(args []string) (Config, error) {
	cfg := Config{
		Port:                8080,
		APIServerAddr:       "https://ebs-apiserver:8443",
		MaxRequestBodyBytes: 1048576,
		RateLimitPerSec:     100,
		RateLimitBurst:      200,
		LogLevel:            "info",
	}

	fs := flag.NewFlagSet("ebs-gateway", flag.ContinueOnError)
	fs.IntVar(&cfg.Port, "port", cfg.Port, "gateway listen port")
	fs.StringVar(&cfg.APIServerAddr, "apiserver-addr", cfg.APIServerAddr, "upstream ebs-apiserver address")
	fs.StringVar(&cfg.JWTSecretFile, "jwt-secret-file", cfg.JWTSecretFile, "base64-encoded HMAC JWT secret file")
	fs.Int64Var(&cfg.MaxRequestBodyBytes, "max-request-body-bytes", cfg.MaxRequestBodyBytes, "maximum request body size")
	fs.BoolVar(&cfg.InsecureSkipVerify, "insecure-skip-verify", cfg.InsecureSkipVerify, "skip upstream TLS verification")
	fs.StringVar(&cfg.APIServerCA, "apiserver-ca", cfg.APIServerCA, "upstream apiserver CA file")
	fs.Float64Var(&cfg.RateLimitPerSec, "rate-limit-per-sec", cfg.RateLimitPerSec, "rate limit token refill rate")
	fs.IntVar(&cfg.RateLimitBurst, "rate-limit-burst", cfg.RateLimitBurst, "rate limit burst size")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if cfg.APIServerAddr == "" {
		return Config{}, fmt.Errorf("apiserver address is required")
	}
	if cfg.JWTSecretFile == "" {
		return Config{}, fmt.Errorf("jwt secret file is required")
	}
	if cfg.MaxRequestBodyBytes <= 0 {
		return Config{}, fmt.Errorf("max-request-body-bytes must be greater than 0")
	}
	if cfg.RateLimitPerSec <= 0 {
		return Config{}, fmt.Errorf("rate-limit-per-sec must be greater than 0")
	}
	if cfg.RateLimitBurst <= 0 {
		return Config{}, fmt.Errorf("rate-limit-burst must be greater than 0")
	}
	return cfg, nil
}

func (c Config) HTTPTransport() (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.InsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if c.APIServerCA != "" {
		pem, err := os.ReadFile(c.APIServerCA)
		if err != nil {
			return nil, fmt.Errorf("read apiserver ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse apiserver ca: no certificates found")
		}
		tlsConfig.RootCAs = pool
	}
	transport.TLSClientConfig = tlsConfig
	transport.ResponseHeaderTimeout = 0
	transport.IdleConnTimeout = 90 * time.Second
	return transport, nil
}
