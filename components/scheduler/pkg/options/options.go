package options

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/rest"
)

type Options struct {
	APIServer, APIServerCA                                                                                           string
	InsecureSkipVerify                                                                                               bool
	Workers                                                                                                          int
	ResyncPeriod, BackoffInitial, BackoffMax, AssumeTimeout, AssumeScanInterval, AssumeRetryInterval, RequestTimeout time.Duration
	AssumeBatchSize, AssumeWorkers                                                                                   int
	ClientQPS                                                                                                        float64
	ClientBurst                                                                                                      int
	HealthAddress                                                                                                    string
}

func Parse(args []string) (Options, error) {
	o := Options{Workers: 4, ResyncPeriod: 60 * time.Second, BackoffInitial: time.Second, BackoffMax: 300 * time.Second, AssumeTimeout: 30 * time.Second, AssumeScanInterval: 5 * time.Second, AssumeRetryInterval: 5 * time.Second, AssumeBatchSize: 100, AssumeWorkers: 4, ClientQPS: 20, ClientBurst: 40, RequestTimeout: 30 * time.Second, HealthAddress: ":8080"}
	f := flag.NewFlagSet("scheduler", flag.ContinueOnError)
	f.StringVar(&o.APIServer, "apiserver", "", "ebs-apiserver address")
	f.StringVar(&o.APIServerCA, "apiserver-ca", "", "server CA file")
	f.BoolVar(&o.InsecureSkipVerify, "insecure-skip-verify", false, "skip server certificate verification (development only)")
	f.IntVar(&o.Workers, "workers", o.Workers, "scheduler workers")
	f.DurationVar(&o.ResyncPeriod, "resync-period", o.ResyncPeriod, "informer resync period")
	f.DurationVar(&o.BackoffInitial, "backoff-initial", o.BackoffInitial, "initial retry backoff")
	f.DurationVar(&o.BackoffMax, "backoff-max", o.BackoffMax, "maximum retry backoff")
	f.DurationVar(&o.AssumeTimeout, "assume-timeout", o.AssumeTimeout, "assumed bind confirmation timeout")
	f.DurationVar(&o.AssumeScanInterval, "assume-scan-interval", o.AssumeScanInterval, "assumed scan interval")
	f.DurationVar(&o.AssumeRetryInterval, "assume-retry-interval", o.AssumeRetryInterval, "assumed retry interval")
	f.IntVar(&o.AssumeBatchSize, "assume-batch-size", o.AssumeBatchSize, "assumed scan batch size")
	f.IntVar(&o.AssumeWorkers, "assume-workers", o.AssumeWorkers, "assumed confirmation workers")
	f.Float64Var(&o.ClientQPS, "client-qps", o.ClientQPS, "API client QPS")
	f.IntVar(&o.ClientBurst, "client-burst", o.ClientBurst, "API client burst")
	f.DurationVar(&o.RequestTimeout, "request-timeout", o.RequestTimeout, "non-watch request timeout")
	f.StringVar(&o.HealthAddress, "health-address", o.HealthAddress, "health and metrics address")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if o.APIServer == "" {
		return o, fmt.Errorf("apiserver is required")
	}
	if !o.InsecureSkipVerify && o.APIServerCA == "" {
		return o, fmt.Errorf("apiserver-ca is required unless insecure-skip-verify is enabled")
	}
	if o.Workers <= 0 || o.AssumeWorkers <= 0 || o.AssumeBatchSize <= 0 || o.ClientQPS <= 0 || o.ClientBurst <= 0 {
		return o, fmt.Errorf("worker, batch, QPS, and burst values must be positive")
	}
	if o.AssumeScanInterval <= 0 || o.AssumeScanInterval > o.AssumeTimeout || o.BackoffInitial <= 0 || o.BackoffMax < o.BackoffInitial || o.RequestTimeout <= 0 {
		return o, fmt.Errorf("invalid timeout or backoff configuration")
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
