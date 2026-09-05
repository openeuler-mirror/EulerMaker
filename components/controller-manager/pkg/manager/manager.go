package manager

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	"golang.org/x/sync/errgroup"
)

type HealthServer interface {
	Run(context.Context) error
	SetReady(bool)
}
type Dependencies struct {
	Client         any
	WatchFactory   source.WatchSourceFactory
	PollingFactory source.PollingSourceFactory
}
type ControllerConfig struct{ Workers int }
type InitContext struct {
	Dependencies Dependencies
	Config       ControllerConfig
}
type InitFunc func(context.Context, InitContext) (controller.Controller, bool, error)
type Config struct {
	Workers                           int
	Controllers                       string
	CacheSyncTimeout, ShutdownTimeout time.Duration
}

type Manager struct {
	initializers map[string]InitFunc
	dependencies Dependencies
	config       Config
	health       HealthServer
}

func New(initializers map[string]InitFunc, dependencies Dependencies, config Config, health HealthServer) (*Manager, error) {
	if initializers == nil || dependencies.WatchFactory == nil || dependencies.PollingFactory == nil || health == nil {
		return nil, fmt.Errorf("initializers, source factories and health server are required")
	}
	if config.Workers <= 0 || config.Controllers == "" || config.CacheSyncTimeout <= 0 || config.ShutdownTimeout <= 0 {
		return nil, fmt.Errorf("workers and timeouts must be positive")
	}
	for name, initializer := range initializers {
		if name == "" || initializer == nil {
			return nil, fmt.Errorf("controller initializer name and function are required")
		}
	}
	return &Manager{initializers: initializers, dependencies: dependencies, config: config, health: health}, nil
}

func (m *Manager) Run(parent context.Context) error {
	controllers, err := m.initialize(parent)
	if err != nil {
		return err
	}
	sources := append(m.dependencies.WatchFactory.Sources(), m.dependencies.PollingFactory.Sources()...)
	runCtx, cancel := context.WithCancel(parent)
	defer cancel()
	group, groupCtx := errgroup.WithContext(runCtx)
	group.Go(func() error { return m.health.Run(groupCtx) })
	for _, current := range sources {
		item := current
		group.Go(func() error {
			if err := item.Run(groupCtx); err != nil {
				return fmt.Errorf("source %s: %w", item.Name(), err)
			}
			return nil
		})
	}
	if err := waitForSync(groupCtx, sources, m.config.CacheSyncTimeout); err != nil {
		cancel()
		return waitAfterCancel(group, m.config.ShutdownTimeout, err)
	}
	for _, current := range controllers {
		item := current
		group.Go(func() error {
			if err := item.Run(groupCtx, m.config.Workers); err != nil {
				return fmt.Errorf("controller %s: %w", item.Name(), err)
			}
			return nil
		})
	}
	m.health.SetReady(true)
	group.Go(func() error {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		defer m.health.SetReady(false)
		for {
			select {
			case <-groupCtx.Done():
				return nil
			case <-ticker.C:
				ready := true
				for _, item := range sources {
					if !item.Ready() {
						ready = false
						break
					}
				}
				m.health.SetReady(ready)
			}
		}
	})
	<-groupCtx.Done()
	m.health.SetReady(false)
	cancel()
	return waitAfterCancel(group, m.config.ShutdownTimeout, nil)
}

func (m *Manager) initialize(ctx context.Context) ([]controller.Controller, error) {
	enabled, err := selectControllers(m.config.Controllers, m.initializers)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(enabled))
	for name := range enabled {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]controller.Controller, 0, len(names))
	seen := make(map[string]struct{})
	for _, name := range names {
		item, active, err := enabled[name](ctx, InitContext{Dependencies: m.dependencies, Config: ControllerConfig{Workers: m.config.Workers}})
		if err != nil {
			return nil, fmt.Errorf("initialize controller %s: %w", name, err)
		}
		if !active {
			if item != nil {
				return nil, fmt.Errorf("disabled controller %s returned an instance", name)
			}
			continue
		}
		if item == nil || item.Name() == "" {
			return nil, fmt.Errorf("enabled controller %s returned no valid instance", name)
		}
		if _, exists := seen[item.Name()]; exists {
			return nil, fmt.Errorf("duplicate controller name %s", item.Name())
		}
		seen[item.Name()] = struct{}{}
		items = append(items, item)
	}
	return items, nil
}

func selectControllers(selection string, initializers map[string]InitFunc) (map[string]InitFunc, error) {
	selected := make(map[string]InitFunc)
	tokens := strings.Split(selection, ",")
	for _, raw := range tokens {
		if strings.TrimSpace(raw) == "*" {
			for name, initializer := range initializers {
				selected[name] = initializer
			}
			break
		}
	}
	for _, raw := range tokens {
		token := strings.TrimSpace(raw)
		if token == "" {
			return nil, fmt.Errorf("empty controller name in selection")
		}
		if token == "*" {
			continue
		}
		disable := strings.HasPrefix(token, "-")
		name := strings.TrimPrefix(token, "-")
		initializer, exists := initializers[name]
		if name == "" || !exists {
			return nil, fmt.Errorf("unknown controller %q", name)
		}
		if disable {
			delete(selected, name)
		} else {
			selected[name] = initializer
		}
	}
	return selected, nil
}

func waitForSync(ctx context.Context, sources []source.Source, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		all := true
		for _, item := range sources {
			if !item.HasSynced() {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("cache sync timed out after %s", timeout)
		case <-ticker.C:
		}
	}
}
func waitAfterCancel(group *errgroup.Group, timeout time.Duration, fallback error) error {
	done := make(chan error, 1)
	go func() { done <- group.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
		return fallback
	case <-timer.C:
		return fmt.Errorf("shutdown timed out after %s", timeout)
	}
}
