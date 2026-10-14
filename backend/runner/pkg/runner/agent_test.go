package runner

import "testing"

func TestMemoryQuantityFromMeminfo(t *testing.T) {
	meminfo := "MemTotal:       65857640 kB\nMemFree:         1024 kB\n"
	got := memoryQuantityFromMeminfo(meminfo)
	if got != "64314Mi" {
		t.Fatalf("expected 64314Mi, got %s", got)
	}
}

func TestBytesToGiQuantity(t *testing.T) {
	got := bytesToGiQuantity(10 * 1024 * 1024 * 1024)
	if got != "10Gi" {
		t.Fatalf("expected 10Gi, got %s", got)
	}
}
