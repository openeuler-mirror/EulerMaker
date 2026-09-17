package build

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
)

const Name = "build"

// nonTerminalBuildFieldSelector keeps the polling source focused on Builds that still need work.
const nonTerminalBuildFieldSelector = "status.phase!=Success,status.phase!=Failed,status.phase!=Aborted,status.phase!=Skipped"

type Config struct {
	PollPeriod time.Duration
	MaxRetries int
}

func (c Config) validate() error {
	if c.PollPeriod <= 0 || c.MaxRetries < 0 {
		return fmt.Errorf("Build controller poll period must be positive and retry count must not be negative")
	}
	return nil
}

type Controller struct {
	*controller.BaseController
	builds      source.Source
	client      Client
	clock       clock.Clock
	config      Config
	startupOnce sync.Once
}

func New(builds source.Source, client Client, clk clock.Clock, config Config, options ...controller.Option) (*Controller, error) {
	if builds == nil || client == nil || clk == nil {
		return nil, fmt.Errorf("Build source, API client and clock are required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	c := &Controller{builds: builds, client: client, clock: clk, config: config}
	base, err := controller.New(Name, c.sync, config.MaxRetries, options...)
	if err != nil {
		return nil, err
	}
	c.BaseController = base
	if err := builds.AddEventHandler(source.ResourceEventHandlerFuncs{AddFunc: c.onAdd, UpdateFunc: c.onUpdate, DeleteFunc: c.onDelete}); err != nil {
		return nil, fmt.Errorf("register Build handler: %w", err)
	}
	return c, nil
}

func Initializer(config Config) manager.InitFunc {
	return func(_ context.Context, init manager.InitContext) (controller.Controller, bool, error) {
		if err := config.validate(); err != nil {
			return nil, false, err
		}
		builds, err := init.Dependencies.PollingFactory.ForResource(
			source.BuildsGVR,
			config.PollPeriod,
			metav1.ListOptions{FieldSelector: nonTerminalBuildFieldSelector},
		)
		if err != nil {
			return nil, false, err
		}
		value, err := New(builds, newAPIClient(init.Dependencies.Client), clock.RealClock{}, config,
			controller.WithSlowRetry(init.Config.SlowRetryInitial, init.Config.SlowRetryMax, init.Config.SlowRetryJitter))
		return value, err == nil, err
	}
}

func (c *Controller) onAdd(obj runtime.Object) { c.enqueueBuild(obj) }

func (c *Controller) onUpdate(_, newObj runtime.Object) { c.enqueueBuild(newObj) }

// onDelete only logs: an object leaving the polling snapshot does not mean it was deleted.
func (c *Controller) onDelete(obj runtime.Object) {
	build, ok := obj.(*ebsv1.Build)
	if !ok || build == nil || build.Name == "" || build.Namespace == "" {
		log.Printf("controller=%s reason=UnexpectedBuildEvent type=%T", Name, obj)
		return
	}
	log.Printf("controller=%s key=%q uid=%q reason=BuildLeftPollingSnapshot", Name, build.Namespace+"/"+build.Name, build.UID)
}

func (c *Controller) enqueueBuild(obj runtime.Object) {
	build, ok := obj.(*ebsv1.Build)
	if !ok || build == nil || build.Name == "" || build.Namespace == "" {
		log.Printf("controller=%s reason=UnexpectedBuildEvent type=%T", Name, obj)
		return
	}
	c.Enqueue(build.Namespace + "/" + build.Name)
}

// logReconciledAfterStart emits one recovery marker per process after the first successful reconcile.
func (c *Controller) logReconciledAfterStart(build *ebsv1.Build) {
	c.startupOnce.Do(func() {
		log.Printf("controller=%s event=reconciled-after-start key=%q phase=%q stage=%q", Name, build.Namespace+"/"+build.Name, build.Status.Phase, build.Status.Stage)
	})
}
