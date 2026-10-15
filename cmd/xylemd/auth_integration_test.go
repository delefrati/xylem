//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/server/servertest"
)

const (
	readerToken = "reader-token-0123456789"
	writerToken = "writer-token-0123456789"
	writerName  = "Walt Writer <walt@example.com>"
	tokensTOML  = `
[[token]]
identity = "Rita Reader <rita@example.com>"
roles = ["read"]
token = "` + readerToken + `"

[[token]]
identity = "` + writerName + `"
roles = ["read", "write"]
token = "` + writerToken + `"
`
)

// TestAuthThroughTiers: the server, cache and daemon enforce tokens; tiers
// fetch with their own (read-only) token and forward a client's token, so
// writes act as the client.
func TestAuthThroughTiers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens, err := auth.ParseTokens([]byte(tokensTOML))
	if err != nil {
		t.Fatal(err)
	}
	tokensFile := filepath.Join(t.TempDir(), "tokens.toml")
	if err := os.WriteFile(tokensFile, []byte(tokensTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	s := servertest.Start(t, "tiny", func(cfg *server.Config) { cfg.Auth = tokens })

	// A tier without a token is refused at startup.
	if _, err := run(t, "cache", "--upstream", s.Addr, "--listen", freeAddr(t), "--http-listen", freeAddr(t)); err == nil ||
		!strings.Contains(err.Error(), "upstream refused the call") {
		t.Errorf("cache without a token: %v", err)
	}
	if _, err := run(t, "server", "--auth-tokens", filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Error("server with a missing tokens file started")
	}

	cacheAddr, cacheHTTP := freeAddr(t), freeAddr(t)
	startCmd(t, cacheHTTP, "cache", "--upstream", s.Addr, "--token", readerToken, "--auth-tokens", tokensFile,
		"--listen", cacheAddr, "--http-listen", cacheHTTP)
	sock := filepath.Join(t.TempDir(), "d.sock")
	daemonHTTP := freeAddr(t)
	startCmd(t, daemonHTTP, "daemon", "--upstream", cacheAddr, "--server", s.Addr, "--token", readerToken,
		"--socket", sock, "--disk-dir", t.TempDir(), "--http-listen", daemonHTTP)
	daemon := "unix://" + sock

	dial := func(addr, token string) *client.Conn {
		conn, err := client.Dial(addr, token)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	blob := func(content string) *xylemv1.StoredObject {
		id, data := object.Encode(object.KindBlob, []byte(content))
		return &xylemv1.StoredObject{Id: id[:], Data: data}
	}
	put := func(conn *client.Conn, o *xylemv1.StoredObject) error {
		stream, err := conn.Object.PutObjects(ctx)
		if err != nil {
			return err
		}
		_ = stream.Send(&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{o}})
		_, err = stream.CloseAndRecv()
		return err
	}

	// The cache checks its clients.
	if _, err := dial(cacheAddr, "").Object.HasObjects(ctx, &xylemv1.HasObjectsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("cache, no token: %v", err)
	}
	if err := put(dial(cacheAddr, readerToken), blob("reader\n")); status.Code(err) != codes.PermissionDenied {
		t.Errorf("cache, reader puts: %v", err)
	}
	// The cache's own token is read-only, so this put succeeding means the
	// writer's token was forwarded to the server.
	if err := put(dial(cacheAddr, writerToken), blob("writer via cache\n")); err != nil {
		t.Errorf("cache, writer puts: %v", err)
	}

	// The daemon authenticates no one (its socket directory is private); calls
	// without a token carry the daemon's, a client's token is forwarded.
	anon := dial(daemon, "")
	tip, err := anon.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Revision: "main"})
	if err != nil {
		t.Fatalf("daemon, read with the daemon's token: %v", err)
	}
	stream, err := anon.Object.GetObjects(ctx, &xylemv1.GetObjectsRequest{Ids: [][]byte{tip.GetRootTree()}})
	if err == nil {
		_, err = stream.Recv()
	}
	if err != nil {
		t.Errorf("daemon, fetch: %v", err)
	}
	o := blob("written through the daemon\n")
	if err := put(anon, o); status.Code(err) != codes.PermissionDenied {
		t.Errorf("daemon, put with the daemon's read token: %v", err)
	}
	writer := dial(daemon, writerToken)
	if err := put(writer, o); err != nil {
		t.Fatalf("daemon, writer puts: %v", err)
	}
	req := &xylemv1.CreateChangesetRequest{
		Parents: [][]byte{tip.GetId()},
		Author:  &xylemv1.Signature{NameEmail: []byte("Ada <ada@example.com>"), Time: timestamppb.New(timestamppb.Now().AsTime().Truncate(1e9))},
		Message: []byte("via daemon\n"),
		Changes: []*xylemv1.FileChange{{Path: []byte("new.txt"), Op: xylemv1.FileChange_OP_PUT, Mode: xylemv1.FileMode_FILE_MODE_FILE, Size: 27, BlobId: o.Id}},
	}
	if _, err := anon.Write.CreateChangeset(ctx, req); status.Code(err) != codes.PermissionDenied {
		t.Errorf("daemon, create with the daemon's read token: %v", err)
	}
	resp, err := writer.Write.CreateChangeset(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.GetChangeset().GetCommitter().GetNameEmail()); got != writerName {
		t.Errorf("committer through the daemon = %q, want %q", got, writerName)
	}
}
