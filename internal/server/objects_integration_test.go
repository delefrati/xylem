//go:build integration

package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
)

// countingStore counts Get calls.
type countingStore struct {
	store.ObjectStore
	gets atomic.Int64
}

func (c *countingStore) Get(ctx context.Context, id hash.ID) ([]byte, error) {
	c.gets.Add(1)
	return c.ObjectStore.Get(ctx, id)
}

func collect(t *testing.T, stream xylemv1.ObjectService_GetObjectsClient) ([]*xylemv1.GetObjectsResponse, error) {
	t.Helper()
	var msgs []*xylemv1.GetObjectsResponse
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return msgs, nil
		}
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, m)
	}
}

func TestObjectService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	counting := &countingStore{ObjectStore: store.NewMem()}
	c := startServerFor(t, repotest.NewEnv(t, counting))

	// 200 small objects, one 3 MiB object, and 2 missing ids.
	var ids [][]byte
	want := map[hash.ID][]byte{}
	put := func(body []byte) hash.ID {
		id, stored := object.Encode(object.KindBlob, body)
		if err := counting.Put(ctx, id, stored); err != nil {
			t.Fatal(err)
		}
		want[id] = stored
		ids = append(ids, id[:])
		return id
	}
	// Random (incompressible) bodies, so stored frames are as large as the bodies.
	rng := rand.NewChaCha8([32]byte{7})
	for range 200 {
		body := make([]byte, 10_000)
		_, _ = rng.Read(body)
		put(body)
	}
	big := make([]byte, 3<<20)
	_, _ = rng.Read(big)
	bigID := put(big)
	missing := []hash.ID{{0xde}, {0xad}}
	ids = append(ids, missing[0][:], missing[1][:])

	stream, err := c.Object.GetObjects(ctx, &xylemv1.GetObjectsRequest{Ids: ids})
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := collect(t, stream)
	if err != nil {
		t.Fatal(err)
	}
	got := map[hash.ID][]byte{}
	var gotMissing int
	for i, m := range msgs {
		size := 0
		for _, o := range m.Objects {
			var id hash.ID
			copy(id[:], o.Id)
			got[id] = o.Data
			size += len(o.Data)
			if id == bigID && len(m.Objects) != 1 {
				t.Error("the 3 MiB object was not sent alone")
			}
		}
		if len(m.Objects) > 1 && size > 1<<20+20_000 {
			t.Errorf("message %d packs %d bytes, want about 1 MiB at most", i, size)
		}
		if len(m.Missing) > 0 && i != len(msgs)-1 {
			t.Error("missing ids reported before the final message")
		}
		gotMissing += len(m.Missing)
	}
	if len(got) != len(want) || gotMissing != 2 {
		t.Fatalf("got %d objects and %d missing, want %d and 2", len(got), gotMissing, len(want))
	}
	for id, data := range want {
		if err := object.Verify(id, got[id]); err != nil || string(got[id]) != string(data) {
			t.Fatalf("object %s differs: %v", id.Short(8), err)
		}
	}
	if len(msgs) < 3 {
		t.Errorf("200 × 10 kB objects came in %d messages; expected packing into several ~1 MiB messages", len(msgs))
	}

	// A second fetch of the small objects is served from the cache.
	before := counting.gets.Load()
	stream, _ = c.Object.GetObjects(ctx, &xylemv1.GetObjectsRequest{Ids: ids[:200]})
	if _, err := collect(t, stream); err != nil {
		t.Fatal(err)
	}
	if n := counting.gets.Load() - before; n != 0 {
		t.Errorf("cached objects caused %d store reads", n)
	}

	has, err := c.Object.HasObjects(ctx, &xylemv1.HasObjectsRequest{Ids: [][]byte{ids[0], missing[0][:], bigID[:], ids[0]}})
	if err != nil || len(has.Present) != 4 || !has.Present[0] || has.Present[1] || !has.Present[2] || !has.Present[3] {
		t.Errorf("HasObjects = %v, %v", has.GetPresent(), err)
	}

	// Limits and validation.
	tooMany := make([][]byte, 1025)
	for i := range tooMany {
		tooMany[i] = ids[0]
	}
	if _, err := c.Object.HasObjects(ctx, &xylemv1.HasObjectsRequest{Ids: tooMany}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("1025 ids: %v", err)
	}
	stream, _ = c.Object.GetObjects(ctx, &xylemv1.GetObjectsRequest{Ids: [][]byte{{1, 2, 3}}})
	if _, err := collect(t, stream); status.Code(err) != codes.InvalidArgument {
		t.Errorf("short id: %v", err)
	}
	stream, _ = c.Object.GetObjects(ctx, &xylemv1.GetObjectsRequest{Repo: "nope", Ids: ids[:1]})
	if _, err := collect(t, stream); status.Code(err) != codes.NotFound {
		t.Errorf("unknown repo: %v", err)
	}
	stream, _ = c.Object.GetObjects(ctx, &xylemv1.GetObjectsRequest{})
	if msgs, err := collect(t, stream); err != nil || len(msgs) != 0 {
		t.Errorf("empty request: %d messages, %v", len(msgs), err)
	}
}

func putObjects(t *testing.T, c *clients, msgs ...*xylemv1.PutObjectsRequest) (*xylemv1.PutObjectsResponse, error) {
	t.Helper()
	stream, err := c.Object.PutObjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if err := stream.Send(m); err != nil {
			break // the server failed the call; CloseAndRecv reports why
		}
	}
	return stream.CloseAndRecv()
}

func storedObject(kind object.Kind, body []byte) *xylemv1.StoredObject {
	id, data := object.Encode(kind, body)
	return &xylemv1.StoredObject{Id: id[:], Data: data}
}

func TestPutObjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := repotest.NewEnv(t, store.NewMem())
	c := startServerFor(t, env, func(cfg *server.Config) { cfg.RepoOptions.MaxBlobSize = 1 << 10 })

	a, b := storedObject(object.KindBlob, []byte("a\n")), storedObject(object.KindBlob, []byte("b\n"))
	resp, err := putObjects(t, c,
		&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{a, b}},
		&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{a}}, // repeated in a later message
	)
	if err != nil || resp.GetStored() != 2 || resp.GetAlreadyPresent() != 1 {
		t.Fatalf("put = %v, %v", resp, err)
	}
	var id hash.ID
	copy(id[:], a.Id)
	if got, err := env.Objects.Get(ctx, id); err != nil || !bytes.Equal(got, a.Data) {
		t.Fatalf("stored a: %v", err)
	}
	// Duplicates within one message count once as stored.
	cc := storedObject(object.KindBlob, []byte("c\n"))
	if resp, err := putObjects(t, c, &xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{cc, cc, a}}); err != nil ||
		resp.GetStored() != 1 || resp.GetAlreadyPresent() != 2 {
		t.Errorf("duplicates: %v, %v", resp, err)
	}
	if resp, err := putObjects(t, c); err != nil || resp.GetStored() != 0 {
		t.Errorf("empty stream: %v, %v", resp, err)
	}

	tree, err := object.EncodeTree(&object.Tree{Entries: []object.Entry{{Name: []byte("f"), Mode: object.ModeFile, ID: id, Size: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	wrongID := storedObject(object.KindBlob, []byte("x"))
	wrongID.Id = a.Id
	for name, tc := range map[string]struct {
		req  *xylemv1.PutObjectsRequest
		code codes.Code
	}{
		"tree":          {&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{storedObject(object.KindTree, tree)}}, codes.InvalidArgument},
		"hash mismatch": {&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{wrongID}}, codes.InvalidArgument},
		"not zstd":      {&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{{Id: a.Id, Data: []byte("blob 2\x00a\n")}}}, codes.InvalidArgument},
		"short id":      {&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{{Id: []byte{1}, Data: a.Data}}}, codes.InvalidArgument},
		"too large":     {&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{storedObject(object.KindBlob, make([]byte, 2<<10))}}, codes.InvalidArgument},
		"unknown repo":  {&xylemv1.PutObjectsRequest{Repo: "nope", Objects: []*xylemv1.StoredObject{a}}, codes.NotFound},
	} {
		if _, err := putObjects(t, c, tc.req); status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %v", name, err, tc.code)
		}
	}
	// The rejected tree was not stored.
	treeID := object.ID(object.KindTree, tree)
	if has, _ := env.Objects.Has(ctx, []hash.ID{treeID}); has[0] {
		t.Error("a rejected tree was stored")
	}
}
