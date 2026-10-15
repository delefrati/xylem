package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/tracing"
)

// Defaults (09 §Caching tiers).
const (
	DefaultBatchWindow = 2 * time.Millisecond
	DefaultMaxBatch    = 1024
	DefaultMaxInflight = 256
	DefaultMemoryBytes = 256 << 20
	upstreamTimeout    = 2 * time.Minute
	packTarget         = 1 << 20 // PutObjects messages are packed up to ~1 MiB
)

var (
	// ErrOverloaded reports that too many upstream requests are queued; the
	// tier fails fast instead of piling up (ResourceExhausted on the wire).
	ErrOverloaded = errors.New("cache tier overloaded: too many upstream requests queued")
	// ErrCorrupt reports an object from upstream that does not hash to its id.
	ErrCorrupt = errors.New("upstream sent a corrupt object")
	// ErrInvalid reports an uploaded object that does not verify.
	ErrInvalid = errors.New("invalid object")
)

// Config configures a Tier.
type Config struct {
	Upstream    xylemv1.ObjectServiceClient
	MemoryBytes int64         // memory LRU size (default DefaultMemoryBytes; negative disables)
	Disk        *Disk         // optional disk LRU
	BatchWindow time.Duration // how long a miss waits for others to batch with (default 2 ms)
	MaxBatch    int           // ids per upstream GetObjects (default 1024)
	MaxInflight int           // concurrent upstream calls (default 256)
	MaxQueue    int           // upstream calls waiting for a slot before failing fast (default 4 × MaxInflight)
	Logger      *slog.Logger
	// Observe, if set, is told about each object lookup (hit) and each
	// upstream call (metrics, M8.5).
	Observe Observer
}

// Observer receives the tier's events.
type Observer interface {
	Hit(n int)
	Miss(n int)
	UpstreamInflight(delta int)
}

// Tier is a caching ObjectService client: reads come from memory, then disk,
// then upstream. Concurrent misses for the same object share one upstream
// fetch, and misses arriving within BatchWindow are fetched in one call. It
// is safe for concurrent use.
type Tier struct {
	cfg    Config
	mem    *Memory
	slots  chan struct{}
	queued atomic.Int64

	mu      sync.Mutex
	flights map[flightKey]*flight
	pending map[string]*batch // per repo, not yet sent
}

type flightKey struct {
	repo string
	id   hash.ID
}

// flight is one object being fetched; done closes when data or err is set.
type flight struct {
	done chan struct{}
	data []byte // nil: upstream does not have it
	err  error
}

type batch struct {
	repo    string
	ids     []hash.ID
	flights map[hash.ID]*flight
	sent    bool
	// trace is the span of the call that opened the batch: the upstream
	// fetch joins that call's trace (M9.5).
	trace trace.SpanContext
}

// NewTier returns a Tier.
func NewTier(cfg Config) *Tier {
	if cfg.MemoryBytes == 0 {
		cfg.MemoryBytes = DefaultMemoryBytes
	}
	if cfg.BatchWindow <= 0 {
		cfg.BatchWindow = DefaultBatchWindow
	}
	if cfg.MaxBatch <= 0 || cfg.MaxBatch > DefaultMaxBatch {
		cfg.MaxBatch = DefaultMaxBatch
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = DefaultMaxInflight
	}
	if cfg.MaxQueue <= 0 {
		cfg.MaxQueue = 4 * cfg.MaxInflight
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Tier{
		cfg: cfg, mem: NewMemory(max(cfg.MemoryBytes, 0)), slots: make(chan struct{}, cfg.MaxInflight),
		flights: map[flightKey]*flight{}, pending: map[string]*batch{},
	}
}

// Memory returns the tier's memory LRU.
func (t *Tier) Memory() *Memory { return t.mem }

// local returns a cached frame, promoting disk hits to memory.
func (t *Tier) local(id hash.ID) ([]byte, bool) {
	if data, ok := t.mem.Get(id); ok {
		return data, true
	}
	if t.cfg.Disk != nil {
		if data, ok := t.cfg.Disk.Get(id); ok {
			t.mem.Add(id, data)
			return data, true
		}
	}
	return nil, false
}

func (t *Tier) keep(id hash.ID, data []byte) {
	t.mem.Add(id, data)
	if t.cfg.Disk != nil {
		if err := t.cfg.Disk.Put(id, data); err != nil {
			t.cfg.Logger.Warn("disk cache write failed", "id", id.String(), "err", err)
		}
	}
}

// Get returns the stored frames of ids that exist (verified when they came
// from upstream); ids upstream does not have are absent from the map.
func (t *Tier) Get(ctx context.Context, repo string, ids []hash.ID) (map[hash.ID][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[hash.ID][]byte, len(ids))
	waits := map[hash.ID]*flight{}
	hits := 0
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		if _, waiting := waits[id]; waiting {
			continue
		}
		if data, ok := t.local(id); ok {
			out[id] = data
			hits++
			continue
		}
		waits[id] = t.join(ctx, repo, id)
	}
	if o := t.cfg.Observe; o != nil {
		o.Hit(hits)
		o.Miss(len(waits))
	}
	for id, f := range waits {
		select {
		case <-f.done:
			if f.err != nil {
				return nil, f.err
			}
			if f.data != nil {
				out[id] = f.data
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, nil
}

// join returns the flight fetching (repo, id), queuing a new one if none.
func (t *Tier) join(ctx context.Context, repo string, id hash.ID) *flight {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := flightKey{repo, id}
	if f, ok := t.flights[key]; ok {
		return f
	}
	f := &flight{done: make(chan struct{})}
	t.flights[key] = f
	b := t.pending[repo]
	if b == nil {
		b = &batch{repo: repo, flights: map[hash.ID]*flight{}, trace: trace.SpanContextFromContext(ctx)}
		t.pending[repo] = b
		time.AfterFunc(t.cfg.BatchWindow, func() { t.send(b) })
	}
	b.ids = append(b.ids, id)
	b.flights[id] = f
	if len(b.ids) >= t.cfg.MaxBatch {
		delete(t.pending, repo)
		b.sent = true
		go t.fetch(b) //nolint:gosec // G118: the batch serves every caller that joins it, so it must not die with ctx
	}
	return f
}

// send dispatches a batch when its window closes (unless it filled up first).
func (t *Tier) send(b *batch) {
	t.mu.Lock()
	if b.sent {
		t.mu.Unlock()
		return
	}
	b.sent = true
	if t.pending[b.repo] == b {
		delete(t.pending, b.repo)
	}
	t.mu.Unlock()
	t.fetch(b)
}

func (t *Tier) finish(repo string, id hash.ID, f *flight, data []byte, err error) {
	f.data, f.err = data, err
	t.mu.Lock()
	delete(t.flights, flightKey{repo, id})
	t.mu.Unlock()
	close(f.done)
}

// acquire takes an upstream slot, failing fast when too many wait.
func (t *Tier) acquire(ctx context.Context) (release func(), err error) {
	if t.queued.Add(1) > int64(t.cfg.MaxQueue) {
		t.queued.Add(-1)
		return nil, ErrOverloaded
	}
	defer t.queued.Add(-1)
	select {
	case t.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if o := t.cfg.Observe; o != nil {
		o.UpstreamInflight(1)
	}
	return func() {
		<-t.slots
		if o := t.cfg.Observe; o != nil {
			o.UpstreamInflight(-1)
		}
	}, nil
}

// fetch gets a batch from upstream, verifies and caches each object, and
// completes the flights. Ids upstream does not mention count as missing.
func (t *Tier) fetch(b *batch) {
	// The batch serves every caller that joined it; it runs on none of their
	// contexts, but continues the trace of the first.
	ctx, span := tracing.Tracer().Start(trace.ContextWithSpanContext(context.Background(), b.trace), "cache.fetch",
		trace.WithAttributes(attribute.Int("xylem.ids", len(b.ids)), attribute.String("xylem.repo", b.repo)))
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	fail := func(err error) {
		for id, f := range b.flights {
			t.finish(b.repo, id, f, nil, err)
		}
	}
	release, err := t.acquire(ctx)
	if err != nil {
		fail(err)
		return
	}
	defer release()
	raw := make([][]byte, len(b.ids))
	for i := range b.ids {
		raw[i] = b.ids[i][:]
	}
	stream, err := t.cfg.Upstream.GetObjects(ctx, &xylemv1.GetObjectsRequest{Repo: b.repo, Ids: raw})
	if err != nil {
		fail(err)
		return
	}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fail(err)
			return
		}
		for _, o := range msg.GetObjects() {
			var id hash.ID
			copy(id[:], o.GetId())
			f, ok := b.flights[id]
			if !ok {
				continue // not asked for (or already done)
			}
			delete(b.flights, id)
			if err := object.Verify(id, o.GetData()); err != nil {
				t.finish(b.repo, id, f, nil, fmt.Errorf("%w %s: %w", ErrCorrupt, id.Short(12), err))
				continue
			}
			t.keep(id, o.GetData())
			t.finish(b.repo, id, f, o.GetData(), nil)
		}
	}
	for id, f := range b.flights { // listed in missing, or not mentioned
		t.finish(b.repo, id, f, nil, nil)
	}
}

// Has reports, in order, whether each id exists here or upstream.
func (t *Tier) Has(ctx context.Context, repo string, ids []hash.ID) ([]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	present := make([]bool, len(ids))
	var ask [][]byte
	var at []int
	for i, id := range ids {
		if t.mem.Has(id) || t.cfg.Disk != nil && t.cfg.Disk.Has(id) {
			present[i] = true
			continue
		}
		ask = append(ask, ids[i][:])
		at = append(at, i)
	}
	for start := 0; start < len(ask); start += DefaultMaxBatch {
		end := min(start+DefaultMaxBatch, len(ask))
		release, err := t.acquire(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := t.cfg.Upstream.HasObjects(ctx, &xylemv1.HasObjectsRequest{Repo: repo, Ids: ask[start:end]})
		release()
		if err != nil {
			return nil, err
		}
		if len(resp.GetPresent()) != end-start {
			return nil, fmt.Errorf("upstream HasObjects answered %d of %d ids", len(resp.GetPresent()), end-start)
		}
		for j, ok := range resp.GetPresent() {
			present[at[start+j]] = ok
		}
	}
	return present, nil
}

// Put forwards objects upstream (write-through) and caches them locally.
// Objects are verified first; an invalid one fails the call with ErrInvalid
// before anything is sent.
func (t *Tier) Put(ctx context.Context, repo string, objs []*xylemv1.StoredObject) (*xylemv1.PutObjectsResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := make([]hash.ID, len(objs))
	for i, o := range objs {
		if len(o.GetId()) != hash.Size {
			return nil, fmt.Errorf("%w: id %d has %d bytes", ErrInvalid, i, len(o.GetId()))
		}
		copy(ids[i][:], o.GetId())
		if err := object.Verify(ids[i], o.GetData()); err != nil {
			return nil, fmt.Errorf("%w %s: %w", ErrInvalid, ids[i].Short(12), err)
		}
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	stream, err := t.cfg.Upstream.PutObjects(ctx)
	if err != nil {
		return nil, err
	}
	msg := &xylemv1.PutObjectsRequest{Repo: repo}
	size := 0
	for _, o := range objs {
		if size > 0 && (size+len(o.GetData()) > packTarget || len(msg.Objects) == DefaultMaxBatch) {
			if err := stream.Send(msg); err != nil {
				break // CloseAndRecv reports why
			}
			msg, size = &xylemv1.PutObjectsRequest{Repo: repo}, 0
		}
		msg.Objects = append(msg.Objects, o)
		size += len(o.GetData())
	}
	if len(msg.Objects) > 0 || len(objs) == 0 {
		_ = stream.Send(msg) // an error surfaces in CloseAndRecv
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	for i, o := range objs {
		t.keep(ids[i], o.GetData())
	}
	return resp, nil
}

// Store adapts the tier to store.ObjectStore for one repository (reads go
// through the cache, puts are written through).
func (t *Tier) Store(repo string) store.ObjectStore { return tierStore{t, repo} }

type tierStore struct {
	t    *Tier
	repo string
}

func (s tierStore) Get(ctx context.Context, id hash.ID) ([]byte, error) {
	got, err := s.t.Get(ctx, s.repo, []hash.ID{id})
	if err != nil {
		return nil, err
	}
	data, ok := got[id]
	if !ok {
		return nil, fmt.Errorf("%s: %w", id, store.ErrNotFound)
	}
	return data, nil
}

func (s tierStore) Put(ctx context.Context, id hash.ID, stored []byte) error {
	_, err := s.t.Put(ctx, s.repo, []*xylemv1.StoredObject{{Id: id[:], Data: stored}})
	return err
}

func (s tierStore) Has(ctx context.Context, ids []hash.ID) ([]bool, error) {
	return s.t.Has(ctx, s.repo, ids)
}
