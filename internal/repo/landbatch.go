package repo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
)

// Land batcher (M9.3, 07 §Contention mitigation): with
// Options.LandBatchWindow > 0, Land hands its request to one goroutine per
// target ref, which collects requests for the window and rebases them one
// after another onto the tip in memory (trees and merged blobs go to the
// object store; changesets are held in a pendingSet). Then one transaction
// inserts every rebased changeset and moves the ref. Only that CAS can lose
// to another server, and a lost one writes no changesets.

const (
	// maxLandBatch caps the requests one batch rebases (they run one after
	// another, so this bounds a batch's latency).
	maxLandBatch = 100
	// landBatcherIdle is how long a ref's batch goroutine waits for work
	// before exiting.
	landBatcherIdle = 30 * time.Second
	// casTimeout bounds the batch's ref update (it runs on no caller's
	// context, since it lands everyone's work).
	casTimeout = 30 * time.Second
)

type landReq struct {
	ctx      context.Context
	stack    []hash.ID
	opts     LandOptions
	arrived  time.Time
	attempts int           // CAS attempts used
	done     chan landDone // buffered 1
}

type landDone struct {
	res *LandResult
	err error
}

// landQueue is one target ref's pending requests.
type landQueue struct {
	pending []*landReq
	wake    chan struct{} // buffered 1
}

type landBatcher struct {
	r      *Repo
	window time.Duration

	mu     sync.Mutex
	queues map[string]*landQueue
}

func newLandBatcher(r *Repo, window time.Duration) *landBatcher {
	return &landBatcher{r: r, window: window, queues: map[string]*landQueue{}}
}

// land queues a request and waits for its batch.
func (b *landBatcher) land(ctx context.Context, stack []hash.ID, opts LandOptions) (*LandResult, error) {
	req := &landReq{ctx: ctx, stack: stack, opts: opts, arrived: time.Now(), done: make(chan landDone, 1)}
	b.mu.Lock()
	q := b.queues[opts.Target]
	if q == nil {
		q = &landQueue{wake: make(chan struct{}, 1)}
		b.queues[opts.Target] = q
		go b.run(opts.Target, q)
	}
	q.pending = append(q.pending, req)
	b.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	select {
	case d := <-req.done:
		return d.res, d.err
	case <-ctx.Done():
		// The batch may still land the stack, as a cancelled unbatched land
		// may have if it was past its CAS.
		return nil, ctx.Err()
	}
}

// run serves one target ref until it has been idle for landBatcherIdle. A
// batch starts when the window since its oldest request has passed, when
// maxLandBatch requests wait, or once as many wait as the previous batch had
// if that was two or more (the same landers are back; waiting longer would
// only add latency).
func (b *landBatcher) run(target string, q *landQueue) {
	idle := time.NewTimer(landBatcherIdle)
	defer idle.Stop()
	last := 0 // size of the previous batch
	for {
		b.mu.Lock()
		if len(q.pending) == 0 {
			b.mu.Unlock()
			select {
			case <-q.wake:
			case <-idle.C:
				b.mu.Lock()
				if len(q.pending) == 0 {
					delete(b.queues, target)
					b.mu.Unlock()
					return
				}
				b.mu.Unlock()
			}
			continue
		}
		enough := maxLandBatch
		if last >= 2 {
			enough = min(last, maxLandBatch)
		}
		deadline := time.NewTimer(time.Until(q.pending[0].arrived.Add(b.window)))
		for len(q.pending) < enough {
			b.mu.Unlock()
			timedOut := false
			select {
			case <-deadline.C:
				timedOut = true
			case <-q.wake:
			}
			b.mu.Lock()
			if timedOut {
				break
			}
		}
		deadline.Stop()
		n := min(len(q.pending), maxLandBatch)
		last = n
		batch := slices.Clone(q.pending[:n])
		q.pending = slices.Delete(q.pending, 0, n)
		b.mu.Unlock()
		b.r.landBatch(target, batch)
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(landBatcherIdle)
	}
}

// outcome is a request's result within one batch attempt.
type outcome struct {
	req *landReq
	res *LandResult
}

// landBatch lands batch on target: each attempt rebases every request onto
// the tip as advanced by the requests before it, then moves the ref once.
// Errors are answered at once; conflicts and landings only after the CAS
// succeeds (or when nothing needs to move), so a conflict is always against
// what actually landed. On a lost CAS the whole batch is retried, minus the
// requests that ran out of attempts.
func (r *Repo) landBatch(target string, batch []*landReq) {
	if r.opts.OnLandBatch != nil {
		r.opts.OnLandBatch(len(batch))
	}
	answer := func(req *landReq, res *LandResult, err error) { req.done <- landDone{res, err} }
	// Every stack's changesets in one query (missing ones are reported by
	// each request's rebase).
	var ids []hash.ID
	for _, req := range batch {
		ids = append(ids, req.stack...)
	}
	stored, err := r.Changesets(context.Background(), ids)
	if err != nil {
		for _, req := range batch {
			answer(req, nil, err)
		}
		return
	}
	for attempt := 1; len(batch) > 0; attempt++ {
		ref, err := r.GetRef(context.Background(), target)
		if err != nil {
			for _, req := range batch {
				answer(req, nil, err)
			}
			return
		}
		cur := ref.ID
		now := r.opts.Now().Unix()
		pending := newPendingSet(r.source, stored)
		var outs []outcome
		var actors []string
		for _, req := range batch {
			if err := req.ctx.Err(); err != nil {
				answer(req, nil, err)
				continue
			}
			mark := len(pending.list)
			rb, err := r.RebaseStack(req.ctx, req.stack, cur, RebaseOptions{Committer: req.opts.Committer, Time: now, pending: pending})
			if err != nil {
				pending.truncate(mark)
				answer(req, nil, err)
				continue
			}
			res := &LandResult{Skipped: rb.Skipped}
			switch {
			case len(rb.Conflicts) > 0:
				pending.truncate(mark) // drafts of the stack's earlier entries
				res.Conflicts, res.Conflicting = rb.Conflicts, rb.Conflicting
			case len(rb.Rebased) == 0: // already on the target
				res.NewTip = cur
			default:
				if req.opts.BeforeCAS != nil {
					if err := req.opts.BeforeCAS(req.ctx, req.attempts+1); err != nil {
						pending.truncate(mark) // nothing of it is written
						answer(req, nil, err)
						continue
					}
				}
				res.Landed, res.NewTip = rb.Rebased, rb.Tip
				if !slices.Contains(actors, req.opts.Actor) {
					actors = append(actors, req.opts.Actor)
				}
				cur = rb.Tip
			}
			outs = append(outs, outcome{req, res})
		}
		if cur != ref.ID {
			ctx, cancel := context.WithTimeout(context.Background(), casTimeout)
			err = r.commitBatch(ctx, pending, target, ref.ID, cur, strings.Join(actors, ", "))
			cancel()
		}
		switch {
		case errors.Is(err, meta.ErrCASMismatch):
			// Another server moved the tip: retry everyone still waiting.
			batch = batch[:0]
			for _, o := range outs {
				o.req.attempts++
				if o.req.attempts >= o.req.opts.MaxAttempts {
					answer(o.req, nil, fmt.Errorf("%w: landing on %q: the tip kept moving (%d attempts)", ErrConflict, target, o.req.attempts))
					continue
				}
				batch = append(batch, o.req)
			}
			_ = sleep(context.Background(), landBackoff(attempt))
		case err != nil:
			for _, o := range outs {
				answer(o.req, nil, err)
			}
			return
		default:
			for _, o := range outs {
				o.res.Attempts = o.req.attempts + 1
				answer(o.req, o.res, nil)
			}
			return
		}
	}
}

// pendingSet holds the changesets a land batch has built but not inserted:
// a derived.Source that answers for them and falls back to src.
type pendingSet struct {
	src    derived.Source
	list   []*prepared // in creation order (parents first)
	byID   map[hash.ID]*prepared
	stored map[hash.ID]*Changeset // stored changesets prefetched for the batch
}

func newPendingSet(src derived.Source, stored map[hash.ID]*Changeset) *pendingSet {
	return &pendingSet{src: src, byID: map[hash.ID]*prepared{}, stored: stored}
}

// changesets returns the changesets ids that exist, from the pending set,
// then the batch's prefetched stacks, then the database; p may be nil.
func (p *pendingSet) changesets(ctx context.Context, r *Repo, ids []hash.ID) (map[hash.ID]*Changeset, error) {
	if p == nil {
		return r.Changesets(ctx, ids)
	}
	out := make(map[hash.ID]*Changeset, len(ids))
	var rest []hash.ID
	for _, id := range ids {
		if pc, ok := p.byID[id]; ok {
			out[id] = pc.cs
		} else if cs, ok := p.stored[id]; ok {
			out[id] = cs
		} else {
			rest = append(rest, id)
		}
	}
	if len(rest) > 0 {
		got, err := r.Changesets(ctx, rest)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, got)
	}
	return out, nil
}

func (p *pendingSet) add(pc *prepared) {
	if _, ok := p.byID[pc.cs.ID]; ok {
		return
	}
	p.list = append(p.list, pc)
	p.byID[pc.cs.ID] = pc
}

// truncate drops the changesets added after the first n.
func (p *pendingSet) truncate(n int) {
	for _, pc := range p.list[n:] {
		delete(p.byID, pc.cs.ID)
	}
	p.list = p.list[:n]
}

// Core implements derived.Source.
func (p *pendingSet) Core(ctx context.Context, ids []hash.ID) (map[hash.ID]meta.CoreRow, error) {
	out := make(map[hash.ID]meta.CoreRow, len(ids))
	var rest []hash.ID
	for _, id := range ids {
		if pc, ok := p.byID[id]; ok {
			out[id] = pc.cs.Core
		} else {
			rest = append(rest, id)
		}
	}
	if len(rest) > 0 {
		got, err := p.src.Core(ctx, rest)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, got)
	}
	return out, nil
}

// Parents implements derived.Source.
func (p *pendingSet) Parents(ctx context.Context, ids []hash.ID) (map[hash.ID][]hash.ID, error) {
	out := make(map[hash.ID][]hash.ID, len(ids))
	var rest []hash.ID
	for _, id := range ids {
		if pc, ok := p.byID[id]; ok {
			out[id] = pc.cs.Parents
		} else {
			rest = append(rest, id)
		}
	}
	if len(rest) > 0 {
		got, err := p.src.Parents(ctx, rest)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, got)
	}
	return out, nil
}

// commitBatch inserts the pending changesets and moves target from old to
// tip in one transaction (a fast-forward by construction: tip descends from
// old through the batch's rebased chains). On a lost CAS nothing is written.
func (r *Repo) commitBatch(ctx context.Context, pending *pendingSet, target string, old, tip hash.ID, actor string) error {
	var update meta.MainlineUpdate
	if target == MainRef {
		rows, err := pending.Core(ctx, []hash.ID{old, tip})
		if err != nil {
			return err
		}
		if update.Rows, err = firstParentRows(ctx, pending, rows[tip], rows[old].FPDepth); err != nil {
			return err
		}
	}
	news := make([]meta.NewChangeset, len(pending.list))
	for i, pc := range pending.list {
		news[i] = meta.NewChangeset{Row: pc.row, Files: pc.files, Core: pc.cs.Core}
	}
	_, _, err := r.meta.InsertAndCASRef(ctx, r.id, news, target, old, tip, update, meta.RefLogEntry{Actor: actor, Reason: "land"})
	if err != nil {
		return err
	}
	for _, pc := range pending.list {
		r.cacheInserted(pc)
	}
	return nil
}
