package meta

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/delefrati/xylem/internal/hash"
)

// GetDerived implements Store.
func (p *PG) GetDerived(ctx context.Context, repo RepoID, k DerivedKey) ([]byte, hash.ID, bool, error) {
	var value, ref []byte
	err := p.pool.QueryRow(ctx, `
		SELECT value, value_ref FROM derived
		WHERE repo_id = $1 AND kind = $2 AND kind_version = $3 AND changeset_id = $4 AND key = $5`,
		repo, k.Kind, k.Version, k.Changeset[:], nonNil(k.Key)).Scan(&value, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, hash.ID{}, false, nil
	}
	if err != nil {
		return nil, hash.ID{}, false, fmt.Errorf("getting derived %s: %w", k.Kind, err)
	}
	if ref != nil {
		id, err := toID(ref)
		return nil, id, err == nil, err
	}
	return nonNil(value), hash.ID{}, true, nil
}

// PutDerived implements Store.
func (p *PG) PutDerived(ctx context.Context, repo RepoID, k DerivedKey, value []byte, ref hash.ID) error {
	var refCol, valueCol any
	if ref.IsZero() {
		valueCol = nonNil(value)
	} else {
		refCol = ref[:]
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO derived (repo_id, kind, kind_version, changeset_id, key, value, value_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
		repo, k.Kind, k.Version, k.Changeset[:], nonNil(k.Key), valueCol, refCol)
	if err != nil {
		return fmt.Errorf("storing derived %s: %w", k.Kind, err)
	}
	return nil
}

// nonNil maps nil to an empty slice (the key column is NOT NULL, and an empty
// inline value must stay distinct from "no value").
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
