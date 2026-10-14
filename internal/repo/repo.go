// Package repo is the domain layer: CreateChangeset, RebaseStack, Land,
// UpdateRef and history queries. It composes meta, store, treeops and derived,
// and is used by both the server and the importer.
package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/delefrati/xylem/internal/derived"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/treeops"
)

// Errors returned by Repo methods. Callers map them to API status codes:
// ErrInvalid → InvalidArgument, *MissingBlobsError → FailedPrecondition,
// ErrNotFound → NotFound, ErrConflict → Aborted.
var (
	ErrInvalid  = errors.New("invalid argument")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	// ErrBinary reports a query that needs text, e.g. blame of a binary file
	// (FailedPrecondition).
	ErrBinary = errors.New("binary file")
)

// DefaultMaxBlobSize is the largest blob a changeset may reference (03 §Stored
// object format: blobs over 64 MiB are rejected in the MVP).
const DefaultMaxBlobSize = 64 << 20

// Options configures a Repo.
type Options struct {
	MaxBlobSize    uint64 // default DefaultMaxBlobSize
	TreeCacheSize  int    // decoded trees; default 100k
	CoreCacheSize  int    // core rows and parent lists; default 1M
	DerivedCache   int    // lazily derived values (blame); default 100k
	SizeCheckLimit int    // concurrent blob header reads; default 16
	Logger         *slog.Logger
	Now            func() time.Time // default time.Now
}

// Repo is one repository. It is safe for concurrent use.
type Repo struct {
	name   string
	id     meta.RepoID
	meta   meta.Store
	objs   store.ObjectStore
	trees  *treeops.Trees
	source *derived.CachedSource
	engine *derived.Engine
	opts   Options
}

// Open returns the repository called name.
func Open(ctx context.Context, m meta.Store, objs store.ObjectStore, name string, opts Options) (*Repo, error) {
	id, err := m.LookupRepo(ctx, name)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, fmt.Errorf("repo %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if opts.MaxBlobSize == 0 {
		opts.MaxBlobSize = DefaultMaxBlobSize
	}
	if opts.TreeCacheSize == 0 {
		opts.TreeCacheSize = 100_000
	}
	if opts.CoreCacheSize == 0 {
		opts.CoreCacheSize = 1_000_000
	}
	if opts.DerivedCache == 0 {
		opts.DerivedCache = 100_000
	}
	if opts.SizeCheckLimit == 0 {
		opts.SizeCheckLimit = 16
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Repo{
		name:   name,
		id:     id,
		meta:   m,
		objs:   objs,
		trees:  treeops.NewTrees(objs, opts.TreeCacheSize),
		source: derived.NewCachedSource(m, id, opts.CoreCacheSize),
		engine: derived.NewEngine(m, id, objs, opts.DerivedCache),
		opts:   opts,
	}, nil
}

// Name returns the repository name.
func (r *Repo) Name() string { return r.name }

// Objects returns the repository's object store.
func (r *Repo) Objects() store.ObjectStore { return r.objs }

// Trees returns the repository's tree reader.
func (r *Repo) Trees() *treeops.Trees { return r.trees }

// Source returns the repository's cached core-derived source.
func (r *Repo) Source() derived.Source { return r.source }
