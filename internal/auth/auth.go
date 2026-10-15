// Package auth implements static bearer-token authentication (M9.1): a
// tokens file mapping each token to an identity and roles, the per-RPC role
// table, server interceptors that enforce it, and the client side that sends
// a token.
package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Header is the metadata key that carries "Bearer <token>".
const Header = "authorization"

// Role is a permission an identity holds.
type Role string

// Roles. Write implies read.
const (
	RoleRead  Role = "read"
	RoleWrite Role = "write"
)

// Identity is an authenticated caller.
type Identity struct {
	// Name is "Name <email>"; it becomes the committer of the changesets the
	// caller creates and the actor in the ref log.
	Name  string
	Roles []Role
}

// Has reports whether the identity holds role (write implies read).
func (id Identity) Has(role Role) bool {
	for _, r := range id.Roles {
		if r == role || r == RoleWrite && role == RoleRead {
			return true
		}
	}
	return false
}

// methodRoles is the role each RPC requires. Methods not listed are denied,
// so a new RPC fails closed until it is given a role here.
var methodRoles = map[string]Role{
	"/xylem.v1.RepoService/ResolveRevision":  RoleRead,
	"/xylem.v1.RepoService/GetChangesets":    RoleRead,
	"/xylem.v1.RepoService/ListRefs":         RoleRead,
	"/xylem.v1.RepoService/UpdateRef":        RoleWrite,
	"/xylem.v1.RepoService/Log":              RoleRead,
	"/xylem.v1.RepoService/ListDirectory":    RoleRead,
	"/xylem.v1.RepoService/ReadFile":         RoleRead,
	"/xylem.v1.RepoService/Diff":             RoleRead,
	"/xylem.v1.RepoService/Blame":            RoleRead,
	"/xylem.v1.RepoService/IsAncestor":       RoleRead,
	"/xylem.v1.RepoService/MergeBase":        RoleRead,
	"/xylem.v1.ObjectService/GetObjects":     RoleRead,
	"/xylem.v1.ObjectService/HasObjects":     RoleRead,
	"/xylem.v1.ObjectService/PutObjects":     RoleWrite,
	"/xylem.v1.WriteService/CreateChangeset": RoleWrite,
	"/xylem.v1.WriteService/Rebase":          RoleWrite,
	"/xylem.v1.WriteService/Land":            RoleWrite,
}

// reflectionPrefix is gRPC server reflection (grpcurl), allowed to readers.
const reflectionPrefix = "/grpc.reflection."

// RoleFor returns the role fullMethod requires, or false if it is not
// allowed to anyone.
func RoleFor(fullMethod string) (Role, bool) {
	if r, ok := methodRoles[fullMethod]; ok {
		return r, true
	}
	if strings.HasPrefix(fullMethod, reflectionPrefix) {
		return RoleRead, true
	}
	return "", false
}

type entry struct {
	digest [sha256.Size]byte
	id     Identity
}

// Tokens is a set of tokens loaded from a tokens file.
type Tokens struct{ entries []entry }

// tokensFile is the TOML layout:
//
//	[[token]]
//	identity = "Ada Lovelace <ada@example.com>"
//	roles = ["read", "write"]
//	token = "…"     # or sha256 = "<hex of sha256(token)>"
type tokensFile struct {
	Token []struct {
		Identity string   `toml:"identity"`
		Roles    []string `toml:"roles"`
		Token    string   `toml:"token"`
		SHA256   string   `toml:"sha256"`
	} `toml:"token"`
}

var nameEmail = regexp.MustCompile(`^[^<>\n]+ <[^<>\n]*>$`)

// MinTokenLength is the shortest plain token accepted in a tokens file.
const MinTokenLength = 16

// LoadTokens reads a tokens file.
func LoadTokens(path string) (*Tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading tokens: %w", err)
	}
	t, err := ParseTokens(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// ParseTokens parses a tokens file (see tokensFile).
func ParseTokens(data []byte) (*Tokens, error) {
	var f tokensFile
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return nil, err
	}
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("unknown key %s", un[0])
	}
	if len(f.Token) == 0 {
		return nil, errors.New("no [[token]] entries")
	}
	t := &Tokens{}
	for i, e := range f.Token {
		where := fmt.Sprintf("token %d (%s)", i+1, e.Identity)
		if !nameEmail.MatchString(e.Identity) {
			return nil, fmt.Errorf(`%s: identity must be "Name <email>"`, where)
		}
		var en entry
		en.id.Name = e.Identity
		switch {
		case e.Token != "" && e.SHA256 != "":
			return nil, fmt.Errorf("%s: set token or sha256, not both", where)
		case e.Token != "":
			if len(e.Token) < MinTokenLength {
				return nil, fmt.Errorf("%s: token shorter than %d characters", where, MinTokenLength)
			}
			en.digest = sha256.Sum256([]byte(e.Token))
		case e.SHA256 != "":
			d, err := hex.DecodeString(e.SHA256)
			if err != nil || len(d) != sha256.Size {
				return nil, fmt.Errorf("%s: sha256 must be 64 hex characters", where)
			}
			copy(en.digest[:], d)
		default:
			return nil, fmt.Errorf("%s: no token or sha256", where)
		}
		if len(e.Roles) == 0 {
			return nil, fmt.Errorf("%s: no roles", where)
		}
		for _, r := range e.Roles {
			if Role(r) != RoleRead && Role(r) != RoleWrite {
				return nil, fmt.Errorf("%s: unknown role %q (read, write)", where, r)
			}
			en.id.Roles = append(en.id.Roles, Role(r))
		}
		for _, prev := range t.entries {
			if bytes.Equal(prev.digest[:], en.digest[:]) {
				return nil, fmt.Errorf("%s: same token as %s", where, prev.id.Name)
			}
		}
		t.entries = append(t.entries, en)
	}
	return t, nil
}

// Lookup returns the identity of token. It compares the token's digest with
// every entry in constant time, so timing reveals neither which entry
// matched nor how much of a token was right.
func (t *Tokens) Lookup(token string) (Identity, bool) {
	d := sha256.Sum256([]byte(token))
	found := -1
	for i := range t.entries {
		if subtle.ConstantTimeCompare(d[:], t.entries[i].digest[:]) == 1 {
			found = i
		}
	}
	if found < 0 || token == "" {
		return Identity{}, false
	}
	return t.entries[found].id, true
}

type identityKey struct{}

// NewContext returns ctx carrying id.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the authenticated identity of the call, if auth is on.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// bearer extracts the token from the call's incoming metadata.
func bearer(ctx context.Context) (string, bool) {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get(Header)
	if len(v) != 1 {
		return "", false
	}
	token, ok := strings.CutPrefix(v[0], "Bearer ")
	return token, ok && token != ""
}

// Authorize checks the call's token against tokens and the role fullMethod
// requires: Unauthenticated without a valid token, PermissionDenied without
// the role. It returns ctx carrying the identity.
func Authorize(ctx context.Context, tokens *Tokens, fullMethod string) (context.Context, error) {
	token, ok := bearer(ctx)
	if !ok {
		return ctx, status.Error(codes.Unauthenticated, `missing token: send "authorization: Bearer <token>"`)
	}
	id, ok := tokens.Lookup(token)
	if !ok {
		return ctx, status.Error(codes.Unauthenticated, "invalid token")
	}
	role, ok := RoleFor(fullMethod)
	if !ok {
		return ctx, status.Errorf(codes.PermissionDenied, "%s is not allowed", fullMethod)
	}
	if !id.Has(role) {
		return ctx, status.Errorf(codes.PermissionDenied, "%s needs the %s role", fullMethod, role)
	}
	return NewContext(ctx, id), nil
}

// ServerInterceptors enforce tokens on every call (for tiers; the server
// calls Authorize from its own interceptors).
func ServerInterceptors(tokens *Tokens) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			ctx, err := Authorize(ctx, tokens, info.FullMethod)
			if err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			ctx, err := Authorize(ss.Context(), tokens, info.FullMethod)
			if err != nil {
				return err
			}
			return h(srv, &stream{ServerStream: ss, ctx: ctx})
		}),
	}
}

type stream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *stream) Context() context.Context { return s.ctx }

// withToken adds token to an outgoing call unless the call already carries
// one (a tier forwarding its client's token).
func withToken(ctx context.Context, token string) context.Context {
	if md, ok := metadata.FromOutgoingContext(ctx); ok && len(md.Get(Header)) > 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, Header, "Bearer "+token)
}

// DialOptions make a client send token on every call; nothing if token is
// empty.
func DialOptions(token string) []grpc.DialOption {
	if token == "" {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(withToken(ctx, token), method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(withToken(ctx, token), desc, cc, method, opts...)
		}),
	}
}

// Forward copies the incoming call's token, if any, to the outgoing context,
// so a tier's upstream call acts as its client.
func Forward(ctx context.Context) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(Header); len(v) == 1 {
		return metadata.AppendToOutgoingContext(ctx, Header, v[0])
	}
	return ctx
}
