//go:build integration

package cache_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/cache"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/server/servertest"
)

func dialAddr(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(cache.MaxMessageSize), grpc.MaxCallSendMsgSize(cache.MaxMessageSize)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// startTier serves a tier of kind over upstream on a loopback port.
func startTier(t *testing.T, upstream string, kind string) (string, *cache.Tier) {
	t.Helper()
	tier := cache.NewTier(cache.Config{Upstream: xylemv1.NewObjectServiceClient(dialAddr(t, upstream))})
	g := cache.NewService(tier, kind, nil).NewGRPCServer()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	return lis.Addr().String(), tier
}

func getAll(t *testing.T, c xylemv1.ObjectServiceClient, ids [][]byte) (map[string][]byte, [][]byte) {
	t.Helper()
	stream, err := c.GetObjects(context.Background(), &xylemv1.GetObjectsRequest{Ids: ids})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	var missing [][]byte
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return got, missing
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range m.GetObjects() {
			got[string(o.GetId())] = o.GetData()
		}
		missing = append(missing, m.GetMissing()...)
	}
}

func TestCacheTierInFrontOfServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servertest.Start(t, "tiny")
	addr, _ := startTier(t, s.Addr, cache.KindCache)
	cc := dialAddr(t, addr)
	objs := xylemv1.NewObjectServiceClient(cc)

	// The kinds: the server says server, the cache says cache.
	if kind, err := cache.UpstreamKind(ctx, xylemv1.NewObjectServiceClient(dialAddr(t, s.Addr))); err != nil || kind != cache.KindServer {
		t.Errorf("server kind %q, %v", kind, err)
	}
	if err := cache.CheckUpstream(ctx, objs, cache.KindServer); err == nil || !strings.Contains(err.Error(), "upstream is a cache, want a server") {
		t.Errorf("a cache as a cache's upstream: %v", err)
	}

	// The root tree of main plus one missing id, twice: the second time the
	// server's store is not read.
	root, err := xylemv1.NewRepoServiceClient(dialAddr(t, s.Addr)).ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	absent, _ := object.Encode(object.KindBlob, []byte("absent\n"))
	ids := [][]byte{root.GetRootTree(), absent[:]}
	s.Store.Reset()
	got, missing := getAll(t, objs, ids)
	var rootID hash.ID
	copy(rootID[:], root.GetRootTree())
	if len(got) != 1 || object.Verify(rootID, got[string(rootID[:])]) != nil || len(missing) != 1 {
		t.Fatalf("first fetch: %d objects, %d missing", len(got), len(missing))
	}
	s.Store.Reset()
	if got, _ := getAll(t, objs, ids[:1]); len(got) != 1 || len(s.Store.Reads()) != 0 {
		t.Errorf("warm fetch read the server's store %d times", len(s.Store.Reads()))
	}
	if has, err := objs.HasObjects(ctx, &xylemv1.HasObjectsRequest{Ids: ids}); err != nil || !has.GetPresent()[0] || has.GetPresent()[1] {
		t.Errorf("HasObjects = %v, %v", has, err)
	}

	// Writes go through to the server.
	newID, data := object.Encode(object.KindBlob, []byte("uploaded through the cache\n"))
	put, err := objs.PutObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := put.Send(&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{{Id: newID[:], Data: data}}}); err != nil {
		t.Fatal(err)
	}
	if resp, err := put.CloseAndRecv(); err != nil || resp.GetStored() != 1 {
		t.Fatalf("PutObjects = %v, %v", resp, err)
	}
	if has, err := s.Store.Has(ctx, []hash.ID{newID}); err != nil || !has[0] {
		t.Error("the upload did not reach the server's store")
	}
	// A tree upload is rejected by the server and the error passes through.
	put, _ = objs.PutObjects(ctx)
	treeID, tree := object.Encode(object.KindTree, []byte{0})
	_ = put.Send(&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{{Id: treeID[:], Data: tree}}})
	if _, err := put.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("tree upload through the cache: %v", err)
	}

	// Metadata RPCs are not served by a cache.
	_, err = xylemv1.NewRepoServiceClient(cc).ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: "main"})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("RepoService on a cache: %v", err)
	}
	// An unknown repository is reported by the server.
	stream, _ := objs.GetObjects(ctx, &xylemv1.GetObjectsRequest{Repo: "nope", Ids: [][]byte{absent[:]}})
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("unknown repo through the cache: %v", err)
	}
}
