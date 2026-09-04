package scheduler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	kcache "k8s.io/client-go/tools/cache"
	"scheduler/pkg/action/allocate"
	schedcache "scheduler/pkg/cache"
	"scheduler/pkg/client"
	"scheduler/pkg/framework"
	"scheduler/pkg/options"
	"scheduler/pkg/plugin"
	"scheduler/pkg/queue"
)

type listerWatcher struct {
	ctx   context.Context
	list  func(context.Context, metav1.ListOptions) (runtime.Object, error)
	watch func(context.Context, metav1.ListOptions) (watch.Interface, error)
}

func (l *listerWatcher) List(opts metav1.ListOptions) (runtime.Object, error) {
	return l.list(l.ctx, opts)
}
func (l *listerWatcher) Watch(opts metav1.ListOptions) (watch.Interface, error) {
	return l.watch(l.ctx, opts)
}

type Scheduler struct {
	options                             options.Options
	client                              client.Interface
	cache                               *schedcache.SchedulerCache
	queue                               *queue.Queue
	jobInformer, runnerInformer         kcache.SharedIndexInformer
	jobRegistration, runnerRegistration kcache.ResourceEventHandlerRegistration
	action                              *allocate.Action
	ready, healthy                      atomic.Bool
	cycles                              atomic.Uint64
	metrics                             *metrics
}

func New(ctx context.Context, o options.Options, c client.Interface) (*Scheduler, error) {
	if c == nil {
		return nil, fmt.Errorf("client is required")
	}
	sc := schedcache.New(o.AssumeTimeout)
	q := queue.New(o.BackoffInitial, o.BackoffMax)
	s := &Scheduler{options: o, client: c, cache: sc, queue: q, metrics: newMetrics()}
	s.action = allocate.New(sc, c.Jobs())
	jlw := &listerWatcher{ctx: ctx, list: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) { return c.Jobs().List(ctx, o) }, watch: c.Jobs().Watch}
	rlw := &listerWatcher{ctx: ctx, list: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
		return c.Runners().List(ctx, o)
	}, watch: c.Runners().Watch}
	s.jobInformer = kcache.NewSharedIndexInformer(jlw, &ebsv1.Job{}, o.ResyncPeriod, kcache.Indexers{kcache.NamespaceIndex: kcache.MetaNamespaceIndexFunc, "status.runner": func(obj any) ([]string, error) { return []string{obj.(*ebsv1.Job).Status.Runner}, nil }, "status.phase": func(obj any) ([]string, error) { return []string{obj.(*ebsv1.Job).Status.Phase}, nil }})
	s.runnerInformer = kcache.NewSharedIndexInformer(rlw, &ebsv1.Runner{}, o.ResyncPeriod, kcache.Indexers{})
	var err error
	s.jobRegistration, err = s.jobInformer.AddEventHandler(kcache.ResourceEventHandlerFuncs{AddFunc: s.onJobAdd, UpdateFunc: s.onJobUpdate, DeleteFunc: s.onJobDelete})
	if err != nil {
		return nil, err
	}
	s.runnerRegistration, err = s.runnerInformer.AddEventHandler(kcache.ResourceEventHandlerFuncs{AddFunc: s.onRunnerAdd, UpdateFunc: s.onRunnerUpdate, DeleteFunc: s.onRunnerDelete})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Scheduler) onJobAdd(obj any) {
	job, ok := obj.(*ebsv1.Job)
	if !ok {
		return
	}
	s.cache.UpsertJob(job)
	s.queue.Add(job)
}
func schedulingJobChanged(a, b *ebsv1.Job) bool {
	return a.UID != b.UID || a.Spec.Priority != b.Spec.Priority || !reflect.DeepEqual(a.Spec, b.Spec) || a.Status.Phase != b.Status.Phase || a.Status.Runner != b.Status.Runner
}
func (s *Scheduler) onJobUpdate(oldObj, newObj any) {
	old, ok1 := oldObj.(*ebsv1.Job)
	job, ok2 := newObj.(*ebsv1.Job)
	if !ok1 || !ok2 {
		return
	}
	s.cache.UpsertJob(job)
	if schedulingJobChanged(old, job) {
		s.queue.Add(job)
		if old.Status.Phase == "Running" || job.Status.Phase == "Running" {
			s.queue.ActivateAll()
		}
	}
}
func tombstone(obj any) (any, bool) {
	if t, ok := obj.(kcache.DeletedFinalStateUnknown); ok {
		return t.Obj, true
	}
	return obj, true
}
func (s *Scheduler) onJobDelete(obj any) {
	obj, _ = tombstone(obj)
	job, ok := obj.(*ebsv1.Job)
	if !ok {
		return
	}
	key := schedcache.JobKey(job)
	s.cache.DeleteJob(key, job.UID)
	s.queue.Delete(key, job.UID)
	if job.Status.Phase == "Running" {
		s.queue.ActivateAll()
	}
}
func (s *Scheduler) onRunnerAdd(obj any) {
	if r, ok := obj.(*ebsv1.Runner); ok {
		s.cache.UpsertRunner(r)
		s.queue.ActivateAll()
	}
}
func (s *Scheduler) onRunnerUpdate(oldObj, newObj any) {
	old, ok1 := oldObj.(*ebsv1.Runner)
	r, ok2 := newObj.(*ebsv1.Runner)
	if !ok1 || !ok2 {
		return
	}
	s.cache.UpsertRunner(r)
	if old.UID != r.UID || !reflect.DeepEqual(old.Labels, r.Labels) || !reflect.DeepEqual(old.Spec, r.Spec) || old.Status.Phase != r.Status.Phase || !reflect.DeepEqual(old.Status.Allocatable, r.Status.Allocatable) {
		s.queue.ActivateAll()
	}
}
func (s *Scheduler) onRunnerDelete(obj any) {
	obj, _ = tombstone(obj)
	if r, ok := obj.(*ebsv1.Runner); ok {
		s.cache.DeleteRunner(r.Name, r.UID)
	}
}

func (s *Scheduler) Run(ctx context.Context) error {
	s.healthy.Store(true)
	defer s.healthy.Store(false)
	server := s.serveHealth()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go s.jobInformer.Run(ctx.Done())
	go s.runnerInformer.Run(ctx.Done())
	if !kcache.WaitForCacheSync(ctx.Done(), s.jobInformer.HasSynced, s.runnerInformer.HasSynced, s.jobRegistration.HasSynced, s.runnerRegistration.HasSynced) {
		return fmt.Errorf("informer synchronization failed")
	}
	s.ready.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < s.options.Workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.worker(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); s.confirmAssumed(ctx) }()
	<-ctx.Done()
	s.ready.Store(false)
	s.queue.ShutDown()
	wg.Wait()
	return nil
}
func (s *Scheduler) worker(ctx context.Context) {
	for {
		item, ok := s.queue.Pop()
		if !ok {
			return
		}
		started := time.Now()
		snapshot, err := s.cache.Snapshot(item.Key, item.UID)
		var result *framework.CycleResult
		if err != nil {
			result = &framework.CycleResult{Code: framework.Conflict, JobKey: item.Key, JobUID: item.UID, Reason: "stale-queue-entry", Err: err, QueueAction: framework.QueueDone}
		} else {
			session := &framework.Session{CycleID: fmt.Sprintf("%d", s.cycles.Add(1)), Job: snapshot.Job, Requests: snapshot.Requests, Runners: snapshot.Runners, OpenedAt: time.Now(), FilterPlugins: plugin.DefaultFilters(), ScorePlugins: plugin.DefaultScores()}
			result = s.action.Execute(ctx, session)
		}
		s.metrics.recordCycle(result, time.Since(started))
		s.finish(item, result)
	}
}
func (s *Scheduler) finish(item *queue.QueuedJob, result *framework.CycleResult) {
	if result == nil || result.JobUID == "" || (result.QueueAction != framework.QueueDone && result.QueueAction != framework.QueueAddBackoff) {
		s.ready.Store(false)
		s.queue.Done(item)
		return
	}
	log.Printf("jobKey=%s jobUID=%s runner=%s result=%s reason=%s", result.JobKey, result.JobUID, result.RunnerName, result.Code, result.Reason)
	if result.QueueAction == framework.QueueAddBackoff {
		s.queue.AddBackoff(item, result.Err)
	} else {
		s.queue.Done(item)
	}
	if result.Reason == "terminal-client-error" {
		s.ready.Store(false)
		s.queue.ShutDown()
	}
}
func (s *Scheduler) confirmAssumed(ctx context.Context) {
	ticker := time.NewTicker(s.options.AssumeScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			records := s.cache.ClaimExpiredAssumed(now, s.options.AssumeRetryInterval, s.options.AssumeBatchSize)
			sem := make(chan struct{}, s.options.AssumeWorkers)
			var wg sync.WaitGroup
			for _, record := range records {
				record := record
				wg.Add(1)
				go func() {
					defer wg.Done()
					select {
					case sem <- struct{}{}:
						defer func() { <-sem }()
					case <-ctx.Done():
						return
					}
					s.confirmOne(ctx, record)
				}()
			}
			wg.Wait()
		}
	}
}
func (s *Scheduler) confirmOne(ctx context.Context, a *schedcache.AssumedJob) {
	job, err := s.client.Jobs().Get(ctx, project(a.JobKey), name(a.JobKey), metav1.GetOptions{})
	if err != nil {
		s.metrics.recordConfirm("error", false)
		return
	}
	if job != nil && job.UID == a.JobUID && job.Status.Phase == "Running" && job.Status.Runner == a.RunnerName {
		s.metrics.recordConfirm("running", false)
		return
	}
	if !s.cache.Forget(a.JobKey, a.JobUID, a.Generation) {
		s.metrics.recordConfirm("stale", false)
		return
	}
	s.metrics.recordConfirm("released", true)
	if job != nil && job.UID == a.JobUID && job.Status.Phase == "Pending" && job.Status.Runner == "" {
		s.queue.Add(job)
	}
}
func project(key string) string {
	for i, c := range key {
		if c == '/' {
			return key[:i]
		}
	}
	return ""
}
func name(key string) string {
	for i, c := range key {
		if c == '/' {
			return key[i+1:]
		}
	}
	return ""
}
func (s *Scheduler) serveHealth() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.healthy.Load() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		active, backoff, inflight := s.queue.Depths()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "scheduler_queue_depth{queue=\"active\"} %d\nscheduler_queue_depth{queue=\"backoff\"} %d\nscheduler_queue_depth{queue=\"inflight\"} %d\n", active, backoff, inflight)
		s.metrics.write(w, s.cache.AssumedCount())
	})
	server := &http.Server{Addr: s.options.HealthAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health server: %v", err)
			s.healthy.Store(false)
		}
	}()
	return server
}
