package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/store/storetest"
)

// upstream is a fake ObjectService backed by a memory store.
type upstream struct {
	xylemv1.UnimplementedObjectServiceServer
	objs    *store.Mem
	calls   atomic.Int64  // GetObjects calls
	ids     atomic.Int64  // ids asked for
	gate    chan struct{} // if set, GetObjects waits for it
	corrupt atomic.Bool
}

func (u *upstream) GetObjects(req *xylemv1.GetObjectsRequest, stream xylemv1.ObjectService_GetObjectsServer) error {
	u.calls.Add(1)
	u.ids.Add(int64(len(req.GetIds())))
	if u.gate != nil {
		select {
		case <-u.gate:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
	resp := &xylemv1.GetObjectsResponse{}
	for _, raw := range req.GetIds() {
		var id hash.ID
		copy(id[:], raw)
		data, err := u.objs.Get(stream.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			resp.Missing = append(resp.Missing, raw)
			continue
		}
		if err != nil {
			return err
		}
		if u.corrupt.Load() {
			_, data = object.Encode(object.KindBlob, []byte("not what you asked for"))
		}
		resp.Objects = append(resp.Objects, &xylemv1.StoredObject{Id: raw, Data: data})
	}
	return stream.Send(resp)
}

func (u *upstream) HasObjects(ctx context.Context, req *xylemv1.HasObjectsRequest) (*xylemv1.HasObjectsResponse, error) {
	ids := make([]hash.ID, len(req.GetIds()))
	for i, raw := range req.GetIds() {
		copy(ids[i][:], raw)
	}
	present, err := u.objs.Has(ctx, ids)
	return &xylemv1.HasObjectsResponse{Present: present}, err
}

func (u *upstream) PutObjects(stream xylemv1.ObjectService_PutObjectsServer) error {
	resp := &xylemv1.PutObjectsResponse{}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return stream.SendAndClose(resp)
		}
		for _, o := range msg.GetObjects() {
			var id hash.ID
			copy(id[:], o.GetId())
			if err := u.objs.Put(stream.Context(), id, o.GetData()); err != nil {
				return err
			}
			resp.Stored++
		}
	}
}

// startUpstream serves u over an in-memory connection.
func startUpstream(t *testing.T, u *upstream) (xylemv1.ObjectServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer(grpc.MaxRecvMsgSize(80<<20), grpc.MaxSendMsgSize(80<<20))
	xylemv1.RegisterObjectServiceServer(g, u)
	go func() { _ = g.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(80<<20), grpc.MaxCallSendMsgSize(80<<20)))
	if err != nil {
		t.Fatal(err)
	}
	stop := func() { _ = conn.Close(); g.Stop() }
	t.Cleanup(stop)
	return xylemv1.NewObjectServiceClient(conn), g.Stop
}

func newTier(t *testing.T, cfg Config) (*Tier, *upstream) {
	t.Helper()
	u := &upstream{objs: store.NewMem()}
	cfg.Upstream, _ = startUpstream(t, u)
	return NewTier(cfg), u
}

func putUpstream(t *testing.T, u *upstream, body string) hash.ID {
	t.Helper()
	id, data := object.Encode(object.KindBlob, []byte(body))
	if err := u.objs.Put(context.Background(), id, data); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTierStoreContract(t *testing.T) {
	t.Parallel()
	t.Run("memory", func(t *testing.T) {
		t.Parallel()
		tier, _ := newTier(t, Config{})
		storetest.Run(t, func(*testing.T) store.ObjectStore { return tier.Store("default") })
	})
	t.Run("memory and disk", func(t *testing.T) {
		t.Parallel()
		disk, err := OpenDisk(t.TempDir(), 256<<20)
		if err != nil {
			t.Fatal(err)
		}
		tier, _ := newTier(t, Config{MemoryBytes: -1, Disk: disk})
		storetest.Run(t, func(*testing.T) store.ObjectStore { return tier.Store("default") })
	})
}

func TestTierCollapsesAndBatchesMisses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tier, u := newTier(t, Config{BatchWindow: 20 * time.Millisecond})
	same := putUpstream(t, u, "shared\n")
	var distinct []hash.ID
	for i := range 200 {
		distinct = append(distinct, putUpstream(t, u, fmt.Sprintf("object %d\n", i)))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for i := range 200 {
		wg.Add(2)
		go func() { // everyone wants the same object...
			defer wg.Done()
			got, err := tier.Get(ctx, "default", []hash.ID{same})
			if err == nil && got[same] == nil {
				err = errors.New("shared object missing")
			}
			errs <- err
		}()
		go func() { // ...and one object of its own
			defer wg.Done()
			got, err := tier.Get(ctx, "default", []hash.ID{distinct[i]})
			if err == nil && got[distinct[i]] == nil {
				err = errors.New("object missing")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if ids := u.ids.Load(); ids != 201 {
		t.Errorf("upstream asked for %d ids, want 201 (each object once)", ids)
	}
	if calls := u.calls.Load(); calls > 20 {
		t.Errorf("%d upstream calls for 201 concurrent misses, want them batched", calls)
	}
	before := u.calls.Load()
	if got, err := tier.Get(ctx, "default", distinct); err != nil || len(got) != len(distinct) || u.calls.Load() != before {
		t.Errorf("warm Get: %d objects, %v, %d new upstream calls", len(got), err, u.calls.Load()-before)
	}
}

func TestTierMissingAndCorrupt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tier, u := newTier(t, Config{})
	id := putUpstream(t, u, "real\n")
	absent, _ := object.Encode(object.KindBlob, []byte("nowhere\n"))
	got, err := tier.Get(ctx, "default", []hash.ID{id, absent})
	if err != nil || len(got) != 1 || got[id] == nil {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if has, err := tier.Has(ctx, "default", []hash.ID{id, absent}); err != nil || !has[0] || has[1] {
		t.Errorf("Has = %v, %v", has, err)
	}

	bad := putUpstream(t, u, "will be corrupted\n")
	u.corrupt.Store(true)
	if _, err := tier.Get(ctx, "default", []hash.ID{bad}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("corrupt upstream object: %v", err)
	}
	if tier.Memory().Has(bad) {
		t.Error("a corrupt object was cached")
	}
	u.corrupt.Store(false)
	if got, err := tier.Get(ctx, "default", []hash.ID{bad}); err != nil || got[bad] == nil {
		t.Errorf("refetch after corruption: %v", err)
	}
	// Uploads are verified before anything is sent.
	if _, err := tier.Put(ctx, "default", []*xylemv1.StoredObject{{Id: id[:], Data: []byte("junk")}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid upload: %v", err)
	}
}

func TestTierUpstreamDown(t *testing.T) {
	t.Parallel()
	u := &upstream{objs: store.NewMem()}
	client, stop := startUpstream(t, u)
	tier := NewTier(Config{Upstream: client})
	id := putUpstream(t, u, "x\n")
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := tier.Get(ctx, "default", []hash.ID{id}); status.Code(err) != codes.Unavailable {
		t.Errorf("Get with upstream down: %v, want Unavailable", err)
	}
}

func TestTierOverload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	u := &upstream{objs: store.NewMem(), gate: make(chan struct{})}
	client, _ := startUpstream(t, u)
	tier := NewTier(Config{Upstream: client, MaxBatch: 1, MaxInflight: 1, MaxQueue: 2})
	var ids []hash.ID
	for i := range 10 {
		ids = append(ids, putUpstream(t, u, fmt.Sprintf("o%d\n", i)))
	}
	errs := make(chan error, len(ids))
	for _, id := range ids {
		go func() {
			_, err := tier.Get(ctx, "default", []hash.ID{id})
			errs <- err
		}()
	}
	overloaded := 0
	deadline := time.After(10 * time.Second)
	for overloaded < len(ids)-3 { // 1 in flight + 2 queued may wait; the rest fail fast
		select {
		case err := <-errs:
			if !errors.Is(err, ErrOverloaded) {
				t.Fatalf("got %v before the gate opened, want ErrOverloaded", err)
			}
			overloaded++
		case <-deadline:
			t.Fatalf("only %d calls failed fast", overloaded)
		}
	}
	close(u.gate)
	for range 3 {
		if err := <-errs; err != nil {
			t.Errorf("queued call: %v", err)
		}
	}
}

func TestDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d, err := OpenDisk(dir, 4000)
	if err != nil {
		t.Fatal(err)
	}
	var ids []hash.ID
	for i := range 6 {
		id, data := object.Encode(object.KindBlob, bytes.Repeat([]byte{byte('a' + i)}, 2000))
		if len(data) > 1000 {
			t.Fatalf("frame of %d bytes; the test expects compressible bodies", len(data))
		}
		if err := d.Put(id, data); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		time.Sleep(5 * time.Millisecond) // distinct mtimes for the reopen order
	}
	if _, ok := d.Get(ids[0]); !ok {
		t.Fatal("first object missing")
	}
	objects, size := d.Stats()
	reopened, err := OpenDisk(dir, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if o, s := reopened.Stats(); o != objects || s != size {
		t.Errorf("reopened index %d objects / %d bytes, want %d / %d", o, s, objects, size)
	}
	for _, id := range ids {
		data, ok := reopened.Get(id)
		if !ok || object.Verify(id, data) != nil {
			t.Errorf("object %s after reopen: %v", id.Short(8), ok)
		}
	}
	// A temp file left by a crash is removed at open.
	leftover := filepath.Join(dir, "objects", "aa", "bb", tmpPrefix+"123")
	if err := os.MkdirAll(filepath.Dir(leftover), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftover, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDisk(dir, 4000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("leftover temp file: %v", err)
	}
	// Shrinking the cap evicts the oldest files.
	small, err := OpenDisk(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if o, s := small.Stats(); s > 100 || o >= objects {
		t.Errorf("after shrinking: %d objects, %d bytes", o, s)
	}
	if _, ok := small.Get(ids[0]); ok {
		t.Error("the oldest object survived eviction")
	}
}
