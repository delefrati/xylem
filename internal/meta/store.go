package meta

import (
	"context"
	"errors"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

var (
	// ErrNotFound reports a missing repo, changeset, ref or mainline entry.
	ErrNotFound = errors.New("not found")
	// ErrExists reports a repo or ref that already exists.
	ErrExists = errors.New("already exists")
	// ErrCASMismatch reports a ref whose current value is not the expected one.
	ErrCASMismatch = errors.New("ref compare-and-swap mismatch")
)

// RepoID identifies a repository in the database.
type RepoID int16

// ChangesetRow is a row of the changesets table. Body is the canonical
// encoding and the source of truth; the other fields are denormalized.
type ChangesetRow struct {
	ID            hash.ID
	Seq           int64 // assigned on insert
	Parents       []hash.ID
	Body          []byte
	Author        []byte
	AuthorTime    int64 // Unix seconds, clamped to years 1–9999 in the column
	Committer     []byte
	CommitterTime int64 // as AuthorTime
	Summary       string
	GitSHA        []byte // 20 bytes, or nil
}

// FileRow is a row of changeset_files: one changed path. Mode, Size and Blob
// are stored as NULL for deletes.
type FileRow struct {
	Path []byte
	Op   changeset.Op
	Mode object.Mode
	Size uint64
	Blob hash.ID
}

// CoreRow is a row of core_derived (04 §Kind 1). FPParent is the zero ID for
// roots (NULL in the table).
type CoreRow struct {
	ChangesetID hash.ID
	RootTree    hash.ID
	Gen         int64
	FPDepth     int64
	FPParent    hash.ID
	FPJump      hash.ID
}

// Ref is a named pointer to a changeset with a version incremented on every
// update.
type Ref struct {
	Name    string
	ID      hash.ID
	Version int64
}

// MainlineRow is one entry of a ref's first-parent chain.
type MainlineRow struct {
	Depth int64
	ID    hash.ID
}

// MainlineUpdate describes how CASRef changes a ref's mainline in the same
// transaction as the ref update (03 §Invariants). For a fast-forward, Rewind
// is false and Rows are the new depths. For a forced update, rows with depth
// greater than KeepThrough are deleted first.
type MainlineUpdate struct {
	Rewind      bool
	KeepThrough int64
	Rows        []MainlineRow
}

// RefLogEntry records who changed a ref and why.
type RefLogEntry struct {
	Actor  string
	Reason string // "land", "import", "admin"
}

// GitMatch is an imported changeset found by git sha prefix.
type GitMatch struct {
	GitSHA []byte
	ID     hash.ID
}

// PathHit is a changeset that changed a path, with its seq.
type PathHit struct {
	ChangesetID hash.ID
	Seq         int64
}

// GitBlob maps a git blob to the Xylem blob holding the same content.
type GitBlob struct {
	GitSHA [20]byte
	BlobID hash.ID
	Size   int64
}

// DerivedKey identifies a lazily computed derived value (04 §Framework).
type DerivedKey struct {
	Kind      string
	Version   int16
	Changeset hash.ID
	Key       []byte
}

// Store is the metadata store. Implementations are safe for concurrent use.
type Store interface {
	CreateRepo(ctx context.Context, name string) (RepoID, error)
	LookupRepo(ctx context.Context, name string) (RepoID, error)

	// InsertChangeset inserts a changeset with its file rows and core row in
	// one transaction. If the changeset already exists it returns its seq and
	// inserted = false.
	InsertChangeset(ctx context.Context, repo RepoID, cs ChangesetRow, files []FileRow, core CoreRow) (seq int64, inserted bool, err error)
	GetChangesets(ctx context.Context, repo RepoID, ids []hash.ID) (map[hash.ID]ChangesetRow, error)
	GetCore(ctx context.Context, repo RepoID, ids []hash.ID) (map[hash.ID]CoreRow, error)
	// GetParents returns the parents of each found changeset (first parent first).
	GetParents(ctx context.Context, repo RepoID, ids []hash.ID) (map[hash.ID][]hash.ID, error)
	// ResolvePrefix returns up to limit ids in [lo, hi) in ascending order; a
	// nil hi is unbounded (see hash.ParsePrefix).
	ResolvePrefix(ctx context.Context, repo RepoID, lo, hi []byte, limit int) ([]hash.ID, error)
	GetByGitSHA(ctx context.Context, repo RepoID, sha []byte) (hash.ID, error)
	// ResolveGitPrefix returns up to limit imported changesets whose git sha
	// is in [lo, hi), by git sha; a nil hi is unbounded.
	ResolveGitPrefix(ctx context.Context, repo RepoID, lo, hi []byte, limit int) ([]GitMatch, error)
	ChangesetFiles(ctx context.Context, repo RepoID, id hash.ID) ([]FileRow, error)
	// FilesFor returns the file rows of several changesets, each ordered by path.
	FilesFor(ctx context.Context, repo RepoID, ids []hash.ID) (map[hash.ID][]FileRow, error)
	// ListChangesets returns up to limit changesets with seq > afterSeq, by seq.
	ListChangesets(ctx context.Context, repo RepoID, afterSeq int64, limit int) ([]ChangesetRow, error)
	// SampleChangesets returns up to n changesets chosen at random.
	SampleChangesets(ctx context.Context, repo RepoID, n int) ([]ChangesetRow, error)

	GetRef(ctx context.Context, repo RepoID, name string) (Ref, error)
	// ListRefs returns the refs whose names start with prefix, by name.
	ListRefs(ctx context.Context, repo RepoID, prefix string) ([]Ref, error)
	// CreateRef creates a ref with its mainline rows and a ref_log entry.
	CreateRef(ctx context.Context, repo RepoID, name string, id hash.ID, mainline []MainlineRow, log RefLogEntry) (Ref, error)
	// CASRef moves ref name from old to new, updating the mainline and ref_log
	// in the same transaction. It fails with ErrCASMismatch if the ref no
	// longer points at old.
	CASRef(ctx context.Context, repo RepoID, name string, old, new hash.ID, mainline MainlineUpdate, log RefLogEntry) (Ref, error)

	// MainlineLookup returns the mainline depth of each id that is on ref's mainline.
	MainlineLookup(ctx context.Context, repo RepoID, ref string, ids []hash.ID) (map[hash.ID]int64, error)
	MainlineAt(ctx context.Context, repo RepoID, ref string, depth int64) (hash.ID, error)
	// MainlineRange returns up to limit mainline rows with depth ≤ maxDepth,
	// deepest first.
	MainlineRange(ctx context.Context, repo RepoID, ref string, maxDepth int64, limit int) ([]MainlineRow, error)
	// PathHistoryMainline returns, newest first, mainline changesets with
	// depth ≤ maxDepth and seq < beforeSeq that changed path or anything below
	// it (04 §Kind 3). path must be non-empty.
	PathHistoryMainline(ctx context.Context, repo RepoID, ref string, path []byte, maxDepth, beforeSeq int64, limit int) ([]PathHit, error)

	// GetDerived returns a stored derived value: inline bytes, or the id of a
	// "derived" object holding it. found is false if none is stored.
	GetDerived(ctx context.Context, repo RepoID, k DerivedKey) (value []byte, ref hash.ID, found bool, err error)
	// PutDerived stores a derived value (exactly one of value and a non-zero
	// ref); an existing value is kept.
	PutDerived(ctx context.Context, repo RepoID, k DerivedKey, value []byte, ref hash.ID) error

	// GitSHAMap returns git commit sha → changeset id for every imported changeset.
	GitSHAMap(ctx context.Context, repo RepoID) (map[[20]byte]hash.ID, error)
	// GetImportState returns the last imported git sha for (source, branch),
	// or ErrNotFound.
	GetImportState(ctx context.Context, repo RepoID, source, branch string) ([20]byte, error)
	PutImportState(ctx context.Context, repo RepoID, source, branch string, lastGitSHA [20]byte) error
	// GetGitBlobs returns the known mappings among shas.
	GetGitBlobs(ctx context.Context, repo RepoID, shas [][20]byte) (map[[20]byte]GitBlob, error)
	// InsertGitBlobs records mappings, ignoring ones already present.
	InsertGitBlobs(ctx context.Context, repo RepoID, blobs []GitBlob) error
}
