//go:build integration

package server_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/repo/repotest"
	"github.com/delefrati/xylem/internal/server"
	"github.com/delefrati/xylem/internal/store"
)

const (
	readerToken = "reader-token-0123456789"
	writerToken = "writer-token-0123456789"
	writerName  = "Walt Writer <walt@example.com>"
)

func testTokens(t *testing.T) *auth.Tokens {
	t.Helper()
	toks, err := auth.ParseTokens([]byte(`
[[token]]
identity = "Rita Reader <rita@example.com>"
roles = ["read"]
token = "` + readerToken + `"

[[token]]
identity = "` + writerName + `"
roles = ["read", "write"]
token = "` + writerToken + `"
`))
	if err != nil {
		t.Fatal(err)
	}
	return toks
}

// rawCodec sends and receives undecoded message bytes, so one helper can
// call every RPC with an empty request.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }

func (rawCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }

func (rawCodec) Unmarshal(data []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), data...)
	return nil
}

// callEmpty calls method with an empty request message and drains the
// response; it works for unary and streaming RPCs alike.
func callEmpty(ctx context.Context, cc *grpc.ClientConn, method string) error {
	s, err := cc.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, method, grpc.ForceCodec(rawCodec{}))
	if err != nil {
		return err
	}
	empty := []byte{}
	if err := s.SendMsg(&empty); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := s.CloseSend(); err != nil {
		return err
	}
	for {
		var out []byte
		if err := s.RecvMsg(&out); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// methods lists every RPC of the three services.
func methods() []string {
	var out []string
	for _, sd := range []grpc.ServiceDesc{xylemv1.RepoService_ServiceDesc, xylemv1.ObjectService_ServiceDesc, xylemv1.WriteService_ServiceDesc} {
		for _, m := range sd.Methods {
			out = append(out, "/"+sd.ServiceName+"/"+m.MethodName)
		}
		for _, s := range sd.Streams {
			out = append(out, "/"+sd.ServiceName+"/"+s.StreamName)
		}
	}
	return out
}

func bearer(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), auth.Header, "Bearer "+token)
}

func TestAuthEveryRPC(t *testing.T) {
	t.Parallel()
	env := repotest.NewEnv(t, store.NewMem())
	g := server.New(server.Config{Meta: env.Meta, Objects: env.Objects, Auth: testTokens(t)}).NewGRPCServer()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	all := methods()
	if len(all) != 17 {
		t.Fatalf("%d RPCs; update this test's expectations", len(all))
	}
	authCode := func(err error) bool {
		c := status.Code(err)
		return c == codes.Unauthenticated || c == codes.PermissionDenied
	}
	for _, m := range all {
		role, _ := auth.RoleFor(m)
		for _, tc := range []struct {
			who  string
			ctx  context.Context
			want codes.Code // an auth code, or OK for "passes auth"
		}{
			{"no token", context.Background(), codes.Unauthenticated},
			{"bad token", bearer("not-a-token-0123456789"), codes.Unauthenticated},
			{"reader", bearer(readerToken), map[auth.Role]codes.Code{auth.RoleRead: codes.OK, auth.RoleWrite: codes.PermissionDenied}[role]},
			{"writer", bearer(writerToken), codes.OK},
		} {
			err := callEmpty(tc.ctx, cc, m)
			switch {
			case tc.want == codes.OK && authCode(err):
				t.Errorf("%s as %s: rejected: %v", m, tc.who, err)
			case tc.want != codes.OK && status.Code(err) != tc.want:
				t.Errorf("%s as %s: %v, want %v", m, tc.who, err, tc.want)
			}
		}
	}
}

func TestAuthIdentity(t *testing.T) {
	t.Parallel()
	env := repotest.NewEnv(t, store.NewMem())
	c := startServerFor(t, env, func(cfg *server.Config) { cfg.Auth = testTokens(t) })
	ctx := bearer(writerToken)
	put := func(path, content string) *xylemv1.FileChange {
		o := storedObject(1, []byte(content)) // object.KindBlob
		stream, err := c.Object.PutObjects(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&xylemv1.PutObjectsRequest{Objects: []*xylemv1.StoredObject{o}}); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.CloseAndRecv(); err != nil {
			t.Fatal(err)
		}
		return &xylemv1.FileChange{Path: []byte(path), Op: xylemv1.FileChange_OP_PUT, Mode: xylemv1.FileMode_FILE_MODE_FILE, Size: uint64(len(content)), BlobId: o.Id}
	}
	create := func(parent []byte, changes ...*xylemv1.FileChange) *xylemv1.Changeset {
		req := &xylemv1.CreateChangesetRequest{Author: author, Message: []byte("m"), Changes: changes}
		if parent != nil {
			req.Parents = [][]byte{parent}
		}
		resp, err := c.Write.CreateChangeset(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetChangeset()
	}

	// CreateChangeset: the author is kept, the committer is the caller.
	root := create(nil, put("a", "a\n"))
	if string(root.GetAuthor().GetNameEmail()) != "Ada <ada@example.com>" || string(root.GetCommitter().GetNameEmail()) != writerName {
		t.Errorf("author %q committer %q", root.GetAuthor().GetNameEmail(), root.GetCommitter().GetNameEmail())
	}
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "main", NewId: root.GetId()}); err != nil {
		t.Fatal(err)
	}
	m1 := create(root.GetId(), put("m", "m\n"))
	if _, err := c.Repo.UpdateRef(ctx, &xylemv1.UpdateRefRequest{Name: "main", ExpectedId: root.GetId(), NewId: m1.GetId()}); err != nil {
		t.Fatal(err)
	}
	// Land rebases x onto m1: the rebased changeset's committer is the caller.
	x := create(root.GetId(), put("x", "x\n"))
	land, err := c.Write.Land(ctx, &xylemv1.LandRequest{Stack: [][]byte{x.GetId()}})
	if err != nil || len(land.GetLanded()) != 1 {
		t.Fatalf("land = %v, %v", land, err)
	}
	got, err := c.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Ids: land.GetLanded()})
	if err != nil || string(got.GetChangesets()[0].GetCommitter().GetNameEmail()) != writerName {
		t.Errorf("rebased committer %v, %v", got, err)
	}
	// The ref log names the caller for both the admin update and the land.
	rows, err := env.Pool.Query(context.Background(), `SELECT reason, actor FROM ref_log WHERE name = 'main' ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var log []string
	for rows.Next() {
		var reason, actor string
		if err := rows.Scan(&reason, &actor); err != nil {
			t.Fatal(err)
		}
		log = append(log, reason+" by "+actor)
	}
	want := fmt.Sprint([]string{"admin by " + writerName, "admin by " + writerName, "land by " + writerName})
	if fmt.Sprint(log) != want {
		t.Errorf("ref log %v, want %v", log, want)
	}
}
