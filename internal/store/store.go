// Package store defines the ObjectStore interface and its fs, s3 and memory
// implementations (ADR-003).
//
// Stores hold objects in stored form (a zstd frame, see package object) keyed
// by id. They do not verify that bytes hash to their id; servers verify on
// PutObjects and readers verify data from untrusted tiers.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/delefrati/xylem/internal/hash"
)

// ErrNotFound reports that a store has no object with the requested id.
var ErrNotFound = errors.New("object not found")

// ObjectStore is a content-addressed store of objects in stored form.
// Implementations are safe for concurrent use.
type ObjectStore interface {
	// Get returns the stored bytes of id, or an error wrapping ErrNotFound.
	Get(ctx context.Context, id hash.ID) ([]byte, error)
	// Put stores the stored bytes of id. Putting an id that already exists
	// succeeds without changing it.
	Put(ctx context.Context, id hash.ID, stored []byte) error
	// Has reports, for each id in order, whether the store holds it.
	Has(ctx context.Context, ids []hash.ID) ([]bool, error)
}

// Key returns the key of id in the fs and S3 layouts:
// objects/<first 2 hex>/<next 2 hex>/<full 64 hex> (03 §Object store key layout).
func Key(id hash.ID) string {
	h := id.String()
	return "objects/" + h[0:2] + "/" + h[2:4] + "/" + h
}

// Open returns the store described by rawURL:
//
//	mem://
//	fs://<path>          relative or absolute directory, e.g. fs://./data/objects
//	s3://<bucket>?endpoint=host:port&access_key=…&secret_key=…&secure=true|false&region=…
//
// For s3, endpoint defaults to s3.amazonaws.com and secure to true; without
// access_key the credentials come from the environment (AWS_* or MINIO_*).
func Open(rawURL string) (ObjectStore, error) {
	scheme, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		return nil, fmt.Errorf("store url %q: missing scheme (mem://, fs://, s3://)", redact(rawURL))
	}
	switch scheme {
	case "mem":
		if rest != "" {
			return nil, fmt.Errorf("store url %q: mem:// takes no path", rawURL)
		}
		return NewMem(), nil
	case "fs":
		if rest == "" {
			return nil, fmt.Errorf("store url %q: fs:// needs a directory", rawURL)
		}
		return NewFS(rest), nil
	case "s3":
		cfg, err := parseS3URL(rawURL)
		if err != nil {
			return nil, err
		}
		return NewS3(cfg)
	default:
		return nil, fmt.Errorf("store url %q: unknown scheme %q", redact(rawURL), scheme)
	}
}

// redact hides credentials in a store URL for error messages.
func redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable url>"
	}
	q := u.Query()
	if q.Has("secret_key") {
		q.Set("secret_key", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.Redacted()
}
