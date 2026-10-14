//go:build integration

package derived_test

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

func TestEngine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := meta.NewPG(testutil.NewDB(t))
	repo, err := m.CreateRepo(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	objs := store.NewMem()
	kind := derived.Kind{Name: "test", Version: 1}
	e := derived.NewEngine(m, repo, objs, 10)

	small, big, empty := []byte("small value"), bytes.Repeat([]byte("x"), derived.InlineMax+1), []byte{}
	for i, v := range [][]byte{small, big, empty} {
		cs := hash.ID{byte(i + 1)}
		if err := e.Store(ctx, kind, cs, []byte("key"), v); err != nil {
			t.Fatal(err)
		}
		// A fresh engine (cold cache) reads it back from the table or object store.
		got, found, err := derived.NewEngine(m, repo, objs, 10).Load(ctx, kind, cs, []byte("key"))
		if err != nil || !found || !bytes.Equal(got, v) {
			t.Errorf("value %d: Load = %d bytes, %v, %v", i, len(got), found, err)
		}
	}
	if _, _, found, _ := m.GetDerived(ctx, repo, meta.DerivedKey{Kind: "test", Version: 1, Changeset: hash.ID{2}, Key: []byte("key")}); !found {
		t.Error("big value has no table row")
	}
	if _, found, _ := e.Load(ctx, derived.Kind{Name: "test", Version: 2}, hash.ID{1}, []byte("key")); found {
		t.Error("a different version must not see the value")
	}

	// Concurrent Gets compute once.
	var computed atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			v, err := e.Get(ctx, kind, hash.ID{9}, []byte("k"), func(context.Context) ([]byte, error) {
				computed.Add(1)
				return []byte("computed"), nil
			})
			if err != nil || string(v) != "computed" {
				t.Errorf("Get = %q, %v", v, err)
			}
		})
	}
	wg.Wait()
	if n := computed.Load(); n != 1 {
		t.Errorf("computed %d times, want 1", n)
	}
}
