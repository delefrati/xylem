package meta

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
)

// copyThreshold is the file-row count above which InsertChangeset uses COPY.
const copyThreshold = 1000

// Bounds for the denormalized time columns: years 1 through 9999 UTC. The
// changeset body keeps the exact value.
const (
	minColumnTime = -62135596800
	maxColumnTime = 253402300799
)

// PG implements Store on Postgres.
type PG struct {
	pool  *pgxpool.Pool
	repos sync.Map // name → RepoID; repos are never deleted or renamed
}

// NewPG returns a Store backed by pool.
func NewPG(pool *pgxpool.Pool) *PG {
	return &PG{pool: pool}
}

var _ Store = (*PG)(nil)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateRepo implements Store.
func (p *PG) CreateRepo(ctx context.Context, name string) (RepoID, error) {
	var id RepoID
	err := p.pool.QueryRow(ctx, "INSERT INTO repos (name) VALUES ($1) RETURNING id", name).Scan(&id)
	if isUniqueViolation(err) {
		return 0, fmt.Errorf("repo %q: %w", name, ErrExists)
	}
	if err != nil {
		return 0, fmt.Errorf("creating repo %q: %w", name, err)
	}
	p.repos.Store(name, id)
	return id, nil
}

// LookupRepo implements Store. Results are cached for the life of p.
func (p *PG) LookupRepo(ctx context.Context, name string) (RepoID, error) {
	if id, ok := p.repos.Load(name); ok {
		return id.(RepoID), nil
	}
	var id RepoID
	err := p.pool.QueryRow(ctx, "SELECT id FROM repos WHERE name = $1", name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("repo %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("looking up repo %q: %w", name, err)
	}
	p.repos.Store(name, id)
	return id, nil
}

// ---- id conversions

func idBytes(ids []hash.ID) [][]byte {
	out := make([][]byte, len(ids))
	for i := range ids {
		out[i] = ids[i][:]
	}
	return out
}

func toID(b []byte) (hash.ID, error) {
	var id hash.ID
	if len(b) != hash.Size {
		return id, fmt.Errorf("id has %d bytes, want %d", len(b), hash.Size)
	}
	copy(id[:], b)
	return id, nil
}

// nullableID maps the zero ID to NULL.
func nullableID(id hash.ID) []byte {
	if id.IsZero() {
		return nil
	}
	return id[:]
}

func columnTime(unix int64) time.Time {
	return time.Unix(min(max(unix, minColumnTime), maxColumnTime), 0).UTC()
}

// ---- changesets

// InsertChangeset implements Store (07 §CreateChangeset step 10).
func (p *PG) InsertChangeset(ctx context.Context, repo RepoID, cs ChangesetRow, files []FileRow, core CoreRow) (int64, bool, error) {
	if core.ChangesetID != cs.ID {
		return 0, false, fmt.Errorf("insert changeset %s: core row is for %s", cs.ID, core.ChangesetID)
	}
	fileValues, err := fileRowValues(repo, cs.ID, files)
	if err != nil {
		return 0, false, fmt.Errorf("insert changeset %s: %w", cs.ID, err)
	}

	var seq int64
	inserted := true
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO changesets (repo_id, id, parents, body, author, author_time, committer, committer_time, summary, git_sha)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT DO NOTHING
			RETURNING seq`,
			repo, cs.ID[:], idBytes(cs.Parents), cs.Body, cs.Author, columnTime(cs.AuthorTime),
			cs.Committer, columnTime(cs.CommitterTime), cs.Summary, cs.GitSHA,
		).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			inserted = false
			return errAlreadyExists // roll back; nothing else to write
		}
		if err != nil {
			return fmt.Errorf("inserting changeset row: %w", err)
		}
		for i := range fileValues {
			fileValues[i][2] = seq
		}
		if err := insertFiles(ctx, tx, fileValues); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO core_derived (repo_id, changeset_id, root_tree, gen, fp_depth, fp_parent, fp_jump)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			repo, cs.ID[:], core.RootTree[:], core.Gen, core.FPDepth, nullableID(core.FPParent), core.FPJump[:])
		if err != nil {
			return fmt.Errorf("inserting core row: %w", err)
		}
		return nil
	})
	if errors.Is(err, errAlreadyExists) {
		err = p.pool.QueryRow(ctx, "SELECT seq FROM changesets WHERE repo_id = $1 AND id = $2", repo, cs.ID[:]).Scan(&seq)
		if err != nil {
			return 0, false, fmt.Errorf("insert changeset %s: reading existing seq: %w", cs.ID, err)
		}
		return seq, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert changeset %s: %w", cs.ID, err)
	}
	return seq, inserted, nil
}

var errAlreadyExists = errors.New("changeset already exists")

var fileColumns = []string{"repo_id", "changeset_id", "seq", "path", "op", "mode", "size", "blob_id"}

// fileRowValues converts rows to column values; seq (index 2) is filled in later.
func fileRowValues(repo RepoID, id hash.ID, files []FileRow) ([][]any, error) {
	values := make([][]any, len(files))
	for i, f := range files {
		row := []any{repo, id[:], int64(0), f.Path, int16(f.Op), nil, nil, nil}
		switch f.Op {
		case changeset.OpPut:
			if f.Size > math.MaxInt64 {
				return nil, fmt.Errorf("file %q: size %d does not fit BIGINT", f.Path, f.Size)
			}
			row[5], row[6], row[7] = int16(f.Mode), int64(f.Size), f.Blob[:]
		case changeset.OpDelete:
		default:
			return nil, fmt.Errorf("file %q: invalid op %d", f.Path, f.Op)
		}
		values[i] = row
	}
	return values, nil
}

func insertFiles(ctx context.Context, tx pgx.Tx, values [][]any) error {
	if len(values) > copyThreshold {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"changeset_files"}, fileColumns, pgx.CopyFromRows(values)); err != nil {
			return fmt.Errorf("copying file rows: %w", err)
		}
		return nil
	}
	batch := &pgx.Batch{}
	for _, v := range values {
		batch.Queue(`INSERT INTO changeset_files (repo_id, changeset_id, seq, path, op, mode, size, blob_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, v...)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("inserting file rows: %w", err)
	}
	return nil
}

// GetChangesets implements Store. Missing ids are absent from the result.
func (p *PG) GetChangesets(ctx context.Context, repo RepoID, ids []hash.ID) (map[hash.ID]ChangesetRow, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, seq, parents, body, author, author_time, committer, committer_time, summary, git_sha
		FROM changesets WHERE repo_id = $1 AND id = ANY($2)`, repo, idBytes(ids))
	if err != nil {
		return nil, fmt.Errorf("getting changesets: %w", err)
	}
	out := make(map[hash.ID]ChangesetRow, len(ids))
	var (
		id, body, author, committer, gitSHA []byte
		parents                             [][]byte
		authorTime, committerTime           time.Time
		cs                                  ChangesetRow
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &cs.Seq, &parents, &body, &author, &authorTime, &committer, &committerTime, &cs.Summary, &gitSHA}, func() error {
		row := cs
		var err error
		if row.ID, err = toID(id); err != nil {
			return err
		}
		row.Parents = make([]hash.ID, len(parents))
		for i, p := range parents {
			if row.Parents[i], err = toID(p); err != nil {
				return err
			}
		}
		row.Body, row.Author, row.Committer, row.GitSHA = body, author, committer, gitSHA
		row.AuthorTime, row.CommitterTime = authorTime.Unix(), committerTime.Unix()
		out[row.ID] = row
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("getting changesets: %w", err)
	}
	return out, nil
}

// GetCore implements Store. Missing ids are absent from the result.
func (p *PG) GetCore(ctx context.Context, repo RepoID, ids []hash.ID) (map[hash.ID]CoreRow, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT changeset_id, root_tree, gen, fp_depth, fp_parent, fp_jump
		FROM core_derived WHERE repo_id = $1 AND changeset_id = ANY($2)`, repo, idBytes(ids))
	if err != nil {
		return nil, fmt.Errorf("getting core rows: %w", err)
	}
	out := make(map[hash.ID]CoreRow, len(ids))
	var (
		id, root, fpParent, jump []byte
		c                        CoreRow
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &root, &c.Gen, &c.FPDepth, &fpParent, &jump}, func() error {
		row := c
		var err error
		if row.ChangesetID, err = toID(id); err != nil {
			return err
		}
		if row.RootTree, err = toID(root); err != nil {
			return err
		}
		if fpParent != nil {
			if row.FPParent, err = toID(fpParent); err != nil {
				return err
			}
		}
		if row.FPJump, err = toID(jump); err != nil {
			return err
		}
		out[row.ChangesetID] = row
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("getting core rows: %w", err)
	}
	return out, nil
}

func collectIDs(rows pgx.Rows) ([]hash.ID, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (hash.ID, error) {
		var b []byte
		if err := row.Scan(&b); err != nil {
			return hash.ID{}, err
		}
		return toID(b)
	})
}

// ResolvePrefix implements Store.
func (p *PG) ResolvePrefix(ctx context.Context, repo RepoID, lo, hi []byte, limit int) ([]hash.ID, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id FROM changesets
		WHERE repo_id = $1 AND id >= $2 AND ($3::bytea IS NULL OR id < $3)
		ORDER BY id LIMIT $4`, repo, lo, hi, limit)
	if err != nil {
		return nil, fmt.Errorf("resolving prefix: %w", err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("resolving prefix: %w", err)
	}
	return ids, nil
}

// GetByGitSHA implements Store. sha is the 20-byte binary SHA-1.
func (p *PG) GetByGitSHA(ctx context.Context, repo RepoID, sha []byte) (hash.ID, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, "SELECT id FROM changesets WHERE repo_id = $1 AND git_sha = $2", repo, sha).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return hash.ID{}, fmt.Errorf("git sha %x: %w", sha, ErrNotFound)
	}
	if err != nil {
		return hash.ID{}, fmt.Errorf("looking up git sha %x: %w", sha, err)
	}
	return toID(b)
}

// ChangesetFiles implements Store, ordered by path.
func (p *PG) ChangesetFiles(ctx context.Context, repo RepoID, id hash.ID) ([]FileRow, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT path, op, mode, size, blob_id FROM changeset_files
		WHERE repo_id = $1 AND changeset_id = $2 ORDER BY path`, repo, id[:])
	if err != nil {
		return nil, fmt.Errorf("getting files of %s: %w", id, err)
	}
	files, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (FileRow, error) {
		var (
			f    FileRow
			op   int16
			mode *int16
			size *int64
			blob []byte
		)
		if err := row.Scan(&f.Path, &op, &mode, &size, &blob); err != nil {
			return f, err
		}
		if op < 1 || op > 2 {
			return f, fmt.Errorf("file %q: invalid op %d", f.Path, op)
		}
		f.Op = changeset.Op(op)
		if f.Op == changeset.OpPut {
			if mode == nil || size == nil || blob == nil || *mode < 1 || *mode > 255 || *size < 0 {
				return f, fmt.Errorf("file %q: incomplete PUT row", f.Path)
			}
			f.Mode, f.Size = object.Mode(*mode), uint64(*size)
			var err error
			if f.Blob, err = toID(blob); err != nil {
				return f, err
			}
		}
		return f, nil
	})
	if err != nil {
		return nil, fmt.Errorf("getting files of %s: %w", id, err)
	}
	return files, nil
}

// ---- refs and mainline

// GetRef implements Store.
func (p *PG) GetRef(ctx context.Context, repo RepoID, name string) (Ref, error) {
	return getRef(ctx, p.pool, repo, name)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getRef(ctx context.Context, q querier, repo RepoID, name string) (Ref, error) {
	ref := Ref{Name: name}
	var b []byte
	err := q.QueryRow(ctx, "SELECT changeset_id, version FROM refs WHERE repo_id = $1 AND name = $2", repo, name).Scan(&b, &ref.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ref{}, fmt.Errorf("ref %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Ref{}, fmt.Errorf("getting ref %q: %w", name, err)
	}
	if ref.ID, err = toID(b); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// CreateRef implements Store.
func (p *PG) CreateRef(ctx context.Context, repo RepoID, name string, id hash.ID, mainline []MainlineRow, log RefLogEntry) (Ref, error) {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO refs (repo_id, name, changeset_id) VALUES ($1, $2, $3)", repo, name, id[:]); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("ref %q: %w", name, ErrExists)
			}
			return fmt.Errorf("inserting ref: %w", err)
		}
		if err := insertMainline(ctx, tx, repo, name, mainline); err != nil {
			return err
		}
		return insertRefLog(ctx, tx, repo, name, 1, hash.ID{}, id, log)
	})
	if err != nil {
		return Ref{}, fmt.Errorf("create ref %q: %w", name, err)
	}
	return Ref{Name: name, ID: id, Version: 1}, nil
}

// CASRef implements Store.
func (p *PG) CASRef(ctx context.Context, repo RepoID, name string, old, new hash.ID, mainline MainlineUpdate, log RefLogEntry) (Ref, error) {
	ref := Ref{Name: name, ID: new}
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE refs SET changeset_id = $4, version = version + 1, updated_at = now()
			WHERE repo_id = $1 AND name = $2 AND changeset_id = $3
			RETURNING version`, repo, name, old[:], new[:]).Scan(&ref.Version)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := getRef(ctx, tx, repo, name); err != nil {
				return err // ErrNotFound
			}
			return fmt.Errorf("ref %q is not at %s: %w", name, old.Short(12), ErrCASMismatch)
		}
		if err != nil {
			return fmt.Errorf("updating ref: %w", err)
		}
		if mainline.Rewind {
			if _, err := tx.Exec(ctx, "DELETE FROM mainline WHERE repo_id = $1 AND ref_name = $2 AND fp_depth > $3",
				repo, name, mainline.KeepThrough); err != nil {
				return fmt.Errorf("rewinding mainline: %w", err)
			}
		}
		if err := insertMainline(ctx, tx, repo, name, mainline.Rows); err != nil {
			return err
		}
		return insertRefLog(ctx, tx, repo, name, ref.Version, old, new, log)
	})
	if err != nil {
		return Ref{}, fmt.Errorf("cas ref %q: %w", name, err)
	}
	return ref, nil
}

func insertMainline(ctx context.Context, tx pgx.Tx, repo RepoID, ref string, rows []MainlineRow) error {
	if len(rows) == 0 {
		return nil
	}
	values := make([][]any, len(rows))
	for i, r := range rows {
		values[i] = []any{repo, ref, r.Depth, r.ID[:]}
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"mainline"}, []string{"repo_id", "ref_name", "fp_depth", "changeset_id"}, pgx.CopyFromRows(values))
	if err != nil {
		return fmt.Errorf("inserting mainline rows: %w", err)
	}
	return nil
}

func insertRefLog(ctx context.Context, tx pgx.Tx, repo RepoID, name string, version int64, old, new hash.ID, log RefLogEntry) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO ref_log (repo_id, name, version, old_id, new_id, actor, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, repo, name, version, nullableID(old), new[:], log.Actor, log.Reason)
	if err != nil {
		return fmt.Errorf("inserting ref log: %w", err)
	}
	return nil
}

// MainlineLookup implements Store.
func (p *PG) MainlineLookup(ctx context.Context, repo RepoID, ref string, ids []hash.ID) (map[hash.ID]int64, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT changeset_id, fp_depth FROM mainline
		WHERE repo_id = $1 AND ref_name = $2 AND changeset_id = ANY($3)`, repo, ref, idBytes(ids))
	if err != nil {
		return nil, fmt.Errorf("mainline lookup: %w", err)
	}
	out := make(map[hash.ID]int64, len(ids))
	var (
		b     []byte
		depth int64
	)
	_, err = pgx.ForEachRow(rows, []any{&b, &depth}, func() error {
		id, err := toID(b)
		out[id] = depth
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("mainline lookup: %w", err)
	}
	return out, nil
}

// MainlineAt implements Store.
func (p *PG) MainlineAt(ctx context.Context, repo RepoID, ref string, depth int64) (hash.ID, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, `SELECT changeset_id FROM mainline WHERE repo_id = $1 AND ref_name = $2 AND fp_depth = $3`,
		repo, ref, depth).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return hash.ID{}, fmt.Errorf("mainline %q depth %d: %w", ref, depth, ErrNotFound)
	}
	if err != nil {
		return hash.ID{}, fmt.Errorf("mainline %q depth %d: %w", ref, depth, err)
	}
	return toID(b)
}

// PathHistoryMainline implements Store with the query from 04 §Kind 3.
func (p *PG) PathHistoryMainline(ctx context.Context, repo RepoID, ref string, path []byte, maxDepth, beforeSeq int64, limit int) ([]PathHit, error) {
	if len(path) == 0 {
		return nil, errors.New("path history: empty path")
	}
	lo, hi := paths.DirPrefixRange(path)
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (cf.seq) cf.changeset_id, cf.seq
		FROM changeset_files cf
		JOIN mainline ml ON ml.repo_id = cf.repo_id AND ml.ref_name = $2 AND ml.changeset_id = cf.changeset_id
		WHERE cf.repo_id = $1
		  AND (cf.path = $3 OR (cf.path >= $4 AND cf.path < $5))
		  AND ml.fp_depth <= $6
		  AND cf.seq < $7
		ORDER BY cf.seq DESC
		LIMIT $8`, repo, ref, path, lo, hi, maxDepth, beforeSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("path history: %w", err)
	}
	hits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PathHit, error) {
		var h PathHit
		var b []byte
		if err := row.Scan(&b, &h.Seq); err != nil {
			return h, err
		}
		var err error
		h.ChangesetID, err = toID(b)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("path history: %w", err)
	}
	return hits, nil
}
