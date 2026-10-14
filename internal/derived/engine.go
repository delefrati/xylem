package derived

import (
	"context"
	"fmt"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
)

// InlineMax is the largest value stored inline in the derived table; larger
// values become "derived" objects referenced by value_ref (04 §Framework).
const InlineMax = 4 << 10

// Kind names a lazily computed derived kind. Bump Version on any format or
// semantic change; old values are then ignored.
type Kind struct {
	Name    string
	Version int16
}

// Engine stores and caches lazily computed derived values. Values are
// immutable, so the cache never needs invalidation, and concurrent
// computations of the same value are collapsed (and would agree anyway).
type Engine struct {
	meta    meta.Store
	repo    meta.RepoID
	objs    store.ObjectStore
	cache   *lru.Cache[string, []byte]
	flights singleflight.Group
}

// NewEngine returns an engine caching up to cacheEntries decoded values.
func NewEngine(m meta.Store, repo meta.RepoID, objs store.ObjectStore, cacheEntries int) *Engine {
	cache, _ := lru.New[string, []byte](max(cacheEntries, 1))
	return &Engine{meta: m, repo: repo, objs: objs, cache: cache}
}

func cacheKey(k Kind, cs hash.ID, key []byte) string {
	return fmt.Sprintf("%s/%d/%x/%s", k.Name, k.Version, cs[:], key)
}

// Load returns a stored value, from the cache, the table or the object store.
func (e *Engine) Load(ctx context.Context, k Kind, cs hash.ID, key []byte) ([]byte, bool, error) {
	ck := cacheKey(k, cs, key)
	if v, ok := e.cache.Get(ck); ok {
		return v, true, nil
	}
	value, ref, found, err := e.meta.GetDerived(ctx, e.repo, meta.DerivedKey{Kind: k.Name, Version: k.Version, Changeset: cs, Key: key})
	if err != nil || !found {
		return nil, false, err
	}
	if !ref.IsZero() {
		stored, err := e.objs.Get(ctx, ref)
		if err != nil {
			return nil, false, fmt.Errorf("derived value %s: %w", ref, err)
		}
		kind, body, err := object.DecodeVerify(ref, stored)
		if err != nil {
			return nil, false, err
		}
		if kind != object.KindDerived {
			return nil, false, fmt.Errorf("derived value %s is a %v", ref, kind)
		}
		value = body
	}
	e.cache.Add(ck, value)
	return value, true, nil
}

// Store saves a value: inline if small, else as a derived object.
func (e *Engine) Store(ctx context.Context, k Kind, cs hash.ID, key, value []byte) error {
	var ref hash.ID
	inline := value
	if len(value) > InlineMax {
		id, stored := object.Encode(object.KindDerived, value)
		if err := e.objs.Put(ctx, id, stored); err != nil {
			return err
		}
		ref, inline = id, nil
	}
	if err := e.meta.PutDerived(ctx, e.repo, meta.DerivedKey{Kind: k.Name, Version: k.Version, Changeset: cs, Key: key}, inline, ref); err != nil {
		return err
	}
	e.cache.Add(cacheKey(k, cs, key), value)
	return nil
}

// Get returns the value, computing and storing it if needed. Concurrent
// calls for the same value share one computation.
func (e *Engine) Get(ctx context.Context, k Kind, cs hash.ID, key []byte, compute func(context.Context) ([]byte, error)) ([]byte, error) {
	if v, ok, err := e.Load(ctx, k, cs, key); err != nil || ok {
		return v, err
	}
	v, err, _ := e.flights.Do(cacheKey(k, cs, key), func() (any, error) {
		if v, ok, err := e.Load(ctx, k, cs, key); err != nil || ok {
			return v, err
		}
		v, err := compute(ctx)
		if err != nil {
			return nil, err
		}
		return v, e.Store(ctx, k, cs, key, v)
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}
