package options

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/rest"
)

type Options struct {
	API     APIOptions
	Manager ManagerOptions
	Source  SourceOptions
	Health  HealthOptions
	Job     JobControllerOptions
}

type APIOptions struct {
	Server, ServerCA   string
	InsecureSkipVerify bool
	RequestTimeout     time.Duration
	ClientQPS          float64
	ClientBurst        int
}

type ManagerOptions struct {
	Controllers                   string
	Workers, ControllerMaxRetries int
	CacheSyncTimeout              time.Duration
	ShutdownTimeout               time.Duration
	SlowRetryInitialDelay         time.Duration
	SlowRetryMaxDelay             time.Duration
	SlowRetryJitter               float64
}

type SourceOptions struct {
	PollPeriod, SourceStaleThreshold, ResyncPeriod time.Duration
	PollPageSize                                   int
}

type HealthOptions struct {
	Address string
}

type JobControllerOptions struct {
	RunnerLostGracePeriod time.Duration
	HistoryRetention      time.Duration
	HistoryGCEnabled      bool
}

func Parse(args []string) (Options, error) {
	o := Options{
		API:     APIOptions{RequestTimeout: 30 * time.Second, ClientQPS: 20, ClientBurst: 40},
		Manager: ManagerOptions{Controllers: "*", Workers: 2, ControllerMaxRetries: 15, CacheSyncTimeout: 2 * time.Minute, ShutdownTimeout: 30 * time.Second, SlowRetryInitialDelay: 30 * time.Second, SlowRetryMaxDelay: 15 * time.Minute, SlowRetryJitter: 0.2},
		Source:  SourceOptions{PollPeriod: 30 * time.Second, PollPageSize: 500, SourceStaleThreshold: 2 * time.Minute, ResyncPeriod: 10 * time.Minute},
		Health:  HealthOptions{Address: ":8080"},
		Job:     JobControllerOptions{RunnerLostGracePeriod: 5 * time.Minute, HistoryGCEnabled: true, HistoryRetention: 720 * time.Hour},
	}
	f := flag.NewFlagSet("controller-manager", flag.ContinueOnError)
	f.StringVar(&o.API.Server, "apiserver", "", "ebs-apiserver address")
	f.StringVar(&o.API.ServerCA, "apiserver-ca", "", "server CA file")
	f.BoolVar(&o.API.InsecureSkipVerify, "insecure-skip-verify", false, "skip server certificate verification (development only)")
	f.StringVar(&o.Manager.Controllers, "controllers", o.Manager.Controllers, "controllers to enable (*, name, -name)")
	f.IntVar(&o.Manager.Workers, "workers", o.Manager.Workers, "workers per controller")
	f.IntVar(&o.Manager.ControllerMaxRetries, "controller-max-retries", o.Manager.ControllerMaxRetries, "maximum consecutive retries for a controller key")
	f.DurationVar(&o.Manager.SlowRetryInitialDelay, "controller-slow-retry-initial-delay", o.Manager.SlowRetryInitialDelay, "initial delay after fast controller retries are exhausted")
	f.DurationVar(&o.Manager.SlowRetryMaxDelay, "controller-slow-retry-max-delay", o.Manager.SlowRetryMaxDelay, "maximum delay between slow controller retries")
	f.Float64Var(&o.Manager.SlowRetryJitter, "controller-slow-retry-jitter", o.Manager.SlowRetryJitter, "fractional jitter applied to slow controller retries")
	f.DurationVar(&o.Source.PollPeriod, "poll-period", o.Source.PollPeriod, "default polling period")
	f.IntVar(&o.Source.PollPageSize, "poll-page-size", o.Source.PollPageSize, "polling list page size")
	f.DurationVar(&o.Manager.CacheSyncTimeout, "cache-sync-timeout", o.Manager.CacheSyncTimeout, "initial source sync timeout")
	f.DurationVar(&o.Manager.ShutdownTimeout, "shutdown-timeout", o.Manager.ShutdownTimeout, "graceful shutdown timeout")
	f.DurationVar(&o.Source.SourceStaleThreshold, "source-stale-threshold", o.Source.SourceStaleThreshold, "source readiness stale threshold")
	f.DurationVar(&o.API.RequestTimeout, "request-timeout", o.API.RequestTimeout, "non-watch request timeout")
	f.DurationVar(&o.Source.ResyncPeriod, "resync-period", o.Source.ResyncPeriod, "watch informer resync period")
	f.Float64Var(&o.API.ClientQPS, "client-qps", o.API.ClientQPS, "API client QPS")
	f.IntVar(&o.API.ClientBurst, "client-burst", o.API.ClientBurst, "API client burst")
	f.StringVar(&o.Health.Address, "health-bind-address", o.Health.Address, "health server address")
	f.DurationVar(&o.Job.RunnerLostGracePeriod, "job-runner-lost-grace-period", o.Job.RunnerLostGracePeriod, "grace period before failing a Job whose Runner is unavailable")
	f.BoolVar(&o.Job.HistoryGCEnabled, "job-history-gc-enabled", o.Job.HistoryGCEnabled, "delete terminal Jobs after their history retention period")
	f.DurationVar(&o.Job.HistoryRetention, "job-history-retention", o.Job.HistoryRetention, "retention period for terminal Jobs")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if o.API.Server == "" {
		return o, fmt.Errorf("apiserver is required")
	}
	if !o.API.InsecureSkipVerify && o.API.ServerCA == "" {
		return o, fmt.Errorf("apiserver-ca is required unless insecure-skip-verify is enabled")
	}
	if o.Manager.Controllers == "" || o.Manager.Workers <= 0 || o.Manager.ControllerMaxRetries < 0 || o.Manager.SlowRetryInitialDelay <= 0 || o.Manager.SlowRetryMaxDelay < o.Manager.SlowRetryInitialDelay || o.Manager.SlowRetryJitter < 0 || o.Manager.SlowRetryJitter >= 1 || o.Source.PollPageSize <= 0 || o.API.ClientQPS <= 0 || o.API.ClientBurst <= 0 || o.Source.PollPeriod <= 0 || o.Manager.CacheSyncTimeout <= 0 || o.Manager.ShutdownTimeout <= 0 || o.Source.SourceStaleThreshold <= 0 || o.API.RequestTimeout <= 0 || o.Source.ResyncPeriod < 0 || o.Health.Address == "" || o.Job.RunnerLostGracePeriod <= 0 || (o.Job.HistoryGCEnabled && o.Job.HistoryRetention <= 0) {
		return o, fmt.Errorf("workers, limits, periods, timeouts and addresses must be valid")
	}
	return o, nil
}
func (o Options) RESTConfig() (*rest.Config, error) {
	if o.API.ServerCA != "" {
		if _, err := os.Stat(o.API.ServerCA); err != nil {
			return nil, fmt.Errorf("read apiserver CA: %w", err)
		}
	}
	return &rest.Config{Host: o.API.Server, QPS: float32(o.API.ClientQPS), Burst: o.API.ClientBurst, TLSClientConfig: rest.TLSClientConfig{CAFile: o.API.ServerCA, Insecure: o.API.InsecureSkipVerify}}, nil
}
