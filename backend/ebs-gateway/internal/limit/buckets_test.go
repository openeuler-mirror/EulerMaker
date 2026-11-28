package limit

import (
	"strconv"
	"testing"
	"time"
)

func TestRecoveredBucketsExpireWithoutResettingActiveAllowance(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(1, 1, func() time.Time { return now })
	b.Allow("idle")
	now = now.Add(sweepInterval)
	b.Allow("active")
	b.Allow("sweep")
	if _, found := b.entries["idle"]; found {
		t.Fatal("idle bucket retained")
	}
	if _, found := b.entries["active"]; !found {
		t.Fatal("active bucket removed")
	}
	if !b.Allow("idle") {
		t.Fatal("expired key cannot return")
	}

	// Periodic cleanup must retain debt while allowance is still recovering.
	slow := New(1.0/3600, 1, func() time.Time { return now })
	slow.Allow("slow")
	now = now.Add(sweepInterval)
	if slow.Allow("slow") {
		t.Fatal("expiration bypassed refill rate")
	}
}

func TestBucketCapacityReclaimsRecoveredBeforeLRU(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(1, 2, func() time.Time { return now })
	for i := 0; i < maxEntries; i++ {
		if !b.Allow(strconv.Itoa(i)) {
			t.Fatal("capacity reached too early")
		}
	}
	if !b.Allow("0") || b.Allow("0") {
		t.Fatal("existing key allowance changed at capacity")
	}
	// The oldest bucket is depleted; the others recover before the next periodic sweep.
	now = now.Add(time.Second)
	if !b.Allow("overflow") || len(b.entries) != 2 {
		t.Fatal("recovered capacity not reclaimed")
	}
	if _, found := b.entries["0"]; !found {
		t.Fatal("depleted bucket evicted despite recovered alternatives")
	}
	if !b.Allow("0") || b.Allow("0") {
		t.Fatal("cleanup reset the retained bucket's allowance")
	}
}

func TestBucketCapacityEvictsLeastRecentlyUsed(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(1, 1, func() time.Time { return now })
	for i := 0; i < maxEntries; i++ {
		if !b.Allow(strconv.Itoa(i)) {
			t.Fatal("capacity reached too early")
		}
		now = now.Add(time.Nanosecond)
	}
	// A rejected request still counts as an access, but must not reset its allowance.
	if b.Allow("0") {
		t.Fatal("depleted bucket accepted a request")
	}
	now = now.Add(time.Nanosecond)
	if !b.Allow("overflow") || len(b.entries) != maxEntries {
		t.Fatal("new key not admitted within capacity")
	}
	if _, found := b.entries["1"]; found {
		t.Fatal("least recently used bucket retained")
	}
	if _, found := b.entries["0"]; !found {
		t.Fatal("recently accessed bucket evicted")
	}
	if b.Allow("0") {
		t.Fatal("retained bucket's allowance reset")
	}
	if !b.Allow("1") || len(b.entries) != maxEntries {
		t.Fatal("evicted key did not return with full allowance")
	}
}

func TestRefill(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(1, 2, func() time.Time { return now })
	if !b.Allow("a") || !b.Allow("a") || b.Allow("a") {
		t.Fatal("unexpected initial allowance")
	}
	if !b.Allow("b") {
		t.Fatal("different key shares allowance")
	}
	now = now.Add(time.Second)
	if !b.Allow("a") || b.Allow("a") {
		t.Fatal("unexpected refill")
	}
}
