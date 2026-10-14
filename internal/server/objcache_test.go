package server

import (
	"testing"

	"github.com/delefrati/xylem/internal/hash"
)

func TestObjCache(t *testing.T) {
	t.Parallel()
	c := newObjCache(100)
	id := func(b byte) hash.ID { return hash.ID{b} }
	c.add(id(1), make([]byte, 20))
	c.add(id(2), make([]byte, 20))
	c.add(id(3), make([]byte, 20))
	if _, ok := c.get(id(1)); !ok { // 1 becomes most recent
		t.Fatal("1 missing")
	}
	c.add(id(4), make([]byte, 25))
	c.add(id(5), make([]byte, 25)) // 115 bytes: evict the LRU (2)
	if _, ok := c.get(id(2)); ok {
		t.Error("least recently used entry not evicted")
	}
	for _, b := range []byte{1, 3, 4, 5} {
		if !c.has(id(b)) {
			t.Errorf("%d evicted", b)
		}
	}
	if c.bytes > c.maxBytes {
		t.Errorf("cache holds %d bytes, budget %d", c.bytes, c.maxBytes)
	}
	c.add(id(6), make([]byte, 30)) // over a quarter of the budget: not cached
	if c.has(id(6)) {
		t.Error("oversized frame cached")
	}
	c.add(id(1), make([]byte, 20)) // re-adding is a no-op
	if c.bytes != 90 {
		t.Errorf("bytes = %d after re-add, want 90", c.bytes)
	}
	off := newObjCache(0)
	off.add(id(1), []byte("x"))
	if _, ok := off.get(id(1)); ok {
		t.Error("disabled cache stored something")
	}
}
