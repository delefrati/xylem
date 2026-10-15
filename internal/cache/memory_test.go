package cache

import (
	"testing"

	"github.com/delefrati/xylem/internal/hash"
)

func TestMemory(t *testing.T) {
	t.Parallel()
	c := NewMemory(100)
	id := func(b byte) hash.ID { return hash.ID{b} }
	c.Add(id(1), make([]byte, 20))
	c.Add(id(2), make([]byte, 20))
	c.Add(id(3), make([]byte, 20))
	if _, ok := c.Get(id(1)); !ok { // 1 becomes most recent
		t.Fatal("1 missing")
	}
	c.Add(id(4), make([]byte, 25))
	c.Add(id(5), make([]byte, 25)) // 115 bytes: evict the LRU (2)
	if _, ok := c.Get(id(2)); ok {
		t.Error("least recently used entry not evicted")
	}
	for _, b := range []byte{1, 3, 4, 5} {
		if !c.Has(id(b)) {
			t.Errorf("%d evicted", b)
		}
	}
	if c.bytes > c.maxBytes {
		t.Errorf("cache holds %d bytes, budget %d", c.bytes, c.maxBytes)
	}
	c.Add(id(6), make([]byte, 30)) // over a quarter of the budget: not cached
	if c.Has(id(6)) {
		t.Error("oversized frame cached")
	}
	c.Add(id(1), make([]byte, 20)) // re-adding is a no-op
	if c.bytes != 90 {
		t.Errorf("bytes = %d after re-add, want 90", c.bytes)
	}
	off := NewMemory(0)
	off.Add(id(1), []byte("x"))
	if _, ok := off.Get(id(1)); ok {
		t.Error("disabled cache stored something")
	}
}
