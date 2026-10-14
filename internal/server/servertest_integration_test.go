//go:build integration

package server_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
)

// clients are connected to a test server.
type clients struct {
	Repo   xylemv1.RepoServiceClient
	Object xylemv1.ObjectServiceClient
	Write  xylemv1.WriteServiceClient
	Env    *repotest.Env
}

// startServer serves a fresh repotest environment on a loopback port.
func startServer(t *testing.T) *clients {
	t.Helper()
	env := repotest.NewEnv(t, store.NewMem())
	return startServerFor(t, env)
}

func startServerFor(t *testing.T, env *repotest.Env) *clients {
	t.Helper()
	srv := server.New(server.Config{Meta: env.Meta, Objects: env.Objects, RepoOptions: repo.Options{Now: repotest.Clock}, ObjectCacheBytes: 64 << 20})
	g := srv.NewGRPCServer()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(server.MaxMessageSize), grpc.MaxCallSendMsgSize(server.MaxMessageSize)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &clients{
		Repo: xylemv1.NewRepoServiceClient(conn), Object: xylemv1.NewObjectServiceClient(conn),
		Write: xylemv1.NewWriteServiceClient(conn), Env: env,
	}
}

func TestRepoResolutionAndHeaders(t *testing.T) {
	t.Parallel()
	c := startServer(t)
	ctx := context.Background()
	// Unknown repo → NotFound; the request id comes back in a header.
	var header metadata.MD
	_, err := c.Repo.ListRefs(ctx, &xylemv1.ListRefsRequest{Repo: "nope"}, grpc.Header(&header))
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown repo: %v", err)
	}
	if len(header.Get(server.RequestIDHeader)) != 1 {
		t.Errorf("no request id header: %v", header)
	}
	// An empty repo name means "default"; refs come back by name.
	r := c.Env.Repo
	root := repotest.Commit(t, r, nil, "root", repotest.Put(t, r, "f", "1"))
	for _, name := range []string{"main", "release/2", "release/1"} {
		if _, err := r.UpdateRef(ctx, name, hash.ID{}, root.ID, false, "t", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := c.Repo.ListRefs(ctx, &xylemv1.ListRefsRequest{Prefix: "release/"})
	if err != nil || len(resp.Refs) != 2 || resp.Refs[0].Name != "release/1" || string(resp.Refs[0].Id) != string(root.ID[:]) || resp.Refs[0].Version != 1 {
		t.Errorf("ListRefs = %v, %v", resp, err)
	}
	if all, _ := c.Repo.ListRefs(ctx, &xylemv1.ListRefsRequest{Repo: "default"}); len(all.Refs) != 3 {
		t.Errorf("ListRefs(all) = %d refs", len(all.Refs))
	}
}
