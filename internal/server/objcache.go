package server

import (
	"container/list"
	"sync"

	"github.com/delefrati/xylem/internal/hash"
)

// objCache is an LRU of stored object frames bounded by total bytes. Objects
// are immutable, so entries never need invalidation.
type objCache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	order    *list.List // front = most recently used
	items    map[hash.ID]*list.Element
}

type cacheEntry struct {
	id   hash.ID
	data []byte
}

func newObjCache(maxBytes int64) *objCache {
	return &objCache{maxBytes: maxBytes, order: list.New(), items: map[hash.ID]*list.Element{}}
}

// get returns the cached frame. Callers must not modify it.
func (c *objCache) get(id hash.ID) ([]byte, bool) {
	if c.maxBytes <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*cacheEntry).data, true
}

func (c *objCache) has(id hash.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[id]
	return ok
}

// add caches a frame, evicting least recently used ones to stay in budget.
// Frames larger than a quarter of the budget are not cached.
func (c *objCache) add(id hash.ID, data []byte) {
	size := int64(len(data))
	if c.maxBytes <= 0 || size > c.maxBytes/4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[id]; ok {
		c.order.MoveToFront(e)
		return
	}
	c.items[id] = c.order.PushFront(&cacheEntry{id: id, data: data})
	c.bytes += size
	for c.bytes > c.maxBytes {
		oldest := c.order.Back()
		ent := oldest.Value.(*cacheEntry)
		c.order.Remove(oldest)
		delete(c.items, ent.id)
		c.bytes -= int64(len(ent.data))
	}
}
