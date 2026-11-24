package limit

import (
	"strconv"
	"testing"
	"time"
)

func TestIdleBucketsExpireWithoutResettingActiveAllowance(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(1, 1, func() time.Time { return now })
	b.Allow("idle")
	now = now.Add(idleTTL - time.Second)
	b.Allow("active")
	now = now.Add(sweepInterval)
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

	// Even idle buckets must retain their debt when refill takes longer than the TTL.
	slow := New(1.0/3600, 1, func() time.Time { return now })
	slow.Allow("slow")
	now = now.Add(idleTTL)
	if slow.Allow("slow") {
		t.Fatal("expiration bypassed refill rate")
	}
}

func TestBucketCapacityRejectsNewKeysUntilCleanup(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(1, 2, func() time.Time { return now })
	for i := 0; i < maxEntries; i++ {
		if !b.Allow(strconv.Itoa(i)) {
			t.Fatal("capacity reached too early")
		}
	}
	if b.Allow("overflow") || len(b.entries) != maxEntries {
		t.Fatal("capacity not enforced")
	}
	if !b.Allow("0") || b.Allow("0") {
		t.Fatal("existing key allowance changed at capacity")
	}
	now = now.Add(idleTTL)
	if !b.Allow("overflow") || len(b.entries) != 1 {
		t.Fatal("expired capacity not reclaimed")
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
