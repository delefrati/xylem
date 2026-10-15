package cache

import (
	"container/list"
	"sync"

	"github.com/delefrati/xylem/internal/hash"
)

// Memory is an LRU of stored object frames bounded by total bytes. Objects
// are immutable, so entries never need invalidation. It is safe for
// concurrent use.
type Memory struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	order    *list.List // front = most recently used
	items    map[hash.ID]*list.Element
}

type entry struct {
	id   hash.ID
	data []byte
}

// NewMemory returns an LRU holding up to maxBytes (0 disables it).
func NewMemory(maxBytes int64) *Memory {
	return &Memory{maxBytes: maxBytes, order: list.New(), items: map[hash.ID]*list.Element{}}
}

// Get returns a cached frame; callers must not modify it.
func (m *Memory) Get(id hash.ID) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[id]
	if !ok {
		return nil, false
	}
	m.order.MoveToFront(e)
	return e.Value.(*entry).data, true
}

// Has reports whether id is cached.
func (m *Memory) Has(id hash.ID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.items[id]
	return ok
}

// Add caches a frame, evicting the least recently used ones to stay within
// budget. Frames larger than a quarter of the budget are not cached.
func (m *Memory) Add(id hash.ID, data []byte) {
	size := int64(len(data))
	if m.maxBytes <= 0 || size > m.maxBytes/4 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.items[id]; ok {
		m.order.MoveToFront(e)
		return
	}
	m.items[id] = m.order.PushFront(&entry{id: id, data: data})
	m.bytes += size
	for m.bytes > m.maxBytes {
		oldest := m.order.Back()
		old := oldest.Value.(*entry)
		m.order.Remove(oldest)
		delete(m.items, old.id)
		m.bytes -= int64(len(old.data))
	}
}

// Stats returns the number of cached frames and their total size.
func (m *Memory) Stats() (objects int, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items), m.bytes
}
