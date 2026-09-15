package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

type Counter struct {
	name  string
	help  string
	value atomic.Uint64
}

var registry = struct {
	sync.RWMutex
	counters map[string]*Counter
}{counters: make(map[string]*Counter)}

func NewCounter(name, help string) *Counter {
	if name == "" || help == "" {
		panic("metric name and help are required")
	}
	counter := &Counter{name: name, help: help}
	registry.Lock()
	defer registry.Unlock()
	if _, exists := registry.counters[name]; exists {
		panic("duplicate metric: " + name)
	}
	registry.counters[name] = counter
	return counter
}

func (c *Counter) Inc() { c.value.Add(1) }

func (c *Counter) Add(value uint64) { c.value.Add(value) }

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		registry.RLock()
		names := make([]string, 0, len(registry.counters))
		for name := range registry.counters {
			names = append(names, name)
		}
		sort.Strings(names)
		counters := make([]*Counter, 0, len(names))
		for _, name := range names {
			counters = append(counters, registry.counters[name])
		}
		registry.RUnlock()
		for _, counter := range counters {
			writeCounter(w, counter)
		}
	})
}

func writeCounter(w io.Writer, counter *Counter) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", counter.name, counter.help, counter.name, counter.name, counter.value.Load())
}
