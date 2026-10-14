package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"golang.org/x/sync/errgroup"

	"github.com/delefrati/xylem/internal/hash"
)

// S3Config configures an S3-compatible store (S3, MinIO, R2, ...).
type S3Config struct {
	Bucket    string
	Endpoint  string // host[:port]; default s3.amazonaws.com
	AccessKey string // empty: credentials from the environment
	SecretKey string
	Secure    bool // use HTTPS
	Region    string
	// HasConcurrency bounds concurrent StatObject calls in Has (default 32).
	HasConcurrency int
}

// S3 stores objects in a bucket under the key layout of Key.
type S3 struct {
	client         *minio.Client
	bucket         string
	hasConcurrency int
}

// NewS3 returns an S3 store. It does no I/O.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 store: bucket is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "s3.amazonaws.com"
	}
	if cfg.HasConcurrency <= 0 {
		cfg.HasConcurrency = 32
	}
	creds := credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	if cfg.AccessKey == "" {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{}, &credentials.EnvMinio{},
		})
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  creds,
		Secure: cfg.Secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 store: %w", err)
	}
	return &S3{client: client, bucket: cfg.Bucket, hasConcurrency: cfg.HasConcurrency}, nil
}

// parseS3URL parses s3://bucket?endpoint=…&access_key=…&secret_key=…&secure=…&region=….
func parseS3URL(rawURL string) (S3Config, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return S3Config{}, fmt.Errorf("store url %s: invalid", redact(rawURL))
	}
	if u.Host == "" || (u.Path != "" && u.Path != "/") {
		return S3Config{}, fmt.Errorf("store url %s: want s3://<bucket>?…", redact(rawURL))
	}
	q := u.Query()
	cfg := S3Config{
		Bucket:    u.Host,
		Endpoint:  q.Get("endpoint"),
		AccessKey: q.Get("access_key"),
		SecretKey: q.Get("secret_key"),
		Secure:    true,
		Region:    q.Get("region"),
	}
	for k := range q {
		switch k {
		case "endpoint", "access_key", "secret_key", "secure", "region":
		default:
			return S3Config{}, fmt.Errorf("store url %s: unknown parameter %q", redact(rawURL), k)
		}
	}
	if s := q.Get("secure"); s != "" {
		if cfg.Secure, err = strconv.ParseBool(s); err != nil {
			return S3Config{}, fmt.Errorf("store url %s: secure=%q is not a boolean", redact(rawURL), s)
		}
	}
	return cfg, nil
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == minio.NoSuchKey || resp.StatusCode == http.StatusNotFound
}

// Get implements ObjectStore.
func (s *S3) Get(ctx context.Context, id hash.ID) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, Key(id), minio.GetObjectOptions{})
	if err != nil {
		return nil, s.wrap("get", id, err)
	}
	defer func() { _ = obj.Close() }()
	b, err := io.ReadAll(obj)
	if err != nil {
		return nil, s.wrap("get", id, err)
	}
	return b, nil
}

// Put implements ObjectStore. Objects are immutable, so overwriting an existing
// key with the same bytes is harmless and cheaper than checking first.
func (s *S3) Put(ctx context.Context, id hash.ID, stored []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, Key(id), bytes.NewReader(stored), int64(len(stored)),
		minio.PutObjectOptions{ContentType: "application/zstd"})
	if err != nil {
		return s.wrap("put", id, err)
	}
	return nil
}

// Has implements ObjectStore with up to HasConcurrency concurrent StatObject calls.
func (s *S3) Has(ctx context.Context, ids []hash.ID) ([]bool, error) {
	present := make([]bool, len(ids))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(s.hasConcurrency)
	for i, id := range ids {
		g.Go(func() error {
			_, err := s.client.StatObject(ctx, s.bucket, Key(id), minio.StatObjectOptions{})
			switch {
			case err == nil:
				present[i] = true
			case !isNotFound(err):
				return s.wrap("stat", id, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return present, nil
}

func (s *S3) wrap(op string, id hash.ID, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("s3 store: %s %s: %w", op, id, ErrNotFound)
	}
	return fmt.Errorf("s3 store: %s %s: %w", op, id, err)
}
