package buildinfo

import (
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
)

var testStart = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

func newTestCache(clk *clocktesting.FakeClock) *perBuildInfoCache[string] {
	return newPerBuildInfoCache[string](clk, 90*time.Second)
}

func TestCacheSetGet(t *testing.T) {
	clk := clocktesting.NewFakeClock(testStart)
	c := newTestCache(clk)
	if _, ok := c.Get("p/b"); ok {
		t.Fatal("Get on empty cache must miss")
	}
	c.Set("p/b", "dcg")
	if got, ok := c.Get("p/b"); !ok || got != "dcg" {
		t.Fatalf("Get = %q,%v, want dcg,true", got, ok)
	}
	c.Set("p/b", "dcg2")
	if got, _ := c.Get("p/b"); got != "dcg2" {
		t.Fatalf("overwrite Get = %q, want dcg2", got)
	}
}

func TestCacheInvalidateImmediate(t *testing.T) {
	clk := clocktesting.NewFakeClock(testStart)
	c := newTestCache(clk)
	c.Set("p/b", "dcg")
	c.Invalidate("p/b")
	if _, ok := c.Get("p/b"); ok {
		t.Fatal("Invalidate must remove immediately (terminal/404)")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
}

func TestCacheTombstoneRevokeWithinGrace(t *testing.T) {
	clk := clocktesting.NewFakeClock(testStart)
	c := newTestCache(clk)
	c.Set("p/b", "dcg")
	c.Tombstone("p/b")
	// Tombstoned entry is a miss but the value is retained.
	if _, ok := c.Get("p/b"); ok {
		t.Fatal("tombstoned entry must miss")
	}
	clk.Step(30 * time.Second)
	c.RevokeTombstone("p/b")
	if got, ok := c.Get("p/b"); !ok || got != "dcg" {
		t.Fatalf("revoked entry = %q,%v, want dcg,true", got, ok)
	}
}

func TestCacheTombstoneExpiresLazy(t *testing.T) {
	clk := clocktesting.NewFakeClock(testStart)
	c := newTestCache(clk)
	c.Set("p/b", "dcg")
	c.Tombstone("p/b")
	clk.Step(91 * time.Second)
	if _, ok := c.Get("p/b"); ok {
		t.Fatal("expired tombstone must miss")
	}
	if c.Len() != 0 {
		t.Fatalf("expired tombstone not swept lazily, Len = %d", c.Len())
	}
	// Too late to revoke after expiry.
	c.Set("p/c", "x")
	c.Tombstone("p/c")
	clk.Step(91 * time.Second)
	c.RevokeTombstone("p/c")
	if _, ok := c.Get("p/c"); ok {
		t.Fatal("revoke after expiry must not resurrect the entry")
	}
}

func TestCacheSweeper(t *testing.T) {
	clk := clocktesting.NewFakeClock(testStart)
	c := newTestCache(clk)
	c.Set("p/a", "1")
	c.Set("p/b", "2")
	c.Set("p/c", "3")
	c.Tombstone("p/a")
	c.Tombstone("p/b")
	if n := c.SweepExpired(); n != 0 {
		t.Fatalf("SweepExpired within grace = %d, want 0", n)
	}
	clk.Step(91 * time.Second)
	if n := c.SweepExpired(); n != 2 {
		t.Fatalf("SweepExpired = %d, want 2", n)
	}
	if got, ok := c.Get("p/c"); !ok || got != "3" {
		t.Fatal("live entry must survive the sweep")
	}
}

func TestCacheTombstoneMissingKeyNoop(t *testing.T) {
	clk := clocktesting.NewFakeClock(testStart)
	c := newTestCache(clk)
	c.Tombstone("p/absent")
	if c.Len() != 0 {
		t.Fatalf("tombstone on missing key created an entry, Len = %d", c.Len())
	}
}

func TestCacheSetClearsTombstone(t *testing.T) {
	clk := clocktesting.NewFakeClock(testStart)
	c := newTestCache(clk)
	c.Set("p/b", "old")
	c.Tombstone("p/b")
	c.Set("p/b", "new")
	if got, ok := c.Get("p/b"); !ok || got != "new" {
		t.Fatalf("Set over tombstone = %q,%v, want new,true", got, ok)
	}
}
