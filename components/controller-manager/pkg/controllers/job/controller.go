package job

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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
)

const Name = "job"

type Config struct {
	RunnerLostGracePeriod time.Duration
	HistoryGCEnabled      bool
	HistoryRetention      time.Duration
	MaxRetries            int
}

func (c Config) validate() error {
	if c.RunnerLostGracePeriod <= 0 || c.MaxRetries < 0 || (c.HistoryGCEnabled && c.HistoryRetention <= 0) {
		return fmt.Errorf("Job controller grace period, retention and retry count are invalid")
	}
	return nil
}

type LostRunnerObservation struct {
	JobUID    types.UID
	Runner    string
	FirstSeen time.Time
}

type Controller struct {
	*controller.BaseController
	jobs          source.CachedSource
	runners       source.CachedSource
	client        Client
	clock         clock.Clock
	config        Config
	index         *runnerIndex
	observations  map[string]LostRunnerObservation
	observationMu sync.Mutex
}

func New(jobs, runners source.CachedSource, client Client, clk clock.Clock, config Config) (*Controller, error) {
	return newController(jobs, runners, client, clk, config)
}

func newController(jobs, runners source.CachedSource, client Client, clk clock.Clock, config Config, options ...controller.Option) (*Controller, error) {
	if jobs == nil || runners == nil || client == nil || clk == nil {
		return nil, fmt.Errorf("Job and Runner sources, client and clock are required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	c := &Controller{jobs: jobs, runners: runners, client: client, clock: clk, config: config, index: newRunnerIndex(), observations: make(map[string]LostRunnerObservation)}
	base, err := controller.New(Name, c.sync, config.MaxRetries, options...)
	if err != nil {
		return nil, err
	}
	c.BaseController = base
	if err := jobs.AddEventHandler(source.ResourceEventHandlerFuncs{AddFunc: c.onJobAdd, UpdateFunc: c.onJobUpdate, DeleteFunc: c.onJobDelete}); err != nil {
		return nil, fmt.Errorf("register Job handler: %w", err)
	}
	if err := runners.AddEventHandler(source.ResourceEventHandlerFuncs{AddFunc: c.onRunnerAdd, UpdateFunc: c.onRunnerUpdate, DeleteFunc: c.onRunnerDelete}); err != nil {
		return nil, fmt.Errorf("register Runner handler: %w", err)
	}
	return c, nil
}

func Initializer(config Config) manager.InitFunc {
	return func(_ context.Context, init manager.InitContext) (controller.Controller, bool, error) {
		jobs, err := init.Dependencies.WatchFactory.ForResource(source.JobsGVR)
		if err != nil {
			return nil, false, err
		}
		runners, err := init.Dependencies.WatchFactory.ForResource(source.RunnersGVR)
		if err != nil {
			return nil, false, err
		}
		value, err := newController(jobs, runners, newAPIClient(init.Dependencies.Client), clock.RealClock{}, config,
			controller.WithSlowRetry(init.Config.SlowRetryInitial, init.Config.SlowRetryMax, init.Config.SlowRetryJitter))
		return value, err == nil, err
	}
}

func (c *Controller) onJobAdd(obj runtime.Object) {
	job, ok := obj.(*ebsv1.Job)
	if !ok || job == nil {
		log.Printf("controller=%s reason=UnexpectedJobEvent type=%T", Name, obj)
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(job)
	if err != nil {
		log.Printf("controller=%s reason=InvalidJobKey error=%v", Name, err)
		return
	}
	c.updateIndex(key, job)
	c.Enqueue(key)
}

func (c *Controller) onJobUpdate(oldObj, newObj runtime.Object) {
	oldJob, oldOK := oldObj.(*ebsv1.Job)
	newJob, newOK := newObj.(*ebsv1.Job)
	if !oldOK || !newOK || oldJob == nil || newJob == nil {
		log.Printf("controller=%s reason=UnexpectedJobUpdate old=%T new=%T", Name, oldObj, newObj)
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(newJob)
	if err != nil {
		return
	}
	c.updateIndex(key, newJob)
	if oldJob.UID != newJob.UID || oldJob.Status.Runner != newJob.Status.Runner || oldJob.Status.Phase != newJob.Status.Phase {
		c.clearObservation(key)
	}
	if oldJob.ResourceVersion == newJob.ResourceVersion || oldJob.UID != newJob.UID || oldJob.Status.Phase != newJob.Status.Phase ||
		oldJob.Status.Runner != newJob.Status.Runner || !oldJob.Status.EndTime.Time.Equal(newJob.Status.EndTime.Time) || deletionTimeChanged(oldJob, newJob) {
		c.Enqueue(key)
	}
}

func (c *Controller) onJobDelete(obj runtime.Object) {
	job, ok := obj.(*ebsv1.Job)
	if !ok || job == nil {
		return
	}
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(job)
	if err != nil {
		return
	}
	c.index.remove(key)
	c.clearObservation(key)
}

func (c *Controller) onRunnerAdd(obj runtime.Object) {
	runner, ok := obj.(*ebsv1.Runner)
	if ok && runner != nil {
		c.enqueueRunnerJobs(runner.Name)
	}
}

func (c *Controller) onRunnerUpdate(oldObj, newObj runtime.Object) {
	oldRunner, oldOK := oldObj.(*ebsv1.Runner)
	newRunner, newOK := newObj.(*ebsv1.Runner)
	if !oldOK || !newOK || oldRunner == nil || newRunner == nil {
		return
	}
	if oldRunner.UID != newRunner.UID || (oldRunner.Status.Phase == "Online") != (newRunner.Status.Phase == "Online") {
		c.enqueueRunnerJobs(newRunner.Name)
	}
}

func (c *Controller) onRunnerDelete(obj runtime.Object) {
	runner, ok := obj.(*ebsv1.Runner)
	if ok && runner != nil {
		c.enqueueRunnerJobs(runner.Name)
	}
}

func (c *Controller) updateIndex(key string, job *ebsv1.Job) {
	if !terminal(job.Status.Phase) && job.Status.Runner != "" {
		c.index.set(key, job.Status.Runner)
		return
	}
	c.index.remove(key)
}

func (c *Controller) enqueueRunnerJobs(runner string) {
	for _, key := range c.index.jobs(runner) {
		c.Enqueue(key)
	}
}

func (c *Controller) observation(key string, job *ebsv1.Job, now time.Time) LostRunnerObservation {
	c.observationMu.Lock()
	defer c.observationMu.Unlock()
	current, exists := c.observations[key]
	if !exists || current.JobUID != job.UID || current.Runner != job.Status.Runner {
		current = LostRunnerObservation{JobUID: job.UID, Runner: job.Status.Runner, FirstSeen: now}
		c.observations[key] = current
		runnerLostObservations.Inc()
		log.Printf("controller=%s key=%s uid=%s runner=%s reason=RunnerUnavailable deadline=%s", Name, key, job.UID, job.Status.Runner, now.Add(c.config.RunnerLostGracePeriod).UTC().Format(time.RFC3339))
	}
	return current
}

func (c *Controller) clearObservation(key string) {
	c.observationMu.Lock()
	delete(c.observations, key)
	c.observationMu.Unlock()
}

func terminal(phase string) bool {
	return phase == "Completed" || phase == "Failed" || phase == "Aborted"
}

func deletionTimeChanged(oldJob, newJob *ebsv1.Job) bool {
	if (oldJob.DeletionTimestamp == nil) != (newJob.DeletionTimestamp == nil) {
		return true
	}
	return oldJob.DeletionTimestamp != nil && !oldJob.DeletionTimestamp.Equal(newJob.DeletionTimestamp)
}

func processable(job *ebsv1.Job) bool {
	return job != nil && job.DeletionTimestamp == nil && job.Status.Phase == "Running" && job.Status.Runner != ""
}
