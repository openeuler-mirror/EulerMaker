package buildinfo

import (
	"container/list"
	"sync"
)

// specFileCache is a global LRU of raw spec contents keyed by commit ID and filename. Entries survive BuildInfo
// completion and expire only by capacity eviction; reads and writes refresh recency.
type specFileCache struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List // front = most recently used
	items map[string]*list.Element
}

type specFileEntry struct {
	key     string
	content string
}

func newSpecFileCache(capacity int) *specFileCache {
	if capacity <= 0 {
		capacity = 1
	}
	return &specFileCache{
		cap:   capacity,
		ll:    list.New(),
		items: map[string]*list.Element{},
	}
}

func specFileKey(commitID, fileName string) string {
	return commitID + "\x00" + fileName
}

// Get returns the cached raw content and refreshes hotness.
func (c *specFileCache) Get(commitID, fileName string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[specFileKey(commitID, fileName)]
	if !ok {
		return "", false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*specFileEntry).content, true
}

// Add inserts or refreshes an entry, evicting the least recently used entries beyond capacity.
func (c *specFileCache) Add(commitID, fileName, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := specFileKey(commitID, fileName)
	if el, ok := c.items[key]; ok {
		el.Value.(*specFileEntry).content = content
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&specFileEntry{key: key, content: content})
	c.items[key] = el
	for c.ll.Len() > c.cap {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.ll.Remove(back)
		delete(c.items, back.Value.(*specFileEntry).key)
	}
}

// Len reports the entry count, for tests and metrics.
func (c *specFileCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
