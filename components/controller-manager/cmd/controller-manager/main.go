package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/clients/gitserver"
	buildcontroller "controller-manager/pkg/controllers/build"
	buildinfocontroller "controller-manager/pkg/controllers/buildinfo"
	jobcontroller "controller-manager/pkg/controllers/job"
	rpmrepocontroller "controller-manager/pkg/controllers/rpmrepo"
	runnercontroller "controller-manager/pkg/controllers/runner"
	snapshotcontroller "controller-manager/pkg/controllers/snapshot"
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
	apiClient, err := apiserver.New(config, o.API.RequestTimeout)
	if err != nil {
		log.Fatal(err)
	}
	watchFactory := source.NewWatchSourceFactory(func(gvr schema.GroupVersionResource) (source.WatchResource, error) {
		return apiClient.ResolveWatch(ctx, gvr)
	}, o.Source.ResyncPeriod, o.Source.SourceStaleThreshold)
	pollingFactory := source.NewPollingSourceFactory(apiClient.ListPage, int64(o.Source.PollPageSize), o.Source.SourceStaleThreshold)
	healthServer := health.New(o.Health.Address)
	gitClient, err := gitserver.New(gitserver.Config{Address: o.GitServer.Address, Timeout: o.GitServer.Timeout, Retries: o.GitServer.Retries, CacheTTL: o.GitServer.CacheTTL})
	if err != nil {
		log.Fatal(err)
	}
	initializers := map[string]manager.InitFunc{
		buildcontroller.Name:  buildcontroller.Initializer(buildcontroller.Config{PollPeriod: o.Source.PollPeriod, MaxRetries: o.Manager.ControllerMaxRetries}),
		jobcontroller.Name:    jobcontroller.Initializer(jobcontroller.Config{RunnerLostGracePeriod: o.Job.RunnerLostGracePeriod, HistoryGCEnabled: o.Job.HistoryGCEnabled, HistoryRetention: o.Job.HistoryRetention, MaxRetries: o.Manager.ControllerMaxRetries}),
		runnercontroller.Name: runnercontroller.Initializer(runnercontroller.Config{HeartbeatTimeout: o.Runner.HeartbeatTimeout, StartupGracePeriod: o.Runner.StartupGracePeriod, MaxRetries: o.Manager.ControllerMaxRetries}),
		rpmrepocontroller.Name: rpmrepocontroller.Initializer(rpmrepocontroller.Config{
			ArtifactManagerAddr:    o.RpmRepo.ArtifactManagerAddr,
			ArtifactManagerTimeout: o.RpmRepo.ArtifactManagerTimeout,
			MaxJobsPerBatch:        o.RpmRepo.MaxJobsPerBatch,
			MaterializeRetryLimit:  o.RpmRepo.MaterializeRetryLimit,
			PollPeriod:             o.Source.PollPeriod,
			MaxRetries:             o.Manager.ControllerMaxRetries,
			Backoff: rpmrepocontroller.Backoff{
				Initial: o.Manager.SlowRetryInitialDelay,
				Max:     o.Manager.SlowRetryMaxDelay,
				Jitter:  o.Manager.SlowRetryJitter,
			},
		}),
		snapshotcontroller.Name: snapshotcontroller.Initializer(snapshotcontroller.Config{PollPeriod: o.Source.PollPeriod, ResolveWorkers: o.Snapshot.ResolveWorkers, ResolveBudget: o.Snapshot.ResolveBudget, SyncRequeueDelay: o.Snapshot.SyncRequeueDelay, FailureLimit: o.Snapshot.FailureRetryLimit, MaxRetries: o.Manager.ControllerMaxRetries}, gitClient),
		buildinfocontroller.Name: buildinfocontroller.Initializer(buildinfocontroller.Config{PollPeriod: o.Source.PollPeriod, MaxRetries: o.Manager.ControllerMaxRetries,
			DcgPruneGrace: o.BuildInfo.DcgPruneGrace, RpmRepoReadyRetryLimit: o.BuildInfo.RpmRepoReadyRetryLimit,
			SnapshotReadyRetryLimit: o.BuildInfo.SnapshotReadyRetryLimit, SpecFileCacheSize: o.BuildInfo.SpecFileCacheSize,
			SpecParseEngine: o.BuildInfo.SpecParseEngine}, gitClient),
	}
	m, err := manager.New(initializers, manager.Dependencies{Client: apiClient, WatchFactory: watchFactory, PollingFactory: pollingFactory}, manager.Config{Workers: o.Manager.Workers, Controllers: o.Manager.Controllers, CacheSyncTimeout: o.Manager.CacheSyncTimeout, ShutdownTimeout: o.Manager.ShutdownTimeout, SlowRetryInitial: o.Manager.SlowRetryInitialDelay, SlowRetryMax: o.Manager.SlowRetryMaxDelay, SlowRetryJitter: o.Manager.SlowRetryJitter}, healthServer)
	if err != nil {
		log.Fatal(err)
	}
	if err := m.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
