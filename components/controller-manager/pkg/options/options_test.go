package options

import (
	"testing"
	"time"
)

func TestParseDevelopmentOptions(t *testing.T) {
	o, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Manager.Workers != 6 || o.Source.PollPageSize != 500 || o.Manager.ControllerMaxRetries != 15 {
		t.Fatalf("unexpected defaults: %+v", o)
	}
	if o.Snapshot.ResolveWorkers != 10 {
		t.Fatalf("unexpected Snapshot resolve workers: %d", o.Snapshot.ResolveWorkers)
	}
	if o.Snapshot.FailureRetryLimit != 5 {
		t.Fatalf("unexpected Snapshot failure retry limit: %d", o.Snapshot.FailureRetryLimit)
	}
	if o.Manager.SlowRetryInitialDelay != 30*time.Second || o.Manager.SlowRetryMaxDelay != 15*time.Minute || o.Manager.SlowRetryJitter != 0.2 {
		t.Fatalf("unexpected slow retry defaults: %+v", o.Manager)
	}
	if o.Job.RunnerLostGracePeriod != 5*time.Minute || !o.Job.HistoryGCEnabled || o.Job.HistoryRetention != 720*time.Hour {
		t.Fatalf("unexpected Job controller defaults: %+v", o)
	}
	if o.Runner.HeartbeatTimeout != 2*time.Minute || o.Runner.StartupGracePeriod != 5*time.Minute {
		t.Fatalf("unexpected Runner controller defaults: %+v", o.Runner)
	}
	if o.RpmRepo.MaxJobsPerBatch != 100 || o.RpmRepo.MaterializeRetryLimit != 3 || o.RpmRepo.ArtifactManagerTimeout != 30*time.Second {
		t.Fatalf("unexpected RpmRepo controller defaults: %+v", o.RpmRepo)
	}
}

func TestParseRpmRepoFlags(t *testing.T) {
	o, err := Parse([]string{
		"--apiserver=https://api:8443",
		"--insecure-skip-verify=true",
		"--rpmrepo-max-jobs-per-batch=5",
		"--rpmrepo-materialize-retry-limit=4",
		"--artifact-manager-addr=http://artifact-manager:8080",
		"--artifact-manager-timeout=10s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.RpmRepo.MaxJobsPerBatch != 5 || o.RpmRepo.MaterializeRetryLimit != 4 {
		t.Fatalf("flat RpmRepo limits were not stored: %+v", o.RpmRepo)
	}
	if o.RpmRepo.ArtifactManagerAddr != "http://artifact-manager:8080" || o.RpmRepo.ArtifactManagerTimeout != 10*time.Second {
		t.Fatalf("flat artifact manager flags were not stored: %+v", o.RpmRepo)
	}
}

func TestParseRejectsInvalidRpmRepoOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--rpmrepo-materialize-retry-limit=0"},
		{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--rpmrepo-max-jobs-per-batch=0"},
		{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--artifact-manager-timeout=0"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("invalid RpmRepo options were accepted: %v", args)
		}
	}
}

func TestParseValidatesArtifactManagerAddress(t *testing.T) {
	for _, address := range []string{"artifact-manager:8080", "ftp://artifact-manager", "http://", "://bad"} {
		if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--artifact-manager-addr=" + address}); err == nil {
			t.Fatalf("invalid artifact manager address %q was accepted", address)
		}
	}
	o, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--artifact-manager-addr=https://artifact-manager:8443"})
	if err != nil {
		t.Fatalf("valid artifact manager address rejected: %v", err)
	}
	if o.RpmRepo.ArtifactManagerAddr != "https://artifact-manager:8443" {
		t.Fatalf("unexpected artifact manager address %q", o.RpmRepo.ArtifactManagerAddr)
	}
	// An unset address keeps the controller inactive instead of failing startup.
	if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true"}); err != nil {
		t.Fatalf("unset artifact manager address must stay valid: %v", err)
	}
}

func TestParseFlatFlagsIntoNestedOptions(t *testing.T) {
	o, err := Parse([]string{
		"--apiserver=https://api:8443",
		"--insecure-skip-verify=true",
		"--workers=3",
		"--poll-page-size=100",
		"--job-runner-lost-grace-period=1m",
		"--job-history-gc-enabled=false",
		"--runner-heartbeat-timeout=3m",
		"--runner-startup-grace-period=6m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Manager.Workers != 3 || o.Source.PollPageSize != 100 {
		t.Fatalf("flat common flags were not stored in nested options: %+v", o)
	}
	if o.Job.RunnerLostGracePeriod != time.Minute || o.Job.HistoryGCEnabled {
		t.Fatalf("flat Job flags were not stored in nested options: %+v", o.Job)
	}
	if o.Runner.HeartbeatTimeout != 3*time.Minute || o.Runner.StartupGracePeriod != 6*time.Minute {
		t.Fatalf("flat Runner flags were not stored in nested options: %+v", o.Runner)
	}
}

func TestParseRejectsInvalidJobControllerDurations(t *testing.T) {
	if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--job-runner-lost-grace-period=0"}); err == nil {
		t.Fatal("zero runner lost grace period was accepted")
	}
	if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--controller-max-retries=-1"}); err == nil {
		t.Fatal("negative controller max retries was accepted")
	}
	if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--controller-slow-retry-initial-delay=2m", "--controller-slow-retry-max-delay=1m"}); err == nil {
		t.Fatal("slow retry maximum below initial delay was accepted")
	}
	if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--controller-slow-retry-jitter=1"}); err == nil {
		t.Fatal("slow retry jitter outside [0, 1) was accepted")
	}
	if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--job-history-retention=0"}); err == nil {
		t.Fatal("zero enabled history retention was accepted")
	}
	if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--job-history-gc-enabled=false", "--job-history-retention=0"}); err != nil {
		t.Fatalf("disabled GC rejected unused retention: %v", err)
	}
	if _, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true", "--runner-heartbeat-timeout=0"}); err == nil {
		t.Fatal("zero Runner heartbeat timeout was accepted")
	}
}
func TestParseRequiresTLSConfiguration(t *testing.T) {
	if _, err := Parse([]string{"--apiserver=https://api:8443"}); err == nil {
		t.Fatal("expected TLS validation error")
	}
}
