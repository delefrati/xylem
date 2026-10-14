package client

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

func TestObjectCache(t *testing.T) {
	t.Parallel()
	c := NewObjectCache(t.TempDir(), 0)
	id, stored := object.Encode(object.KindBlob, []byte("hello"))
	if c.Has(id) {
		t.Fatal("empty cache has an object")
	}
	if _, err := c.Get(id); !errors.Is(err, ErrCacheMiss) {
		t.Errorf("Get miss: %v", err)
	}
	if err := c.Put(id, stored); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(id, stored); err != nil {
		t.Errorf("repeat Put: %v", err)
	}
	other, _ := object.Encode(object.KindBlob, []byte("other"))
	if err := c.Put(other, stored); err == nil {
		t.Error("Put with the wrong id accepted")
	}
	kind, body, err := c.GetVerified(id)
	if err != nil || kind != object.KindBlob || string(body) != "hello" {
		t.Errorf("GetVerified = %v %q %v", kind, body, err)
	}
	// A corrupted file is detected and removed.
	if err := os.WriteFile(c.path(id), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetVerified(id); !errors.Is(err, ErrCacheMiss) || c.Has(id) {
		t.Errorf("corrupt entry: %v, still cached %v", err, c.Has(id))
	}
}

func TestObjectCacheLRU(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := NewObjectCache(dir, 0)
	var ids []hash.ID
	var size int64
	for i := range 6 {
		id, stored := object.Encode(object.KindBlob, []byte{byte('a' + i)})
		if err := c.Put(id, stored); err != nil {
			t.Fatal(err)
		}
		// Distinct, increasing access times.
		at := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(c.path(id), at, at); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		size += int64(len(stored))
	}
	if _, err := c.Get(ids[0]); err != nil { // the oldest becomes the most recent
		t.Fatal(err)
	}
	s, err := c.Stats()
	if err != nil || s.Objects != 6 || s.Bytes != size {
		t.Fatalf("Stats = %+v, %v", s, err)
	}
	per := size / 6
	removed, freed, err := c.GC(size - 2*per)
	if err != nil || removed != 2 || freed != 2*per {
		t.Fatalf("GC = %d, %d, %v", removed, freed, err)
	}
	if !c.Has(ids[0]) || c.Has(ids[1]) || c.Has(ids[2]) || !c.Has(ids[3]) {
		t.Error("GC did not evict the least recently used objects (ids 1 and 2)")
	}
	limited := NewObjectCache(dir, 2*per)
	if err := limited.EnsureLimit(); err != nil {
		t.Fatal(err)
	}
	if s, _ := limited.Stats(); s.Bytes > 2*per {
		t.Errorf("EnsureLimit left %d bytes, limit %d", s.Bytes, 2*per)
	}
}
