// Package storetest is a contract test suite for store.ObjectStore
// implementations. Every implementation, including the cache tiers, runs it.
package storetest

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
)

// Factory returns a store for one subtest. It may return a fresh store or a
// shared one: the suite uses unique random objects, so subtests never collide.
type Factory func(t *testing.T) store.ObjectStore

// LargeBodySize is the body size of the large-object test. It is above the
// 16 MiB S3 multipart threshold and made of random bytes, so the stored frame
// is about as large.
const LargeBodySize = 24 << 20

// Run runs the contract suite against stores from newStore.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.ObjectStore)
	}{
		{"PutGet", testPutGet},
		{"NotFound", testNotFound},
		{"Has", testHas},
		{"HasEmpty", testHasEmpty},
		{"IdempotentPut", testIdempotentPut},
		{"ConcurrentPutsSameID", testConcurrentPutsSameID},
		{"ConcurrentPutsDistinctIDs", testConcurrentPutsDistinctIDs},
		{"LargeObject", testLargeObject},
		{"CanceledContext", testCanceledContext},
		{"List", testList},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.fn(t, newStore(t))
		})
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	return c
}

// newObject returns a unique object for this test run: its body is random, so
// subtests can share a store (e.g. one S3 bucket) without colliding.
func newObject(t *testing.T, size int) (hash.ID, []byte) {
	t.Helper()
	var seed [32]byte
	if _, err := cryptorand.Read(seed[:]); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, size)
	_, _ = rand.NewChaCha8(seed).Read(body)
	return object.Encode(object.KindBlob, body)
}

func testPutGet(t *testing.T, s store.ObjectStore) {
	id, stored := newObject(t, 100)
	if err := s.Put(ctx(t), id, stored); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx(t), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, stored) {
		t.Fatalf("Get returned %d bytes, want the %d stored", len(got), len(stored))
	}
	if err := object.Verify(id, got); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Mutating the returned slice must not affect the store.
	got[0] ^= 0xff
	again, err := s.Get(ctx(t), id)
	if err != nil || !bytes.Equal(again, stored) {
		t.Fatalf("store changed after mutating a Get result: %v", err)
	}
}

func testNotFound(t *testing.T, s store.ObjectStore) {
	id, _ := newObject(t, 10) // never put
	if b, err := s.Get(ctx(t), id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(missing) = %d bytes, %v; want ErrNotFound", len(b), err)
	}
}

func testHas(t *testing.T, s store.ObjectStore) {
	var ids []hash.ID
	var want []bool
	for i := range 10 {
		id, stored := newObject(t, 10+i)
		if i%3 == 0 {
			if err := s.Put(ctx(t), id, stored); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		ids = append(ids, id)
		want = append(want, i%3 == 0)
	}
	// Duplicates are answered per position.
	ids = append(ids, ids[0], ids[1])
	want = append(want, want[0], want[1])
	got, err := s.Has(ctx(t), ids)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Has returned %d results for %d ids", len(got), len(ids))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Has[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func testHasEmpty(t *testing.T, s store.ObjectStore) {
	got, err := s.Has(ctx(t), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Has(nil) = %v, %v; want empty", got, err)
	}
}

func testIdempotentPut(t *testing.T, s store.ObjectStore) {
	id, stored := newObject(t, 1000)
	for i := range 3 {
		if err := s.Put(ctx(t), id, stored); err != nil {
			t.Fatalf("Put #%d: %v", i+1, err)
		}
	}
	got, err := s.Get(ctx(t), id)
	if err != nil || !bytes.Equal(got, stored) {
		t.Fatalf("Get after repeated Put: %v", err)
	}
}

func testConcurrentPutsSameID(t *testing.T, s store.ObjectStore) {
	id, stored := newObject(t, 64<<10)
	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { errs[i] = s.Put(ctx(t), id, stored) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("concurrent Put: %v", err)
	}
	got, err := s.Get(ctx(t), id)
	if err != nil || !bytes.Equal(got, stored) {
		t.Fatalf("Get after concurrent Put: %v", err)
	}
}

func testConcurrentPutsDistinctIDs(t *testing.T, s store.ObjectStore) {
	const n = 64
	ids := make([]hash.ID, n)
	objs := make([][]byte, n)
	for i := range n {
		ids[i], objs[i] = newObject(t, 100+i)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { errs[i] = s.Put(ctx(t), ids[i], objs[i]) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("concurrent Put: %v", err)
	}
	has, err := s.Has(ctx(t), ids)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	for i := range n {
		if !has[i] {
			t.Errorf("Has[%d] = false after Put", i)
		}
		if got, err := s.Get(ctx(t), ids[i]); err != nil || !bytes.Equal(got, objs[i]) {
			t.Errorf("Get[%d]: %v", i, err)
		}
	}
}

func testLargeObject(t *testing.T, s store.ObjectStore) {
	id, stored := newObject(t, LargeBodySize)
	if len(stored) < LargeBodySize {
		t.Fatalf("stored frame is %d bytes, want at least %d (incompressible body)", len(stored), LargeBodySize)
	}
	if err := s.Put(ctx(t), id, stored); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx(t), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := object.Verify(id, got); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func testCanceledContext(t *testing.T, s store.ObjectStore) {
	id, stored := newObject(t, 10)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Put(canceled, id, stored); err == nil {
		t.Error("Put with canceled context succeeded")
	}
	if _, err := s.Get(canceled, id); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get with canceled context = %v, want a context error", err)
	}
	if _, err := s.Has(canceled, []hash.ID{id}); err == nil {
		t.Error("Has with canceled context succeeded")
	}
}

// testList checks Lister, for stores that implement it: every object put is
// listed with its stored size and a recent modification time.
func testList(t *testing.T, s store.ObjectStore) {
	l, ok := s.(store.Lister)
	if !ok {
		t.Skip("not a store.Lister")
	}
	before := time.Now().Add(-time.Minute)
	want := map[hash.ID]int{}
	for range 3 {
		id, stored := newObject(t, 100)
		if err := s.Put(ctx(t), id, stored); err != nil {
			t.Fatal(err)
		}
		want[id] = len(stored)
	}
	err := l.List(ctx(t), func(info store.ObjectInfo) error {
		size, ok := want[info.ID]
		if !ok {
			return nil // another subtest's object (shared store)
		}
		if info.Size != int64(size) || info.ModTime.Before(before) || info.ModTime.After(time.Now().Add(time.Minute)) {
			t.Errorf("listed %s: size %d (want %d), mod time %v", info.ID.Short(8), info.Size, size, info.ModTime)
		}
		delete(want, info.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 0 {
		t.Errorf("%d objects not listed", len(want))
	}
	stop := errors.New("stop")
	if err := l.List(ctx(t), func(store.ObjectInfo) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("an error from fn is not returned: %v", err)
	}
}
