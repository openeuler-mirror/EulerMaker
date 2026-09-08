package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"controller-manager/pkg/client"
	jobcontroller "controller-manager/pkg/controllers/job"
	runnercontroller "controller-manager/pkg/controllers/runner"
	"controller-manager/pkg/health"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/options"
	"controller-manager/pkg/source"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func main() {
	o, err := options.Parse(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if o.API.InsecureSkipVerify {
		log.Print("WARNING: TLS server certificate verification is disabled")
	}
	config, err := o.RESTConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	apiClient, err := client.New(config, o.API.RequestTimeout)
	if err != nil {
		log.Fatal(err)
	}
	watchFactory := source.NewWatchSourceFactory(func(gvr schema.GroupVersionResource) (source.WatchResource, error) {
		return apiClient.ResolveWatch(ctx, gvr)
	}, o.Source.ResyncPeriod, o.Source.SourceStaleThreshold)
	pollingFactory := source.NewPollingSourceFactory(apiClient.ListPage, int64(o.Source.PollPageSize), o.Source.SourceStaleThreshold)
	healthServer := health.New(o.Health.Address)
	initializers := map[string]manager.InitFunc{
		jobcontroller.Name:    jobcontroller.Initializer(jobcontroller.Config{RunnerLostGracePeriod: o.Job.RunnerLostGracePeriod, HistoryGCEnabled: o.Job.HistoryGCEnabled, HistoryRetention: o.Job.HistoryRetention, MaxRetries: o.Manager.ControllerMaxRetries}),
		runnercontroller.Name: runnercontroller.Initializer(runnercontroller.Config{HeartbeatTimeout: o.Runner.HeartbeatTimeout, StartupGracePeriod: o.Runner.StartupGracePeriod, MaxRetries: o.Manager.ControllerMaxRetries}),
	}
	m, err := manager.New(initializers, manager.Dependencies{Client: apiClient, WatchFactory: watchFactory, PollingFactory: pollingFactory}, manager.Config{Workers: o.Manager.Workers, Controllers: o.Manager.Controllers, CacheSyncTimeout: o.Manager.CacheSyncTimeout, ShutdownTimeout: o.Manager.ShutdownTimeout, SlowRetryInitial: o.Manager.SlowRetryInitialDelay, SlowRetryMax: o.Manager.SlowRetryMaxDelay, SlowRetryJitter: o.Manager.SlowRetryJitter}, healthServer)
	if err != nil {
		log.Fatal(err)
	}
	if err := m.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
