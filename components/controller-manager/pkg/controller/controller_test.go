package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestControllerRetriesThenForgets(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	c, err := New("test", func(context.Context, string) error {
		if calls.Add(1) == 1 {
			return errors.New("retry")
		}
		close(done)
		return nil
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

func TestPermanentErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c, err := New("test", func(context.Context, string) error { calls.Add(1); return NewPermanentError(errors.New("bad input")) }, 3)
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
