package repo

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
)

// DefaultLandAttempts is Land's default max_attempts (07 §Land).
const DefaultLandAttempts = 10

// LandOptions configures Land.
type LandOptions struct {
	Target      string // ref to land on (default MainRef)
	MaxAttempts int    // CAS attempts (default DefaultLandAttempts)
	Committer   []byte // committer of rebased changesets (default each entry's author)
	Actor       string // recorded in the ref log
	// BeforeCAS, if set, runs after an attempt's rebase and before its CAS.
	// It is a fault-injection hook for tests: an error aborts Land there.
	BeforeCAS func(ctx context.Context, attempt int) error
}

// LandResult is the outcome of Land. Conflicts non-empty means nothing landed.
type LandResult struct {
	Landed      []hash.ID // the rebased stack now on the target, bottom → top
	NewTip      hash.ID
	Skipped     []hash.ID // original ids already present on the target
	Conflicts   []Conflict
	Conflicting hash.ID
	Attempts    int
}

// Land rebases stack onto the target ref's tip and moves the ref there with
// a compare-and-swap (07 §Land), retrying from the original stack with
// jittered backoff when the tip moved. The ref, mainline rows and ref log
// change in one transaction. Conflicts are returned in the result, not as an
// error; running out of attempts is ErrConflict. With
// Options.LandBatchWindow set, the request joins its target's batch instead
// (landbatch.go): Attempts then counts the batch's CAS attempts, and NewTip
// is the request's own top changeset (the batch may land more above it).
func (r *Repo) Land(ctx context.Context, stack []hash.ID, opts LandOptions) (*LandResult, error) {
	if opts.Target == "" {
		opts.Target = MainRef
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultLandAttempts
	}
	if r.lander != nil {
		return r.lander.land(ctx, stack, opts)
	}
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		ref, err := r.GetRef(ctx, opts.Target)
		if err != nil {
			return nil, err
		}
		rb, err := r.RebaseStack(ctx, stack, ref.ID, RebaseOptions{Committer: opts.Committer, Time: r.opts.Now().Unix()})
		if err != nil {
			return nil, err
		}
		res := &LandResult{Skipped: rb.Skipped, Attempts: attempt}
		if len(rb.Conflicts) > 0 {
			res.Conflicts, res.Conflicting = rb.Conflicts, rb.Conflicting
			return res, nil
		}
		if len(rb.Rebased) == 0 { // every entry already on the target: nothing to move
			res.NewTip = ref.ID
			return res, nil
		}
		if opts.BeforeCAS != nil {
			if err := opts.BeforeCAS(ctx, attempt); err != nil {
				return nil, err
			}
		}
		_, err = r.UpdateRef(ctx, opts.Target, ref.ID, rb.Tip, false, opts.Actor, "land")
		if errors.Is(err, meta.ErrCASMismatch) {
			if err := sleep(ctx, landBackoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		res.Landed, res.NewTip = rb.Rebased, rb.Tip
		return res, nil
	}
	return nil, fmt.Errorf("%w: landing on %q: the tip kept moving (%d attempts)", ErrConflict, opts.Target, opts.MaxAttempts)
}

// landBackoff is jitter(n) = rand(0, min(200ms, 5ms·2^n)).
func landBackoff(attempt int) time.Duration {
	ceiling := 200 * time.Millisecond
	if attempt < 6 {
		ceiling = min(ceiling, 5*time.Millisecond<<attempt)
	}
	return rand.N(ceiling) //nolint:gosec // G404: backoff jitter, not security
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
