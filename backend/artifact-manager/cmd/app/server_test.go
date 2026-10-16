package app

import (
	"testing"
	"time"

	"artifact-manager/pkg/server"
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
		got.config.Upload.MaxJobSize != 1048576 || got.config.Upload.LogChunkSize != 131072 ||
		got.config.Repository.Workers != 4 || got.config.Release.HistoryTTL != 48*time.Hour || got.config.Signing.Workers != 3 {
		t.Fatalf("unexpected command options: %+v", got)
	}
	if got.config.Upload.MaxFileSize != server.DefaultConfig().Upload.MaxFileSize {
		t.Fatal("default artifact size was not retained")
	}
	if server.DefaultConfig().Signing.Workers != 8 {
		t.Fatal("default signing concurrency must be eight")
	}
	if got.config.Signing.SignatrustKeyName != "openeuler-default-key" {
		t.Fatalf("default Signatrust key name = %q", got.config.Signing.SignatrustKeyName)
	}
}

func TestOptionsValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "defaults"},
		{name: "listen", args: []string{"--listen="}, want: "--listen must not be empty"},
		{name: "upload", args: []string{"--max-file-size=0"}, want: "--max-file-size must be greater than 0"},
		{name: "repository", args: []string{"--repository-workers=-1"}, want: "--repository-workers must be greater than 0"},
		{name: "release", args: []string{"--release-timeout=0s"}, want: "--release-timeout must be greater than 0"},
		{name: "signing", args: []string{"--rpm-signing-workers=0"}, want: "--rpm-signing-workers must be greater than 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newCommand(func(opts options) error { return opts.validate() })
			cmd.SetArgs(append([]string{}, tt.args...))
			err := cmd.Execute()
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("validation error = %v, want %q", err, tt.want)
			}
		})
	}
}
