package options

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"k8s.io/client-go/rest"
)

type Options struct {
	API       APIOptions
	Manager   ManagerOptions
	Source    SourceOptions
	Health    HealthOptions
	Job       JobControllerOptions
	Runner    RunnerControllerOptions
	RpmRepo   RpmRepoControllerOptions
	Snapshot  SnapshotControllerOptions
	GitServer GitServerOptions
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

type RunnerControllerOptions struct {
	HeartbeatTimeout   time.Duration
	StartupGracePeriod time.Duration
}

type RpmRepoControllerOptions struct {
	MaxJobsPerBatch        int
	MaxInputBytes          int64
	MaterializeRetryLimit  int
	ArtifactManagerAddr    string
	ArtifactManagerTimeout time.Duration
}

type SnapshotControllerOptions struct {
	ResolveWorkers    int
	ResolveBudget     time.Duration
	SyncRequeueDelay  time.Duration
	FailureRetryLimit int
}

type GitServerOptions struct {
	Address  string
	Timeout  time.Duration
	Retries  int
	CacheTTL time.Duration
}

func Parse(args []string) (Options, error) {
	o := Options{
		API:       APIOptions{RequestTimeout: 30 * time.Second, ClientQPS: 20, ClientBurst: 40},
		Manager:   ManagerOptions{Controllers: "*", Workers: 6, ControllerMaxRetries: 15, CacheSyncTimeout: 2 * time.Minute, ShutdownTimeout: 30 * time.Second, SlowRetryInitialDelay: 30 * time.Second, SlowRetryMaxDelay: 15 * time.Minute, SlowRetryJitter: 0.2},
		Source:    SourceOptions{PollPeriod: 30 * time.Second, PollPageSize: 500, SourceStaleThreshold: 2 * time.Minute, ResyncPeriod: 10 * time.Minute},
		Health:    HealthOptions{Address: ":8080"},
		Job:       JobControllerOptions{RunnerLostGracePeriod: 5 * time.Minute, HistoryGCEnabled: true, HistoryRetention: 720 * time.Hour},
		Runner:    RunnerControllerOptions{HeartbeatTimeout: 2 * time.Minute, StartupGracePeriod: 5 * time.Minute},
		RpmRepo:   RpmRepoControllerOptions{MaxJobsPerBatch: 20, MaxInputBytes: 21474836480, MaterializeRetryLimit: 3, ArtifactManagerTimeout: 30 * time.Second},
		Snapshot:  SnapshotControllerOptions{ResolveWorkers: 10, ResolveBudget: 120 * time.Second, SyncRequeueDelay: 30 * time.Second, FailureRetryLimit: 5},
		GitServer: GitServerOptions{Address: "http://localhost:8080", Timeout: 30 * time.Second, Retries: 3, CacheTTL: 30 * time.Second},
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
	f.DurationVar(&o.Runner.HeartbeatTimeout, "runner-heartbeat-timeout", o.Runner.HeartbeatTimeout, "timeout since the last persisted Runner heartbeat")
	f.DurationVar(&o.Runner.StartupGracePeriod, "runner-startup-grace-period", o.Runner.StartupGracePeriod, "grace period for a new Runner to publish its first heartbeat")
	f.IntVar(&o.RpmRepo.MaxJobsPerBatch, "rpmrepo-max-jobs-per-batch", o.RpmRepo.MaxJobsPerBatch, "maximum number of Jobs in one repository batch")
	f.Int64Var(&o.RpmRepo.MaxInputBytes, "rpmrepo-max-input-bytes", o.RpmRepo.MaxInputBytes, "maximum materialization input bytes in one repository batch")
	f.IntVar(&o.RpmRepo.MaterializeRetryLimit, "rpmrepo-materialize-retry-limit", o.RpmRepo.MaterializeRetryLimit, "retryable materialization failures before the batch is abandoned")
	f.StringVar(&o.RpmRepo.ArtifactManagerAddr, "artifact-manager-addr", o.RpmRepo.ArtifactManagerAddr, "artifact-manager address")
	f.DurationVar(&o.RpmRepo.ArtifactManagerTimeout, "artifact-manager-timeout", o.RpmRepo.ArtifactManagerTimeout, "artifact-manager request timeout")
	f.IntVar(&o.Snapshot.ResolveWorkers, "snapshot-resolve-workers", o.Snapshot.ResolveWorkers, "concurrent repository resolution workers per Snapshot")
	f.DurationVar(&o.Snapshot.ResolveBudget, "snapshot-resolve-budget", o.Snapshot.ResolveBudget, "total repository resolution budget per Snapshot reconcile")
	f.DurationVar(&o.Snapshot.SyncRequeueDelay, "snapshot-sync-requeue-delay", o.Snapshot.SyncRequeueDelay, "delay before checking repositories that are still synchronizing")
	f.IntVar(&o.Snapshot.FailureRetryLimit, "snapshot-failure-retry-limit", o.Snapshot.FailureRetryLimit, "confirmed temporary failures before a repository is skipped")
	f.StringVar(&o.GitServer.Address, "git-server-addr", o.GitServer.Address, "git-server address")
	f.DurationVar(&o.GitServer.Timeout, "git-server-timeout", o.GitServer.Timeout, "git-server request timeout")
	f.IntVar(&o.GitServer.Retries, "git-server-retry", o.GitServer.Retries, "git-server request retry count")
	f.DurationVar(&o.GitServer.CacheTTL, "git-server-cache-ttl", o.GitServer.CacheTTL, "git-server synchronization status cache TTL")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if o.API.Server == "" {
		return o, fmt.Errorf("apiserver is required")
	}
	if o.RpmRepo.ArtifactManagerAddr != "" {
		parsed, err := url.Parse(o.RpmRepo.ArtifactManagerAddr)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return o, fmt.Errorf("artifact-manager-addr must be an absolute http or https URL")
		}
	}
	if !o.API.InsecureSkipVerify && o.API.ServerCA == "" {
		return o, fmt.Errorf("apiserver-ca is required unless insecure-skip-verify is enabled")
	}
	if o.Manager.Controllers == "" || o.Manager.Workers <= 0 || o.Manager.ControllerMaxRetries < 0 || o.Manager.SlowRetryInitialDelay <= 0 || o.Manager.SlowRetryMaxDelay < o.Manager.SlowRetryInitialDelay || o.Manager.SlowRetryJitter < 0 || o.Manager.SlowRetryJitter >= 1 || o.Source.PollPageSize <= 0 || o.API.ClientQPS <= 0 || o.API.ClientBurst <= 0 || o.Source.PollPeriod <= 0 || o.Manager.CacheSyncTimeout <= 0 || o.Manager.ShutdownTimeout <= 0 || o.Source.SourceStaleThreshold <= 0 || o.API.RequestTimeout <= 0 || o.Source.ResyncPeriod < 0 || o.Health.Address == "" || o.Job.RunnerLostGracePeriod <= 0 || (o.Job.HistoryGCEnabled && o.Job.HistoryRetention <= 0) || o.Runner.HeartbeatTimeout <= 0 || o.Runner.StartupGracePeriod <= 0 || o.RpmRepo.MaxJobsPerBatch <= 0 || o.RpmRepo.MaxInputBytes <= 0 || o.RpmRepo.MaterializeRetryLimit <= 0 || o.RpmRepo.ArtifactManagerTimeout <= 0 || o.Snapshot.ResolveWorkers <= 0 || o.Snapshot.ResolveBudget <= 0 || o.Snapshot.SyncRequeueDelay <= 0 || o.Snapshot.FailureRetryLimit <= 0 || o.GitServer.Address == "" || o.GitServer.Timeout <= 0 || o.GitServer.Retries < 0 || o.GitServer.CacheTTL <= 0 {
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
