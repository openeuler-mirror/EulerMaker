package runner

import (
	"context"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

const Name = "runner"

type Config struct {
	HeartbeatTimeout   time.Duration
	StartupGracePeriod time.Duration
	MaxRetries         int
}

func (c Config) validate() error {
	if c.HeartbeatTimeout <= 0 || c.StartupGracePeriod <= 0 || c.MaxRetries < 0 {
		return fmt.Errorf("Runner controller timeouts and retry count are invalid")
	}
	return nil
}

type Controller struct {
	*controller.BaseController
	runners source.CachedSource
	client  Client
	clock   clock.Clock
	config  Config
}

func New(runners source.CachedSource, client Client, clk clock.Clock, config Config) (*Controller, error) {
	return newController(runners, client, clk, config)
}

func newController(runners source.CachedSource, client Client, clk clock.Clock, config Config, options ...controller.Option) (*Controller, error) {
	if runners == nil || client == nil || clk == nil {
		return nil, fmt.Errorf("Runner source, client and clock are required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	c := &Controller{runners: runners, client: client, clock: clk, config: config}
	base, err := controller.New(Name, c.sync, config.MaxRetries, options...)
	if err != nil {
		return nil, err
	}
	c.BaseController = base
	if err := runners.AddEventHandler(source.ResourceEventHandlerFuncs{AddFunc: c.onAdd, UpdateFunc: c.onUpdate, DeleteFunc: c.onDelete}); err != nil {
		return nil, fmt.Errorf("register Runner handler: %w", err)
	}
	return c, nil
}

func Initializer(config Config) manager.InitFunc {
	return func(_ context.Context, init manager.InitContext) (controller.Controller, bool, error) {
		runners, err := init.Dependencies.WatchFactory.ForResource(source.RunnersGVR)
		if err != nil {
			return nil, false, err
		}
		value, err := newController(runners, newAPIClient(init.Dependencies.Client), clock.RealClock{}, config,
			controller.WithSlowRetry(init.Config.SlowRetryInitial, init.Config.SlowRetryMax, init.Config.SlowRetryJitter))
		return value, err == nil, err
	}
}

func (c *Controller) onAdd(obj runtime.Object) {
	runner, ok := obj.(*ebsv1.Runner)
	if !ok || runner == nil || runner.Name == "" {
		log.Printf("controller=%s reason=UnexpectedRunnerEvent type=%T", Name, obj)
		return
	}
	c.Enqueue(runner.Name)
}

func (c *Controller) onUpdate(oldObj, newObj runtime.Object) {
	oldRunner, oldOK := oldObj.(*ebsv1.Runner)
	newRunner, newOK := newObj.(*ebsv1.Runner)
	if !oldOK || !newOK || oldRunner == nil || newRunner == nil || oldRunner.Name != newRunner.Name || newRunner.Name == "" {
		log.Printf("controller=%s reason=UnexpectedRunnerUpdate old=%T new=%T", Name, oldObj, newObj)
		return
	}
	if shouldEnqueueRunnerUpdate(oldRunner, newRunner) {
		c.Enqueue(newRunner.Name)
	}
}

func (c *Controller) onDelete(runtime.Object) {}

func shouldEnqueueRunnerUpdate(oldRunner, newRunner *ebsv1.Runner) bool {
	return oldRunner.UID != newRunner.UID ||
		oldRunner.ResourceVersion == newRunner.ResourceVersion ||
		deletionTimestampChanged(oldRunner.DeletionTimestamp, newRunner.DeletionTimestamp) ||
		oldRunner.Status.Phase != newRunner.Status.Phase ||
		!oldRunner.Status.Heartbeat.Equal(&newRunner.Status.Heartbeat)
}

func deletionTimestampChanged(oldTime, newTime *metav1.Time) bool {
	if oldTime == nil || newTime == nil {
		return oldTime != newTime
	}
	return !oldTime.Equal(newTime)
}
