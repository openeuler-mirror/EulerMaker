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
	cmd.SetArgs([]string{"--listen=:9000", "--repository-workers=4", "--rpm-signing-workers=3", "--release-history-ttl=48h"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.listen != ":9000" || got.config.RepositoryWorkers != 4 || got.config.RPMSigningWorkers != 3 || got.config.ReleaseHistoryTTL != 48*time.Hour {
		t.Fatalf("unexpected command options: %+v", got)
	}
	if got.config.MaxFileSize != artifact.DefaultConfig().MaxFileSize {
		t.Fatal("default artifact size was not retained")
	}
	if artifact.DefaultConfig().RPMSigningWorkers != 8 {
		t.Fatal("default signing concurrency must be eight")
	}
	if got.config.RPMSigningSignatrustKeyName != "openeuler-default-key" {
		t.Fatalf("default Signatrust key name = %q", got.config.RPMSigningSignatrustKeyName)
	}
}
