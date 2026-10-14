package meta

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/delefrati/xylem/internal/hash"
)

// GitSHAMap implements Store.
func (p *PG) GitSHAMap(ctx context.Context, repo RepoID) (map[[20]byte]hash.ID, error) {
	rows, err := p.pool.Query(ctx, "SELECT git_sha, id FROM changesets WHERE repo_id = $1 AND git_sha IS NOT NULL", repo)
	if err != nil {
		return nil, fmt.Errorf("loading git sha map: %w", err)
	}
	out := map[[20]byte]hash.ID{}
	var sha, id []byte
	_, err = pgx.ForEachRow(rows, []any{&sha, &id}, func() error {
		cs, err := toID(id)
		if err != nil {
			return err
		}
		g, err := toGitSHA(sha)
		out[g] = cs
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("loading git sha map: %w", err)
	}
	return out, nil
}

func toGitSHA(b []byte) ([20]byte, error) {
	var s [20]byte
	if len(b) != len(s) {
		return s, fmt.Errorf("git sha has %d bytes, want 20", len(b))
	}
	copy(s[:], b)
	return s, nil
}

// GetImportState implements Store.
func (p *PG) GetImportState(ctx context.Context, repo RepoID, source, branch string) ([20]byte, error) {
	var b []byte
	err := p.pool.QueryRow(ctx, "SELECT last_git_sha FROM import_state WHERE repo_id = $1 AND source = $2 AND branch = $3",
		repo, source, branch).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b == nil) {
		return [20]byte{}, fmt.Errorf("import state for %s %s: %w", source, branch, ErrNotFound)
	}
	if err != nil {
		return [20]byte{}, fmt.Errorf("reading import state: %w", err)
	}
	return toGitSHA(b)
}

// PutImportState implements Store.
func (p *PG) PutImportState(ctx context.Context, repo RepoID, source, branch string, lastGitSHA [20]byte) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO import_state (repo_id, source, branch, last_git_sha) VALUES ($1, $2, $3, $4)
		ON CONFLICT (repo_id, source, branch) DO UPDATE SET last_git_sha = EXCLUDED.last_git_sha, updated_at = now()`,
		repo, source, branch, lastGitSHA[:])
	if err != nil {
		return fmt.Errorf("writing import state: %w", err)
	}
	return nil
}

// GetGitBlobs implements Store.
func (p *PG) GetGitBlobs(ctx context.Context, repo RepoID, shas [][20]byte) (map[[20]byte]GitBlob, error) {
	keys := make([][]byte, len(shas))
	for i := range shas {
		keys[i] = shas[i][:]
	}
	rows, err := p.pool.Query(ctx, "SELECT git_sha, blob_id, size FROM git_blobs WHERE repo_id = $1 AND git_sha = ANY($2)", repo, keys)
	if err != nil {
		return nil, fmt.Errorf("getting git blobs: %w", err)
	}
	out := make(map[[20]byte]GitBlob, len(shas))
	var (
		sha, id []byte
		size    int64
	)
	_, err = pgx.ForEachRow(rows, []any{&sha, &id, &size}, func() error {
		b := GitBlob{Size: size}
		var err error
		if b.GitSHA, err = toGitSHA(sha); err != nil {
			return err
		}
		if b.BlobID, err = toID(id); err != nil {
			return err
		}
		out[b.GitSHA] = b
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("getting git blobs: %w", err)
	}
	return out, nil
}

// InsertGitBlobs implements Store: COPY into a temporary table, then insert
// with ON CONFLICT DO NOTHING (08 §Blob mapping table).
func (p *PG) InsertGitBlobs(ctx context.Context, repo RepoID, blobs []GitBlob) error {
	if len(blobs) == 0 {
		return nil
	}
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE git_blobs_in (git_sha BYTEA, blob_id BYTEA, size BIGINT) ON COMMIT DROP`); err != nil {
			return err
		}
		values := make([][]any, len(blobs))
		for i, b := range blobs {
			values[i] = []any{b.GitSHA[:], b.BlobID[:], b.Size}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"git_blobs_in"}, []string{"git_sha", "blob_id", "size"}, pgx.CopyFromRows(values)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO git_blobs (repo_id, git_sha, blob_id, size)
			SELECT DISTINCT ON (git_sha) $1, git_sha, blob_id, size FROM git_blobs_in
			ON CONFLICT DO NOTHING`, repo)
		return err
	})
	if err != nil {
		return fmt.Errorf("inserting git blobs: %w", err)
	}
	return nil
}

// ResolveGitPrefix implements Store.
func (p *PG) ResolveGitPrefix(ctx context.Context, repo RepoID, lo, hi []byte, limit int) ([]GitMatch, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT git_sha, id FROM changesets
		WHERE repo_id = $1 AND git_sha >= $2 AND ($3::bytea IS NULL OR git_sha < $3)
		ORDER BY git_sha LIMIT $4`, repo, lo, hi, limit)
	if err != nil {
		return nil, fmt.Errorf("resolving git prefix: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GitMatch, error) {
		var m GitMatch
		var id []byte
		if err := row.Scan(&m.GitSHA, &id); err != nil {
			return m, err
		}
		var err error
		m.ID, err = toID(id)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("resolving git prefix: %w", err)
	}
	return out, nil
}
