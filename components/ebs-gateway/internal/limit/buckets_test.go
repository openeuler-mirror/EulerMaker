package limit

import (
	"testing"
	"time"
)

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
