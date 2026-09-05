package controller

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"
)

type Controller interface {
	Name() string
	Run(context.Context, int) error
}
type SyncFunc func(context.Context, string) error

type BaseController struct {
	name       string
	queue      workqueue.RateLimitingInterface
	sync       SyncFunc
	maxRetries int
	runOnce    sync.Once
	runErr     error
}

func New(name string, syncFn SyncFunc, maxRetries int) (*BaseController, error) {
	if name == "" || syncFn == nil || maxRetries < 0 {
		return nil, fmt.Errorf("valid name, sync function and retry count are required")
	}
	return &BaseController{name: name, sync: syncFn, maxRetries: maxRetries, queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), name)}, nil
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
	err := c.callSync(ctx, key)
	switch {
	case err == nil:
		c.queue.Forget(item)
	case ctx.Err() != nil:
		c.queue.Forget(item)
		return false
	case IsPermanent(err):
		c.queue.Forget(item)
		log.Printf("controller=%s key=%s result=permanent-error error=%v", c.name, key, err)
	case c.queue.NumRequeues(item) < c.maxRetries:
		c.queue.AddRateLimited(item)
	default:
		c.queue.Forget(item)
		log.Printf("controller=%s key=%s result=max-retries error=%v", c.name, key, err)
	}
	return true
}
func (c *BaseController) callSync(ctx context.Context, key string) (err error) {
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
