package source

import (
	"fmt"
	"net/url"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

var (
	ProjectsGVR        = schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "projects"}
	SnapshotsGVR       = schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "snapshots"}
	BuildsGVR          = schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "builds"}
	BuildInfosGVR      = schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "buildinfos"}
	BuildResourceConfigsGVR  = schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "buildresourceconfigs"}
	RpmReposGVR        = schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "rpmrepos"}
	JobsGVR            = schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "jobs"}
	RunnersGVR         = schema.GroupVersionResource{Group: "ebs", Version: "v1", Resource: "runners"}
)

type WatchResource struct {
	ListerWatcher cache.ListerWatcher
	ObjectType    runtime.Object
}
type WatchResolver func(schema.GroupVersionResource) (WatchResource, error)

type WatchSourceFactory interface {
	ForResource(schema.GroupVersionResource) (CachedSource, error)
	Sources() []Source
}
type PollingSourceFactory interface {
	ForResource(schema.GroupVersionResource, time.Duration, metav1.ListOptions) (Source, error)
	Sources() []Source
}

type watchFactory struct {
	mu            sync.Mutex
	frozen        bool
	resolver      WatchResolver
	resync, stale time.Duration
	sources       map[schema.GroupVersionResource]*WatchSource
}

func NewWatchSourceFactory(resolver WatchResolver, resync, stale time.Duration) WatchSourceFactory {
	return &watchFactory{resolver: resolver, resync: resync, stale: stale, sources: make(map[schema.GroupVersionResource]*WatchSource)}
}
func (f *watchFactory) ForResource(gvr schema.GroupVersionResource) (CachedSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen {
		return nil, ErrSourceStarted
	}
	if gvr != JobsGVR && gvr != RunnersGVR {
		return nil, ErrWatchUnsupported
	}
	if existing := f.sources[gvr]; existing != nil {
		return existing, nil
	}
	if f.resolver == nil {
		return nil, fmt.Errorf("watch resolver is required")
	}
	resource, err := f.resolver(gvr)
	if err != nil {
		return nil, err
	}
	s, err := NewWatchSource(gvr.String(), resource.ListerWatcher, resource.ObjectType, f.resync, f.stale)
	if err != nil {
		return nil, err
	}
	f.sources[gvr] = s
	return s, nil
}
func (f *watchFactory) Sources() []Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen = true
	return orderedWatchSources(f.sources)
}

type pollingFactory struct {
	mu       sync.Mutex
	frozen   bool
	list     ListFunc
	pageSize int64
	stale    time.Duration
	sources  map[pollingSourceKey]*PollingSource
}

type pollingSourceKey struct {
	gvr                          schema.GroupVersionResource
	labelSelector, fieldSelector string
}

func NewPollingSourceFactory(list ListFunc, pageSize int64, stale time.Duration) PollingSourceFactory {
	return &pollingFactory{list: list, pageSize: pageSize, stale: stale, sources: make(map[pollingSourceKey]*PollingSource)}
}
func (f *pollingFactory) ForResource(gvr schema.GroupVersionResource, period time.Duration, options metav1.ListOptions) (Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen {
		return nil, ErrSourceStarted
	}
	options = pollingListOptions(options)
	key := pollingSourceKey{gvr: gvr, labelSelector: options.LabelSelector, fieldSelector: options.FieldSelector}
	if existing := f.sources[key]; existing != nil {
		if err := existing.shortenPeriod(period); err != nil {
			return nil, err
		}
		return existing, nil
	}
	s, err := NewPollingSource(gvr, period, f.pageSize, f.stale, options, f.list)
	if err != nil {
		return nil, err
	}
	f.sources[key] = s
	return s, nil
}
func (f *pollingFactory) Sources() []Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen = true
	return orderedPollingSources(f.sources)
}

func orderedWatchSources(items map[schema.GroupVersionResource]*WatchSource) []Source {
	out := make([]Source, 0, 2)
	for _, gvr := range []schema.GroupVersionResource{JobsGVR, RunnersGVR} {
		if s := items[gvr]; s != nil {
			out = append(out, s)
		}
	}
	return out
}
func orderedPollingSources(items map[pollingSourceKey]*PollingSource) []Source {
	keys := make([]pollingSourceKey, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if pollingSourceName(keys[j].gvr, metav1.ListOptions{LabelSelector: keys[j].labelSelector, FieldSelector: keys[j].fieldSelector}) < pollingSourceName(keys[i].gvr, metav1.ListOptions{LabelSelector: keys[i].labelSelector, FieldSelector: keys[i].fieldSelector}) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	out := make([]Source, 0, len(keys))
	for _, key := range keys {
		out = append(out, items[key])
	}
	return out
}

func pollingListOptions(options metav1.ListOptions) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: options.LabelSelector, FieldSelector: options.FieldSelector}
}

func pollingSourceName(gvr schema.GroupVersionResource, options metav1.ListOptions) string {
	query := url.Values{}
	if options.LabelSelector != "" {
		query.Set("labelSelector", options.LabelSelector)
	}
	if options.FieldSelector != "" {
		query.Set("fieldSelector", options.FieldSelector)
	}
	if encoded := query.Encode(); encoded != "" {
		return gvr.String() + "?" + encoded
	}
	return gvr.String()
}
