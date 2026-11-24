package limit

import (
	"sync"
	"time"
)

const (
	maxEntries    = 4096
	idleTTL       = 10 * time.Minute
	sweepInterval = time.Minute
)

type bucket struct {
	tokens float64
	last   time.Time
}

// Buckets is an in-process keyed token-bucket limiter.
type Buckets struct {
	mu        sync.Mutex
	rate      float64
	capacity  float64
	entries   map[string]bucket
	now       func() time.Time
	nextSweep time.Time
}

func New(rate float64, capacity int, now func() time.Time) *Buckets {
	if now == nil {
		now = time.Now
	}
	return &Buckets{
		rate:     rate,
		capacity: float64(capacity),
		entries:  make(map[string]bucket),
		now:      now,
	}
}

func (b *Buckets) Allow(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if !now.Before(b.nextSweep) {
		for key, entry := range b.entries {
			elapsed := now.Sub(entry.last)
			// Only discard buckets whose allowance has fully recovered.
			if elapsed >= idleTTL && entry.tokens+elapsed.Seconds()*b.rate >= b.capacity {
				delete(b.entries, key)
			}
		}
		b.nextSweep = now.Add(sweepInterval)
	}
	entry, found := b.entries[key]
	if !found {
		if len(b.entries) >= maxEntries {
			return false
		}
		entry = bucket{tokens: b.capacity, last: now}
	}
	if elapsed := now.Sub(entry.last).Seconds(); elapsed > 0 {
		entry.tokens += elapsed * b.rate
		if entry.tokens > b.capacity {
			entry.tokens = b.capacity
		}
	}
	entry.last = now
	if entry.tokens < 1 {
		b.entries[key] = entry
		return false
	}
	entry.tokens--
	b.entries[key] = entry
	return true
}
