package app

import (
	"testing"
	"time"

	"artifact-manager/pkg/artifact"
)

func TestServerCommandBindsOptions(t *testing.T) {
	var got options
	cmd := newCommand(func(opts options) error {
		got = opts
		return nil
	})
	cmd.SetArgs([]string{
		"--listen=:9000", "--gateway-url=https://gateway.example", "--auth-cache-ttl=2h",
		"--max-job-size=1048576", "--log-chunk-size=131072", "--repository-workers=4",
		"--release-history-ttl=48h", "--rpm-signing-workers=3",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.listen != ":9000" || got.config.Gateway.URL != "https://gateway.example" || got.config.Gateway.AuthCacheTTL != 2*time.Hour ||
		got.config.Upload.MaxJobSize != 1048576 || got.config.Log.ChunkSize != 131072 ||
		got.config.Repository.Workers != 4 || got.config.Release.HistoryTTL != 48*time.Hour || got.config.Signing.Workers != 3 {
		t.Fatalf("unexpected command options: %+v", got)
	}
	if got.config.Upload.MaxFileSize != artifact.DefaultConfig().Upload.MaxFileSize {
		t.Fatal("default artifact size was not retained")
	}
	if artifact.DefaultConfig().Signing.Workers != 8 {
		t.Fatal("default signing concurrency must be eight")
	}
	if got.config.Signing.SignatrustKeyName != "openeuler-default-key" {
		t.Fatalf("default Signatrust key name = %q", got.config.Signing.SignatrustKeyName)
	}
}
