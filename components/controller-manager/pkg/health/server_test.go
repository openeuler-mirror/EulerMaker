package health

import (
	"context"
	"errors"
	"strings"
	"testing"

	"controller-manager/pkg/controller"
)

func TestHealthCheckerRegistrationAndExecution(t *testing.T) {
	server := New(":0")
	if err := server.AddHealthChecker("healthy", controller.HealthCheckFunc(func(context.Context) error { return nil })); err != nil {
		t.Fatal(err)
	}
	if err := server.AddHealthChecker("broken", controller.HealthCheckFunc(func(context.Context) error { return errors.New("failed") })); err != nil {
		t.Fatal(err)
	}
	if err := server.AddHealthChecker("healthy", controller.HealthCheckFunc(func(context.Context) error { return nil })); err == nil {
		t.Fatal("duplicate checker registration succeeded")
	}
	failures := server.checkHealth(context.Background())
	if len(failures) != 1 || !strings.Contains(failures[0], "broken: failed") {
		t.Fatalf("unexpected failures: %v", failures)
	}
}

func TestHealthCheckerRegistrationFreezesAtStart(t *testing.T) {
	server := New(":0")
	server.mu.Lock()
	server.started = true
	server.mu.Unlock()
	if err := server.AddHealthChecker("late", controller.HealthCheckFunc(func(context.Context) error { return nil })); err == nil {
		t.Fatal("late checker registration succeeded")
	}
}
