package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"controller-manager/pkg/client"
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
	if o.InsecureSkipVerify {
		log.Print("WARNING: TLS server certificate verification is disabled")
	}
	config, err := o.RESTConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	apiClient, err := client.New(config, o.RequestTimeout)
	if err != nil {
		log.Fatal(err)
	}
	watchFactory := source.NewWatchSourceFactory(func(gvr schema.GroupVersionResource) (source.WatchResource, error) {
		return apiClient.ResolveWatch(ctx, gvr)
	}, o.ResyncPeriod, o.SourceStaleThreshold)
	pollingFactory := source.NewPollingSourceFactory(apiClient.ListPage, int64(o.PollPageSize), o.SourceStaleThreshold)
	healthServer := health.New(o.HealthAddress)
	m, err := manager.New(map[string]manager.InitFunc{}, manager.Dependencies{Client: apiClient, WatchFactory: watchFactory, PollingFactory: pollingFactory}, manager.Config{Workers: o.Workers, Controllers: o.Controllers, CacheSyncTimeout: o.CacheSyncTimeout, ShutdownTimeout: o.ShutdownTimeout}, healthServer)
	if err != nil {
		log.Fatal(err)
	}
	if err := m.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
