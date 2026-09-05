package options

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/rest"
)

type Options struct {
	APIServer, APIServerCA, Controllers                                                               string
	InsecureSkipVerify                                                                                bool
	Workers, PollPageSize, ClientBurst                                                                int
	ClientQPS                                                                                         float64
	PollPeriod, CacheSyncTimeout, ShutdownTimeout, SourceStaleThreshold, RequestTimeout, ResyncPeriod time.Duration
	HealthAddress                                                                                     string
}

func Parse(args []string) (Options, error) {
	o := Options{Controllers: "*", Workers: 2, PollPageSize: 500, ClientQPS: 20, ClientBurst: 40, PollPeriod: 30 * time.Second, CacheSyncTimeout: 2 * time.Minute, ShutdownTimeout: 30 * time.Second, SourceStaleThreshold: 2 * time.Minute, RequestTimeout: 30 * time.Second, ResyncPeriod: 10 * time.Minute, HealthAddress: ":8080"}
	f := flag.NewFlagSet("controller-manager", flag.ContinueOnError)
	f.StringVar(&o.APIServer, "apiserver", "", "ebs-apiserver address")
	f.StringVar(&o.APIServerCA, "apiserver-ca", "", "server CA file")
	f.BoolVar(&o.InsecureSkipVerify, "insecure-skip-verify", false, "skip server certificate verification (development only)")
	f.StringVar(&o.Controllers, "controllers", o.Controllers, "controllers to enable (*, name, -name)")
	f.IntVar(&o.Workers, "workers", o.Workers, "workers per controller")
	f.DurationVar(&o.PollPeriod, "poll-period", o.PollPeriod, "default polling period")
	f.IntVar(&o.PollPageSize, "poll-page-size", o.PollPageSize, "polling list page size")
	f.DurationVar(&o.CacheSyncTimeout, "cache-sync-timeout", o.CacheSyncTimeout, "initial source sync timeout")
	f.DurationVar(&o.ShutdownTimeout, "shutdown-timeout", o.ShutdownTimeout, "graceful shutdown timeout")
	f.DurationVar(&o.SourceStaleThreshold, "source-stale-threshold", o.SourceStaleThreshold, "source readiness stale threshold")
	f.DurationVar(&o.RequestTimeout, "request-timeout", o.RequestTimeout, "non-watch request timeout")
	f.DurationVar(&o.ResyncPeriod, "resync-period", o.ResyncPeriod, "watch informer resync period")
	f.Float64Var(&o.ClientQPS, "client-qps", o.ClientQPS, "API client QPS")
	f.IntVar(&o.ClientBurst, "client-burst", o.ClientBurst, "API client burst")
	f.StringVar(&o.HealthAddress, "health-bind-address", o.HealthAddress, "health server address")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if o.APIServer == "" {
		return o, fmt.Errorf("apiserver is required")
	}
	if !o.InsecureSkipVerify && o.APIServerCA == "" {
		return o, fmt.Errorf("apiserver-ca is required unless insecure-skip-verify is enabled")
	}
	if o.Controllers == "" || o.Workers <= 0 || o.PollPageSize <= 0 || o.ClientQPS <= 0 || o.ClientBurst <= 0 || o.PollPeriod <= 0 || o.CacheSyncTimeout <= 0 || o.ShutdownTimeout <= 0 || o.SourceStaleThreshold <= 0 || o.RequestTimeout <= 0 || o.ResyncPeriod < 0 || o.HealthAddress == "" {
		return o, fmt.Errorf("workers, limits, periods, timeouts and addresses must be valid")
	}
	return o, nil
}
func (o Options) RESTConfig() (*rest.Config, error) {
	if o.APIServerCA != "" {
		if _, err := os.Stat(o.APIServerCA); err != nil {
			return nil, fmt.Errorf("read apiserver CA: %w", err)
		}
	}
	return &rest.Config{Host: o.APIServer, QPS: float32(o.ClientQPS), Burst: o.ClientBurst, TLSClientConfig: rest.TLSClientConfig{CAFile: o.APIServerCA, Insecure: o.InsecureSkipVerify}}, nil
}
