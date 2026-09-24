package snapshot

import (
	"context"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

const Name = "snapshot"

type Config struct {
	PollPeriod       time.Duration
	ResolveWorkers   int
	ResolveBudget    time.Duration
	SyncRequeueDelay time.Duration
	FailureLimit     int
	MaxRetries       int
}

func (c Config) validate() error {
	if c.PollPeriod <= 0 || c.ResolveWorkers <= 0 || c.ResolveBudget <= 0 || c.SyncRequeueDelay <= 0 || c.FailureLimit <= 0 || c.MaxRetries < 0 {
		return fmt.Errorf("Snapshot controller periods, worker counts and retry limits are invalid")
	}
	return nil
}

type Controller struct {
	*controller.BaseController
	snapshots source.Source
	client    Client
	gitClient gitserver.GitServerClient
	clock     clock.Clock
	config    Config
	failures  *failureTracker
}

func New(snapshots source.Source, client Client, gitClient gitserver.GitServerClient, clk clock.Clock, config Config) (*Controller, error) {
	return newController(snapshots, client, gitClient, clk, config)
}

func newController(snapshots source.Source, client Client, gitClient gitserver.GitServerClient, clk clock.Clock, config Config, options ...controller.Option) (*Controller, error) {
	if snapshots == nil || client == nil || gitClient == nil || clk == nil {
		return nil, fmt.Errorf("Snapshot source, API client, git-server client and clock are required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	c := &Controller{snapshots: snapshots, client: client, gitClient: gitClient, clock: clk, config: config, failures: newFailureTracker()}
	base, err := controller.New(Name, c.sync, config.MaxRetries, options...)
	if err != nil {
		return nil, err
	}
	c.BaseController = base
	if err := snapshots.AddEventHandler(source.ResourceEventHandlerFuncs{AddFunc: c.onAdd, UpdateFunc: c.onUpdate}); err != nil {
		return nil, fmt.Errorf("register Snapshot handler: %w", err)
	}
	return c, nil
}

func Initializer(config Config, gitClient gitserver.GitServerClient) manager.InitFunc {
	return func(_ context.Context, init manager.InitContext) (controller.Controller, bool, error) {
		snapshots, err := init.Dependencies.PollingFactory.ForResource(source.SnapshotsGVR, config.PollPeriod, metav1.ListOptions{FieldSelector: "status.phase!=Active"})
		if err != nil {
			return nil, false, err
		}
		value, err := newController(snapshots, newAPIClient(init.Dependencies.Client), gitClient, clock.RealClock{}, config,
			controller.WithSlowRetry(init.Config.SlowRetryInitial, init.Config.SlowRetryMax, init.Config.SlowRetryJitter))
		return value, err == nil, err
	}
}

func (c *Controller) onAdd(obj runtime.Object) { c.enqueueSnapshot(obj) }

func (c *Controller) onUpdate(_, newObj runtime.Object) { c.enqueueSnapshot(newObj) }

func (c *Controller) enqueueSnapshot(obj runtime.Object) {
	snapshot, ok := obj.(*ebsv1.Snapshot)
	if !ok || snapshot == nil || snapshot.Name == "" || snapshot.Namespace == "" {
		log.Printf("controller=%s reason=UnexpectedSnapshotEvent type=%T", Name, obj)
		return
	}
	if snapshot.DeletionTimestamp == nil && (snapshot.Status.Phase == ebsv1.SnapshotPending || snapshot.Status.Phase == ebsv1.SnapshotProcessing) {
		c.Enqueue(snapshot.Namespace + "/" + snapshot.Name)
	}
}
