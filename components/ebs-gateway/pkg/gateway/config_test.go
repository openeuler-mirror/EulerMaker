package gateway

import "testing"

func TestLoadConfigUsesDefaultsAndFlags(t *testing.T) {
	cfg, err := LoadConfig([]string{"--jwt-secret-file=/from/flag"})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Port != 8080 || cfg.APIServerAddr != "https://ebs-apiserver:8443" {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.JWTSecretFile != "/from/flag" {
		t.Fatalf("expected flag value, got %q", cfg.JWTSecretFile)
	}
	if cfg.InsecureSkipVerify || cfg.RateLimitPerSec != 100 || cfg.RateLimitBurst != 200 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
}
