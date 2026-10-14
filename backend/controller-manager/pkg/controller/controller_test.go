package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	clientpkg "controller-manager/pkg/clients/apiserver"
)

func TestControllerRetriesThenForgets(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	c, err := New("test", func(context.Context, string) (ReconcileResult, error) {
		if calls.Add(1) == 1 {
			return ReconcileResult{}, errors.New("retry")
		}
		close(done)
		return ReconcileResult{}, nil
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, 1) }()
	c.Enqueue("p/name")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("controller did not retry")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestReconcileResultValidation(t *testing.T) {
	tests := []struct {
		name  string
		value ReconcileResult
		valid bool
	}{
		{name: "zero", valid: true},
		{name: "immediate", value: ReconcileResult{Requeue: true}, valid: true},
		{name: "delayed", value: ReconcileResult{RequeueAfter: time.Second}, valid: true},
		{name: "both", value: ReconcileResult{Requeue: true, RequeueAfter: time.Second}},
		{name: "negative", value: ReconcileResult{RequeueAfter: -time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.value.Valid(); got != tt.valid {
				t.Fatalf("Valid()=%t, want %t", got, tt.valid)
			}
		})
	}
}

func TestControllerRequeueClearsRateLimit(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	c, err := New("requeue", func(context.Context, string) (ReconcileResult, error) {
		switch calls.Add(1) {
		case 1:
			return ReconcileResult{}, errors.New("retry")
		case 2:
			return ReconcileResult{Requeue: true}, nil
		default:
			close(done)
			return ReconcileResult{}, nil
		}
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, 1) }()
	c.Enqueue("key")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("controller did not immediately requeue")
	}
	if got := c.queue.NumRequeues("key"); got != 0 {
		t.Fatalf("NumRequeues=%d, want 0", got)
	}
}

func TestControllerHonorsWriteRetryAfter(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	c, err := New("retry-after", func(context.Context, string) (ReconcileResult, error) {
		if calls.Add(1) == 1 {
			return ReconcileResult{}, &clientpkg.WriteError{Outcome: clientpkg.WriteRejected, StatusCode: 429, RetryAfter: 10 * time.Millisecond, Err: errors.New("busy")}
		}
		close(done)
		return ReconcileResult{}, nil
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, 1) }()
	c.Enqueue("key")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("controller did not honor RetryAfter")
	}
	if got := c.queue.NumRequeues("key"); got != 0 {
		t.Fatalf("NumRequeues=%d, want 0", got)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c, err := New("test", func(context.Context, string) (ReconcileResult, error) {
		calls.Add(1)
		return ReconcileResult{}, NewPermanentError(errors.New("bad input"))
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx, 1); close(done) }()
	c.Enqueue("name")
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("controller did not stop")
	}
	if calls.Load() != 1 {
		t.Fatalf("permanent error retried %d times", calls.Load())
	}
}

func TestControllerEntersSlowRetryAndClearsItAfterSuccess(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	c, err := New("slow-retry", func(context.Context, string) (ReconcileResult, error) {
		if calls.Add(1) < 3 {
			return ReconcileResult{}, errors.New("temporary failure")
		}
		close(done)
		return ReconcileResult{}, nil
	}, 0, WithSlowRetry(time.Millisecond, 4*time.Millisecond, 0.2), WithJitter(func(delay time.Duration, _ float64) time.Duration { return delay }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, 1) }()
	c.Enqueue("key")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("controller did not continue slow retries")
	}
	if c.isSlowRetry("key") {
		t.Fatal("successful reconciliation did not clear slow retry state")
	}
}

func TestControllerSlowRetryDelayIsExponentialAndCapped(t *testing.T) {
	c, err := New("slow-delay", func(context.Context, string) (ReconcileResult, error) {
		return ReconcileResult{}, errors.New("temporary failure")
	}, 0, WithSlowRetry(time.Second, 4*time.Second, 0.2), WithJitter(func(delay time.Duration, _ float64) time.Duration { return delay }))
	if err != nil {
		t.Fatal(err)
	}
	for retries, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		if got := c.slowRetryDelay(retries); got != want {
			t.Fatalf("slowRetryDelay(%d)=%s, want %s", retries, got, want)
		}
	}
}

func TestWithSlowRetryRejectsInvalidDelays(t *testing.T) {
	if _, err := New("invalid", func(context.Context, string) (ReconcileResult, error) { return ReconcileResult{}, nil }, 0, WithSlowRetry(0, time.Second, 0.2)); err == nil {
		t.Fatal("zero initial delay was accepted")
	}
	if _, err := New("invalid", func(context.Context, string) (ReconcileResult, error) { return ReconcileResult{}, nil }, 0, WithSlowRetry(time.Second, time.Millisecond, 0.2)); err == nil {
		t.Fatal("maximum below initial delay was accepted")
	}
	if _, err := New("invalid", func(context.Context, string) (ReconcileResult, error) { return ReconcileResult{}, nil }, 0, WithSlowRetry(time.Second, time.Minute, 1)); err == nil {
		t.Fatal("jitter outside [0, 1) was accepted")
	}
}
