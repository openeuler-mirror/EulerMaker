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
	if o.Manager.Workers != 2 || o.Source.PollPageSize != 500 || o.Manager.ControllerMaxRetries != 15 {
		t.Fatalf("unexpected defaults: %+v", o)
	}
	if o.Manager.SlowRetryInitialDelay != 30*time.Second || o.Manager.SlowRetryMaxDelay != 15*time.Minute || o.Manager.SlowRetryJitter != 0.2 {
		t.Fatalf("unexpected slow retry defaults: %+v", o.Manager)
	}
	if o.Job.RunnerLostGracePeriod != 5*time.Minute || !o.Job.HistoryGCEnabled || o.Job.HistoryRetention != 720*time.Hour {
		t.Fatalf("unexpected Job controller defaults: %+v", o)
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
}
func TestParseRequiresTLSConfiguration(t *testing.T) {
	if _, err := Parse([]string{"--apiserver=https://api:8443"}); err == nil {
		t.Fatal("expected TLS validation error")
	}
}
