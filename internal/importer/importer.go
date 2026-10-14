package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
)

// Options configures an import (08 §Command).
type Options struct {
	Branch    string // git branch to import (default "main")
	Source    string // import_state key for the git repo (default "file://" + absolute dir)
	Batch     int    // commits between progress checkpoints (default 500)
	Limit     int    // import at most this many new commits (0: all)
	Workers   int    // parallel commit readers and blob uploaders (default 8)
	BlobCache int    // git blob → Xylem blob LRU entries (default 100k)
	Actor     string // ref log actor (default "importer")
	Logger    *slog.Logger
}

// Stats counts an import's progress. Safe to read while Run is running.
type Stats struct {
	Commits   int64 // changesets created by this run
	Existing  int64 // commits found already imported (idempotent re-creates)
	Blobs     int64 // blobs read from git and stored
	BlobBytes int64
	Reused    int64 // blobs found in the cache or git_blobs table
	Tip       gitsource.SHA
	MainTip   hash.ID // where main points after the last checkpoint
}

// Importer imports one git branch into a repository.
type Importer struct {
	r    *repo.Repo
	src  *gitsource.Repo
	opts Options

	blobs   *lru.Cache[gitsource.SHA, BlobInfo]
	flights singleflight.Group

	mu      sync.Mutex
	pending []repo.GitBlob // stored blobs not yet recorded in git_blobs

	commits, existing, blobsN, blobBytes, reused atomic.Int64
}

// New returns an Importer of src into r.
func New(r *repo.Repo, src *gitsource.Repo, opts Options) (*Importer, error) {
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	if opts.Source == "" {
		abs, err := filepath.Abs(src.Dir())
		if err != nil {
			return nil, err
		}
		opts.Source = "file://" + abs
	}
	if opts.Batch <= 0 {
		opts.Batch = 500
	}
	if opts.Workers <= 0 {
		opts.Workers = 8
	}
	if opts.BlobCache <= 0 {
		opts.BlobCache = 100_000
	}
	if opts.Actor == "" {
		opts.Actor = "importer"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	cache, err := lru.New[gitsource.SHA, BlobInfo](opts.BlobCache)
	if err != nil {
		return nil, err
	}
	return &Importer{r: r, src: src, opts: opts, blobs: cache}, nil
}

// Stats returns a snapshot of the progress counters.
func (im *Importer) Stats() Stats {
	return Stats{
		Commits: im.commits.Load(), Existing: im.existing.Load(),
		Blobs: im.blobsN.Load(), BlobBytes: im.blobBytes.Load(), Reused: im.reused.Load(),
	}
}

// job is one commit flowing through the pipeline.
type job struct {
	sha    gitsource.SHA
	result chan jobResult // buffered(1), filled by a worker
}

type jobResult struct {
	commit  *gitsource.Commit
	changes []gitsource.Change
	blobs   map[gitsource.SHA]BlobInfo
	err     error
}

// Run imports every commit reachable from the branch that is not imported
// yet, parents before children, and advances main. It is idempotent and
// resumable: re-running after a failure or cancellation continues.
func (im *Importer) Run(ctx context.Context) (Stats, error) {
	log := im.opts.Logger
	tip, err := im.src.ResolveBranch(ctx, im.opts.Branch)
	if err != nil {
		return Stats{}, err
	}
	shaMap, err := im.r.GitSHAMap(ctx)
	if err != nil {
		return Stats{}, err
	}
	main, err := im.currentMain(ctx)
	if err != nil {
		return Stats{}, err
	}
	ck := &checkpoint{im: im, main: main}

	if id, ok := shaMap[tip]; ok { // up to date
		ck.lastOnChain, ck.lastSHA = id, tip
		if err := ck.flush(ctx); err != nil {
			return Stats{}, err
		}
		return im.finalStats(tip, ck), nil
	}

	fpChain, err := im.src.FirstParentChain(ctx, tip)
	if err != nil {
		return Stats{}, err
	}
	var exclude []gitsource.SHA
	if last, ok, err := im.r.ImportState(ctx, im.opts.Source, im.opts.Branch); err != nil {
		return Stats{}, err
	} else if _, imported := shaMap[last]; ok && imported {
		exclude = append(exclude, last)
	}
	log.Info("import starting", "branch", im.opts.Branch, "tip", tip.String(), "already_imported", len(shaMap))

	g, gctx := errgroup.WithContext(ctx)
	jobs := make(chan *job, im.opts.Workers*4)
	ordered := make(chan *job, im.opts.Workers*16)

	g.Go(func() error { // producer
		defer close(jobs)
		defer close(ordered)
		n := 0
		for sha, err := range im.src.RevList(gctx, tip, exclude...) {
			if err != nil {
				return err
			}
			if _, done := shaMap[sha]; done {
				continue
			}
			if im.opts.Limit > 0 && n >= im.opts.Limit {
				return nil
			}
			n++
			j := &job{sha: sha, result: make(chan jobResult, 1)}
			select {
			case jobs <- j:
			case <-gctx.Done():
				return gctx.Err()
			}
			select {
			case ordered <- j:
			case <-gctx.Done():
				return gctx.Err()
			}
		}
		return nil
	})
	for range im.opts.Workers {
		g.Go(func() error {
			for j := range jobs {
				j.result <- im.prepare(gctx, j.sha)
			}
			return nil
		})
	}
	g.Go(func() error { // committer, strictly in topological order
		warned := map[string]bool{}
		for j := range ordered {
			var res jobResult
			select {
			case res = <-j.result:
			case <-gctx.Done():
				return gctx.Err()
			}
			if res.err != nil {
				return res.err
			}
			id, err := im.commit(gctx, res, shaMap, warned)
			if err != nil {
				return err
			}
			shaMap[j.sha] = id
			ck.lastSHA = j.sha
			if fpChain[j.sha] {
				ck.lastOnChain = id
			}
			if ck.count++; ck.count%im.opts.Batch == 0 {
				if err := ck.flush(gctx); err != nil {
					return err
				}
			}
		}
		return nil
	})
	runErr := g.Wait()
	// Checkpoint whatever was committed, even after an error or cancellation,
	// so a re-run resumes from here.
	flushErr := ck.flush(context.WithoutCancel(ctx))
	if err := errors.Join(runErr, flushErr); err != nil {
		return im.finalStats(tip, ck), err
	}
	return im.finalStats(tip, ck), nil
}

func (im *Importer) finalStats(tip gitsource.SHA, ck *checkpoint) Stats {
	s := im.Stats()
	s.Tip, s.MainTip = tip, ck.main
	return s
}

func (im *Importer) currentMain(ctx context.Context) (hash.ID, error) {
	ref, err := im.r.GetRef(ctx, repo.MainRef)
	if errors.Is(err, repo.ErrNotFound) {
		return hash.ID{}, nil
	}
	return ref.ID, err
}

// prepare reads a commit and its diff and uploads the blobs it needs.
func (im *Importer) prepare(ctx context.Context, sha gitsource.SHA) jobResult {
	c, err := im.src.ReadCommit(ctx, sha)
	if err != nil {
		return jobResult{err: err}
	}
	var parent *gitsource.SHA
	if len(c.Parents) > 0 {
		parent = &c.Parents[0]
	}
	changes, err := im.src.DiffTree(ctx, parent, sha)
	if err != nil {
		return jobResult{err: err}
	}
	blobs, err := im.uploadBlobs(ctx, BlobsNeeded(changes))
	if err != nil {
		return jobResult{err: fmt.Errorf("commit %s: %w", sha, err)}
	}
	return jobResult{commit: c, changes: changes, blobs: blobs}
}

// uploadBlobs makes sure each git blob is stored and returns its Xylem id
// and size: from the LRU, then the git_blobs table, then by reading it from
// git and storing it.
func (im *Importer) uploadBlobs(ctx context.Context, shas []gitsource.SHA) (map[gitsource.SHA]BlobInfo, error) {
	out := make(map[gitsource.SHA]BlobInfo, len(shas))
	var misses [][20]byte
	for _, s := range shas {
		if info, ok := im.blobs.Get(s); ok {
			out[s] = info
			im.reused.Add(1)
		} else {
			misses = append(misses, s)
		}
	}
	if len(misses) > 0 {
		known, err := im.r.GitBlobs(ctx, misses)
		if err != nil {
			return nil, err
		}
		for s, b := range known {
			if b.Size < 0 {
				return nil, fmt.Errorf("git blob %x has negative size", s)
			}
			info := BlobInfo{ID: b.BlobID, Size: uint64(b.Size)}
			im.blobs.Add(s, info)
			out[s] = info
			im.reused.Add(1)
		}
	}
	for _, s := range misses {
		if _, ok := out[s]; ok {
			continue
		}
		v, err, _ := im.flights.Do(string(s[:]), func() (any, error) {
			if info, ok := im.blobs.Get(s); ok {
				return info, nil
			}
			return im.storeBlob(ctx, s)
		})
		if err != nil {
			return nil, err
		}
		out[s] = v.(BlobInfo)
	}
	return out, nil
}

func (im *Importer) storeBlob(ctx context.Context, s gitsource.SHA) (BlobInfo, error) {
	data, err := im.src.Blob(ctx, s)
	if err != nil {
		return BlobInfo{}, err
	}
	id, stored := object.Encode(object.KindBlob, data)
	objs := im.r.Objects()
	present, err := objs.Has(ctx, []hash.ID{id})
	if err != nil {
		return BlobInfo{}, err
	}
	if !present[0] {
		if err := objs.Put(ctx, id, stored); err != nil {
			return BlobInfo{}, err
		}
	}
	info := BlobInfo{ID: id, Size: uint64(len(data))}
	im.blobs.Add(s, info)
	im.mu.Lock()
	im.pending = append(im.pending, repo.GitBlob{GitSHA: s, BlobID: id, Size: int64(len(data))})
	im.mu.Unlock()
	im.blobsN.Add(1)
	im.blobBytes.Add(int64(len(data)))
	return info, nil
}

// commit creates the changeset for one prepared commit.
func (im *Importer) commit(ctx context.Context, res jobResult, shaMap map[[20]byte]hash.ID, warned map[string]bool) (hash.ID, error) {
	c := res.commit
	parents := make([]hash.ID, len(c.Parents))
	for i, p := range c.Parents {
		id, ok := shaMap[p]
		if !ok {
			return hash.ID{}, fmt.Errorf("commit %s: parent %s not imported", c.SHA, p)
		}
		parents[i] = id
	}
	m, err := MapCommit(c, res.changes, parents, func(s gitsource.SHA) (BlobInfo, bool) {
		info, ok := res.blobs[s]
		return info, ok
	})
	if err != nil {
		return hash.ID{}, err
	}
	for _, p := range m.Submodules {
		if !warned[string(p)] {
			warned[string(p)] = true
			im.opts.Logger.Warn("skipping submodule", "path", string(p), "commit", c.SHA.String())
		}
	}
	cs, existed, err := im.r.CreateChangeset(ctx, m.Request, repo.CreateOptions{
		AllowRoot: true, AllowEmpty: true, Committer: &m.Committer, TrustSizes: true,
	})
	if err != nil {
		return hash.ID{}, fmt.Errorf("commit %s: %w", c.SHA, err)
	}
	if existed {
		im.existing.Add(1)
	} else {
		im.commits.Add(1)
	}
	return cs.ID, nil
}

// checkpoint records progress: git_blobs rows, import_state and main.
type checkpoint struct {
	im          *Importer
	count       int
	lastSHA     gitsource.SHA // last committed commit; zero if none yet
	lastOnChain hash.ID       // newest committed changeset on the branch's first-parent chain
	main        hash.ID       // current value of main (zero: no ref yet)
}

func (ck *checkpoint) flush(ctx context.Context) error {
	im := ck.im
	im.mu.Lock()
	pending := im.pending
	im.pending = nil
	im.mu.Unlock()
	if err := im.r.RecordGitBlobs(ctx, pending); err != nil {
		return err
	}
	if !ck.lastSHA.IsZero() {
		if err := im.r.SetImportState(ctx, im.opts.Source, im.opts.Branch, ck.lastSHA); err != nil {
			return err
		}
	}
	if ck.lastOnChain.IsZero() || ck.lastOnChain == ck.main {
		return nil
	}
	_, err := im.r.UpdateRef(ctx, repo.MainRef, ck.main, ck.lastOnChain, false, im.opts.Actor, "import")
	if errors.Is(err, repo.ErrNotFastForward) {
		return fmt.Errorf("main (%s) is not on the first-parent history of %s; import into a fresh repository: %w",
			ck.main.Short(12), im.opts.Branch, err)
	}
	if err != nil {
		return err
	}
	ck.main = ck.lastOnChain
	im.opts.Logger.Debug("checkpoint", "commits", ck.count, "main", ck.main.Short(12))
	return nil
}
