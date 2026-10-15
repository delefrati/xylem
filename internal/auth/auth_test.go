package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
)

const (
	readerToken = "reader-token-0123456789"
	writerToken = "writer-token-0123456789"
)

func testTokens(t *testing.T) *Tokens {
	t.Helper()
	sum := sha256.Sum256([]byte(writerToken))
	toks, err := ParseTokens([]byte(`
[[token]]
identity = "Rita Reader <rita@example.com>"
roles = ["read"]
token = "` + readerToken + `"

[[token]]
identity = "Walt Writer <walt@example.com>"
roles = ["read", "write"]
sha256 = "` + hex.EncodeToString(sum[:]) + `"
`))
	if err != nil {
		t.Fatal(err)
	}
	return toks
}

func TestParseTokens(t *testing.T) {
	t.Parallel()
	toks := testTokens(t)
	for token, want := range map[string]*Identity{
		readerToken:                  {Name: "Rita Reader <rita@example.com>", Roles: []Role{RoleRead}},
		writerToken:                  {Name: "Walt Writer <walt@example.com>", Roles: []Role{RoleRead, RoleWrite}},
		"":                           nil,
		"reader-token-012345":        nil,
		strings.ToUpper(readerToken): nil,
	} {
		got, ok := toks.Lookup(token)
		if want == nil {
			if ok {
				t.Errorf("Lookup(%q) = %v, want no match", token, got)
			}
			continue
		}
		if diff := cmp.Diff(*want, got); !ok || diff != "" {
			t.Errorf("Lookup(%q) ok=%v (-want +got):\n%s", token, ok, diff)
		}
	}

	const good = `identity = "A <a@x>"` + "\n" + `roles = ["read"]` + "\n"
	for name, file := range map[string]string{
		"empty":           "",
		"bad toml":        "[[token]\n",
		"unknown key":     "[[token]]\n" + good + `token = "0123456789abcdef"` + "\nadmin = true\n",
		"no identity":     "[[token]]\nroles = [\"read\"]\ntoken = \"0123456789abcdef\"\n",
		"bad identity":    "[[token]]\nidentity = \"just a name\"\nroles = [\"read\"]\ntoken = \"0123456789abcdef\"\n",
		"no token":        "[[token]]\n" + good,
		"short token":     "[[token]]\n" + good + `token = "short"` + "\n",
		"both":            "[[token]]\n" + good + `token = "0123456789abcdef"` + "\n" + `sha256 = "` + strings.Repeat("ab", 32) + `"` + "\n",
		"bad sha256":      "[[token]]\n" + good + `sha256 = "abc"` + "\n",
		"no roles":        "[[token]]\nidentity = \"A <a@x>\"\nroles = []\ntoken = \"0123456789abcdef\"\n",
		"unknown role":    "[[token]]\nidentity = \"A <a@x>\"\nroles = [\"admin\"]\ntoken = \"0123456789abcdef\"\n",
		"duplicate token": "[[token]]\n" + good + `token = "0123456789abcdef"` + "\n[[token]]\nidentity = \"B <b@x>\"\nroles = [\"read\"]\ntoken = \"0123456789abcdef\"\n",
	} {
		if _, err := ParseTokens([]byte(file)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	path := filepath.Join(t.TempDir(), "tokens.toml")
	if _, err := LoadTokens(path); err == nil {
		t.Error("a missing file was accepted")
	}
	if err := os.WriteFile(path, []byte("[[token]]\n"+good+`token = "short"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokens(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
}

func TestIdentityHas(t *testing.T) {
	t.Parallel()
	reader, writer := Identity{Roles: []Role{RoleRead}}, Identity{Roles: []Role{RoleWrite}}
	if !reader.Has(RoleRead) || reader.Has(RoleWrite) || !writer.Has(RoleRead) || !writer.Has(RoleWrite) {
		t.Error("write must imply read, and read must not imply write")
	}
	if (Identity{}).Has(RoleRead) {
		t.Error("no roles has read")
	}
}

// Every RPC of every service has a role, so none is denied by omission;
// unknown methods are denied.
func TestEveryMethodHasARole(t *testing.T) {
	t.Parallel()
	writes := map[string]bool{
		"/xylem.v1.RepoService/UpdateRef": true, "/xylem.v1.ObjectService/PutObjects": true,
		"/xylem.v1.WriteService/CreateChangeset": true, "/xylem.v1.WriteService/Rebase": true, "/xylem.v1.WriteService/Land": true,
	}
	n := 0
	for _, sd := range []grpc.ServiceDesc{xylemv1.RepoService_ServiceDesc, xylemv1.ObjectService_ServiceDesc, xylemv1.WriteService_ServiceDesc} {
		var names []string
		for _, m := range sd.Methods {
			names = append(names, m.MethodName)
		}
		for _, s := range sd.Streams {
			names = append(names, s.StreamName)
		}
		for _, name := range names {
			method := "/" + sd.ServiceName + "/" + name
			want := RoleRead
			if writes[method] {
				want = RoleWrite
			}
			if got, ok := RoleFor(method); !ok || got != want {
				t.Errorf("%s: role %q (%v), want %q", method, got, ok, want)
			}
			n++
		}
	}
	if n != len(methodRoles) {
		t.Errorf("%d RPCs in the services, %d in the role table", n, len(methodRoles))
	}
	if _, ok := RoleFor("/xylem.v1.RepoService/Nope"); ok {
		t.Error("an unknown method has a role")
	}
	if r, ok := RoleFor("/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"); !ok || r != RoleRead {
		t.Error("reflection needs read")
	}
}

func withBearer(v ...string) context.Context {
	md := metadata.MD{}
	for _, s := range v {
		md.Append(Header, s)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestAuthorize(t *testing.T) {
	t.Parallel()
	toks := testTokens(t)
	const read, write = "/xylem.v1.RepoService/Log", "/xylem.v1.WriteService/Land"
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		method string
		code   codes.Code
	}{
		{"no metadata", context.Background(), read, codes.Unauthenticated},
		{"no bearer prefix", withBearer(readerToken), read, codes.Unauthenticated},
		{"empty token", withBearer("Bearer "), read, codes.Unauthenticated},
		{"two tokens", withBearer("Bearer "+readerToken, "Bearer "+writerToken), read, codes.Unauthenticated},
		{"wrong token", withBearer("Bearer nope-nope-nope-nope"), read, codes.Unauthenticated},
		{"reader reads", withBearer("Bearer " + readerToken), read, codes.OK},
		{"reader writes", withBearer("Bearer " + readerToken), write, codes.PermissionDenied},
		{"writer writes", withBearer("Bearer " + writerToken), write, codes.OK},
		{"writer reads", withBearer("Bearer " + writerToken), read, codes.OK},
		{"unknown method", withBearer("Bearer " + writerToken), "/xylem.v1.RepoService/Nope", codes.PermissionDenied},
	} {
		ctx, err := Authorize(tc.ctx, toks, tc.method)
		if status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.code)
			continue
		}
		if id, ok := FromContext(ctx); ok != (tc.code == codes.OK) || ok && id.Name == "" {
			t.Errorf("%s: identity %v, %v", tc.name, id, ok)
		}
	}
}

func TestOutgoingToken(t *testing.T) {
	t.Parallel()
	md := func(ctx context.Context) []string {
		m, _ := metadata.FromOutgoingContext(ctx)
		return m.Get(Header)
	}
	if got := md(withToken(context.Background(), "tier")); !cmp.Equal(got, []string{"Bearer tier"}) {
		t.Errorf("tier token: %v", got)
	}
	// A forwarded client token wins over the tier's.
	fwd := Forward(withBearer("Bearer client"))
	if got := md(withToken(fwd, "tier")); !cmp.Equal(got, []string{"Bearer client"}) {
		t.Errorf("forwarded: %v", got)
	}
	if got := md(Forward(context.Background())); len(got) != 0 {
		t.Errorf("nothing to forward: %v", got)
	}
	if len(DialOptions("")) != 0 || len(DialOptions("x")) == 0 {
		t.Error("DialOptions")
	}
}
