package buildinfo

import (
	"fmt"
	"testing"
)

func TestSpecFileCacheTwoLevelKey(t *testing.T) {
	c := newSpecFileCache(10)
	c.Add("commit-a", "foo.spec", "foo-a")
	c.Add("commit-b", "foo.spec", "foo-b")
	c.Add("commit-a", "bar.spec", "bar-a")

	if got, ok := c.Get("commit-a", "foo.spec"); !ok || got != "foo-a" {
		t.Fatalf("commit-a/foo.spec = %q,%v", got, ok)
	}
	// Same file name under another commit is an independent entry.
	if got, ok := c.Get("commit-b", "foo.spec"); !ok || got != "foo-b" {
		t.Fatalf("commit-b/foo.spec = %q,%v", got, ok)
	}
	if got, ok := c.Get("commit-a", "bar.spec"); !ok || got != "bar-a" {
		t.Fatalf("commit-a/bar.spec = %q,%v", got, ok)
	}
	if _, ok := c.Get("commit-b", "bar.spec"); ok {
		t.Fatal("commit-b/bar.spec must miss")
	}
}

func TestSpecFileCacheOverwrite(t *testing.T) {
	c := newSpecFileCache(10)
	c.Add("c", "a.spec", "v1")
	c.Add("c", "a.spec", "v2")
	if got, _ := c.Get("c", "a.spec"); got != "v2" {
		t.Fatalf("overwrite = %q, want v2", got)
	}
	if c.Len() != 1 {
		t.Fatalf("overwrite must not grow, Len = %d", c.Len())
	}
}

func TestSpecFileCacheLRUEviction(t *testing.T) {
	c := newSpecFileCache(3)
	c.Add("c", "a.spec", "a")
	c.Add("c", "b.spec", "b")
	c.Add("c", "c.spec", "c")
	// Refresh a.spec so b.spec becomes the coldest.
	if _, ok := c.Get("c", "a.spec"); !ok {
		t.Fatal("a.spec must hit")
	}
	c.Add("c", "d.spec", "d")

	if _, ok := c.Get("c", "b.spec"); ok {
		t.Fatal("b.spec (coldest) must be evicted")
	}
	for _, name := range []string{"a.spec", "c.spec", "d.spec"} {
		if _, ok := c.Get("c", name); !ok {
			t.Fatalf("%s must survive eviction", name)
		}
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want capacity 3", c.Len())
	}
}

func TestSpecFileCacheWriteRefreshesHotness(t *testing.T) {
	c := newSpecFileCache(2)
	c.Add("c", "a.spec", "a")
	c.Add("c", "b.spec", "b")
	// Rewrite a.spec: b.spec is now coldest.
	c.Add("c", "a.spec", "a2")
	c.Add("c", "c.spec", "c")
	if _, ok := c.Get("c", "b.spec"); ok {
		t.Fatal("b.spec must be evicted after a.spec rewrite refreshed it")
	}
	if got, ok := c.Get("c", "a.spec"); !ok || got != "a2" {
		t.Fatalf("a.spec = %q,%v", got, ok)
	}
}

func TestSpecFileCacheCapacityOne(t *testing.T) {
	c := newSpecFileCache(1)
	for i := 0; i < 5; i++ {
		c.Add("c", fmt.Sprintf("%d.spec", i), "x")
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	if _, ok := c.Get("c", "4.spec"); !ok {
		t.Fatal("only the newest entry survives at capacity 1")
	}
}
