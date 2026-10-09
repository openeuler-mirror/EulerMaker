package buildinfo

import (
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// Per-BuildInfo caches are keyed by namespace/name. Terminal objects and confirmed deletion invalidate entries
// immediately; polling deletions leave a tombstone that can be revoked during the grace period. Expired tombstones are
// swept or removed on access. Callers perform graph-building I/O outside the cache lock before storing results.

// cacheKey builds the namespace/name cache key.
func cacheKey(namespace, name string) string { return namespace + "/" + name }

// perBuildInfoCache is the lifecycle mechanism shared by dcgDict, rpmMetaSources and specDependsCache.
type perBuildInfoCache[V any] struct {
	mu    sync.RWMutex
	clock clock.Clock
	grace time.Duration
	items map[string]*perBuildInfoEntry[V]
}

type perBuildInfoEntry[V any] struct {
	value        V
	tombstoned   bool
	tombstonedAt time.Time
}

func newPerBuildInfoCache[V any](clk clock.Clock, grace time.Duration) *perBuildInfoCache[V] {
	return &perBuildInfoCache[V]{
		clock: clk,
		grace: grace,
		items: map[string]*perBuildInfoEntry[V]{},
	}
}

// Get returns a live cached value. Tombstoned entries are misses until OnAdd or OnUpdate revokes them; expired
// tombstones are removed on access.
func (c *perBuildInfoCache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	if entry.tombstoned {
		if c.expired(entry) {
			delete(c.items, key)
		}
		var zero V
		return zero, false
	}
	return entry.value, true
}

// Set upserts the value and clears any tombstone.
func (c *perBuildInfoCache[V]) Set(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = &perBuildInfoEntry[V]{value: value}
}

// Invalidate removes the entry immediately: terminal phase observed or re-get 404 during reconcile.
func (c *perBuildInfoCache[V]) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// Tombstone records an OnDelete: the value is retained for the grace period so a spurious delete does not trigger a
// rebuild storm. Tombstoning a key with no entry is a no-op.
func (c *perBuildInfoCache[V]) Tombstone(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok || entry.tombstoned {
		return
	}
	entry.tombstoned = true
	entry.tombstonedAt = c.clock.Now()
}

// RevokeTombstone clears a pending tombstone on OnAdd/OnUpdate. An expired tombstone is swept instead of revoked.
func (c *perBuildInfoCache[V]) RevokeTombstone(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok || !entry.tombstoned {
		return
	}
	if c.expired(entry) {
		delete(c.items, key)
		return
	}
	entry.tombstoned = false
	entry.tombstonedAt = time.Time{}
}

// SweepExpired removes all expired tombstones and returns the removal count. Called by the controller sweeper.
func (c *perBuildInfoCache[V]) SweepExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for key, entry := range c.items {
		if entry.tombstoned && c.expired(entry) {
			delete(c.items, key)
			removed++
		}
	}
	return removed
}

// Len reports the live (non-tombstoned) entry count, for tests and metrics.
func (c *perBuildInfoCache[V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, entry := range c.items {
		if !entry.tombstoned {
			n++
		}
	}
	return n
}

func (c *perBuildInfoCache[V]) expired(entry *perBuildInfoEntry[V]) bool {
	return c.clock.Since(entry.tombstonedAt) >= c.grace
}
