package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port               int
	APIServerAddr      string
	JWTSecret          string
	InsecureSkipVerify bool
	APIServerCA        string
	RateLimitPerSec    float64
	RateLimitBurst     int
	LogLevel           string
}

func LoadConfig(args []string) (Config, error) {
	cfg := Config{
		Port:            envInt("PORT", 8080),
		APIServerAddr:   envString("EBS_APISERVER", "https://ebs-apiserver:8443"),
		JWTSecret:       envString("JWT_SECRET", ""),
		APIServerCA:     envString("APISERVER_CA", ""),
		RateLimitPerSec: envFloat("RATE_LIMIT_PER_SEC", 100),
		RateLimitBurst:  envInt("RATE_LIMIT_BURST", 200),
		LogLevel:        envString("LOG_LEVEL", "info"),
	}
	cfg.InsecureSkipVerify = envBool("INSECURE_SKIP_VERIFY", false)

	fs := flag.NewFlagSet("ebs-gateway", flag.ContinueOnError)
	fs.IntVar(&cfg.Port, "port", cfg.Port, "gateway listen port")
	fs.StringVar(&cfg.APIServerAddr, "apiserver-addr", cfg.APIServerAddr, "upstream ebs-apiserver address")
	fs.StringVar(&cfg.JWTSecret, "jwt-secret", cfg.JWTSecret, "HMAC JWT signing secret")
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
	if cfg.JWTSecret == "" {
		return Config{}, fmt.Errorf("jwt secret is required")
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

func envString(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
