package controller

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"runtime/debug"
	"sync"
	"time"

	clientpkg "controller-manager/pkg/client"
	"k8s.io/client-go/util/workqueue"
	clockutils "k8s.io/utils/clock"
)

type Controller interface {
	Name() string
	Run(context.Context, int) error
}
type HealthChecker interface {
	Check(context.Context) error
}

type HealthCheckFunc func(context.Context) error

func (f HealthCheckFunc) Check(ctx context.Context) error { return f(ctx) }

type HealthCheckable interface {
	HealthChecker() HealthChecker
}

type ReconcileResult struct {
	Requeue      bool
	RequeueAfter time.Duration
}

func (r ReconcileResult) Valid() bool {
	return r.RequeueAfter >= 0 && !(r.Requeue && r.RequeueAfter > 0)
}

type SyncFunc func(context.Context, string) (ReconcileResult, error)

type BaseController struct {
	name        string
	queue       workqueue.RateLimitingInterface
	sync        SyncFunc
	maxRetries  int
	slowInitial time.Duration
	slowMax     time.Duration
	slowJitter  float64
	slowKeys    map[string]int
	slowMu      sync.Mutex
	jitter      func(time.Duration, float64) time.Duration
	queueClock  clockutils.WithTicker
	runOnce     sync.Once
	runErr      error
}

type Option func(*BaseController) error

func WithSlowRetry(initial, maximum time.Duration, jitter float64) Option {
	return func(c *BaseController) error {
		if initial <= 0 || maximum < initial || jitter < 0 || jitter >= 1 {
			return fmt.Errorf("slow retry delays must be valid and jitter must be in [0, 1)")
		}
		c.slowInitial = initial
		c.slowMax = maximum
		c.slowJitter = jitter
		return nil
	}
}

func WithJitter(fn func(time.Duration, float64) time.Duration) Option {
	return func(c *BaseController) error {
		if fn == nil {
			return fmt.Errorf("jitter function is required")
		}
		c.jitter = fn
		return nil
	}
}

func WithClock(clock clockutils.WithTicker) Option {
	return func(c *BaseController) error {
		if clock == nil {
			return fmt.Errorf("queue clock is required")
		}
		c.queueClock = clock
		return nil
	}
}

func New(name string, syncFn SyncFunc, maxRetries int, options ...Option) (*BaseController, error) {
	if name == "" || syncFn == nil || maxRetries < 0 {
		return nil, fmt.Errorf("valid name, sync function and retry count are required")
	}
	c := &BaseController{
		name:        name,
		sync:        syncFn,
		maxRetries:  maxRetries,
		slowInitial: 30 * time.Second,
		slowMax:     15 * time.Minute,
		slowJitter:  0.2,
		slowKeys:    make(map[string]int),
		queueClock:  clockutils.RealClock{},
		jitter: func(delay time.Duration, factor float64) time.Duration {
			return time.Duration(float64(delay) * (1 - factor + rand.Float64()*2*factor))
		},
	}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("controller option is required")
		}
		if err := option(c); err != nil {
			return nil, err
		}
	}
	c.queue = workqueue.NewRateLimitingQueueWithConfig(workqueue.DefaultControllerRateLimiter(), workqueue.RateLimitingQueueConfig{Name: name, Clock: c.queueClock})
	return c, nil
}
func (c *BaseController) Name() string { return c.name }
func (c *BaseController) Enqueue(key string) {
	if key != "" {
		c.queue.Add(key)
	}
}
func (c *BaseController) Queue() workqueue.RateLimitingInterface { return c.queue }
func (c *BaseController) Run(ctx context.Context, workers int) error {
	if workers <= 0 {
		return fmt.Errorf("workers must be positive")
	}
	ran := false
	c.runOnce.Do(func() { ran = true; c.runErr = c.run(ctx, workers) })
	if !ran {
		return fmt.Errorf("controller %s already started", c.name)
	}
	return c.runErr
}
func (c *BaseController) run(ctx context.Context, workers int) error {
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	c.queue.ShutDownWithDrain()
	wg.Wait()
	return nil
}
func (c *BaseController) processNext(ctx context.Context) bool {
	item, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(item)
	key, ok := item.(string)
	if !ok {
		c.queue.Forget(item)
		log.Printf("controller=%s result=invalid-key type=%T", c.name, item)
		return true
	}
	result, err := c.callSync(ctx, key)
	if err != nil && result != (ReconcileResult{}) {
		log.Printf("controller=%s key=%s result=invalid-result-with-error", c.name, key)
	}
	switch {
	case ctx.Err() != nil:
		c.queue.Forget(item)
		return false
	case IsPermanent(err):
		c.clearSlowRetry(key)
		c.queue.Forget(item)
		log.Printf("controller=%s key=%s result=permanent-error error=%v", c.name, key, err)
	case err != nil && clientpkg.RetryAfter(err) > 0:
		delay := clientpkg.RetryAfter(err)
		c.queue.Forget(item)
		c.queue.AddAfter(item, delay)
	case err != nil && c.isSlowRetry(key):
		c.queue.Forget(item)
		c.addSlowRetry(key)
	case err != nil && c.queue.NumRequeues(item) < c.maxRetries:
		c.queue.AddRateLimited(item)
	case err != nil:
		c.queue.Forget(item)
		c.enterSlowRetry(key)
		c.addSlowRetry(key)
		log.Printf("controller=%s key=%s result=max-retries error=%v", c.name, key, err)
	case !result.Valid():
		c.clearSlowRetry(key)
		c.queue.Forget(item)
		log.Printf("controller=%s key=%s result=invalid-result requeue=%t requeue-after=%s", c.name, key, result.Requeue, result.RequeueAfter)
	case result.RequeueAfter > 0:
		c.clearSlowRetry(key)
		c.queue.Forget(item)
		c.queue.AddAfter(item, result.RequeueAfter)
	case result.Requeue:
		c.clearSlowRetry(key)
		c.queue.Forget(item)
		c.queue.Add(item)
	default:
		c.clearSlowRetry(key)
		c.queue.Forget(item)
	}
	return true
}

func (c *BaseController) isSlowRetry(key string) bool {
	c.slowMu.Lock()
	defer c.slowMu.Unlock()
	_, exists := c.slowKeys[key]
	return exists
}

func (c *BaseController) enterSlowRetry(key string) {
	c.slowMu.Lock()
	defer c.slowMu.Unlock()
	if _, exists := c.slowKeys[key]; !exists {
		c.slowKeys[key] = 0
	}
}

func (c *BaseController) addSlowRetry(key string) {
	c.slowMu.Lock()
	retries, exists := c.slowKeys[key]
	if !exists {
		c.slowMu.Unlock()
		return
	}
	delay := c.slowRetryDelay(retries)
	c.slowKeys[key] = retries + 1
	c.slowMu.Unlock()
	c.queue.AddAfter(key, c.jitter(delay, c.slowJitter))
}

func (c *BaseController) slowRetryDelay(retries int) time.Duration {
	delay := c.slowInitial
	for i := 0; i < retries && delay < c.slowMax; i++ {
		if delay > c.slowMax/2 {
			return c.slowMax
		}
		delay *= 2
	}
	if delay > c.slowMax {
		return c.slowMax
	}
	return delay
}

func (c *BaseController) clearSlowRetry(key string) {
	c.slowMu.Lock()
	delete(c.slowKeys, key)
	c.slowMu.Unlock()
}
func (c *BaseController) callSync(ctx context.Context, key string) (result ReconcileResult, err error) {
	started := time.Now()
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("panic: %v", value)
			log.Printf("controller=%s key=%s panic=%v stack=%s", c.name, key, value, debug.Stack())
		}
		log.Printf("controller=%s key=%s duration=%s error=%v", c.name, key, time.Since(started), err)
	}()
	return c.sync(ctx, key)
}
