// Package client implements the xy workspace: configuration and state, the
// local object cache, the fetcher, dirstate, checkout and status
// (06-client.md, 09-fetch-and-caching.md).
package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/auth"
	"github.com/delefrati/xylem/internal/tracing"
)

// Exit codes (06 §Commands).
const (
	ExitOK       = 0
	ExitFailure  = 1
	ExitUsage    = 2
	ExitConflict = 3
	ExitDirty    = 4
)

// ExitError carries a process exit code with an error.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// Usage wraps err as a usage error (exit 2).
func Usage(err error) error { return &ExitError{Code: ExitUsage, Err: err} }

// ExitCode returns the exit code for err: its ExitError code, else 1.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	return ExitFailure
}

// maxMessageSize matches the server's gRPC limit (05 §Conventions).
const maxMessageSize = 80 << 20

// Conn is a connection to a Xylem server (or, from M8, a cache or daemon).
type Conn struct {
	cc     *grpc.ClientConn
	Repo   xylemv1.RepoServiceClient
	Object xylemv1.ObjectServiceClient
	Write  xylemv1.WriteServiceClient
}

// Dial connects to a server or cache tier at "host:port", or to the local
// daemon at "unix:///path/to/daemon.sock" (09 §Local client cache vs daemon),
// sending token (if not empty) on every call.
func Dial(addr, token string) (*Conn, error) {
	switch {
	case addr == "":
		return nil, errors.New("no server address: use --server or run inside a workspace")
	case addr == "unix://" || strings.HasPrefix(addr, "unix://") && !strings.HasPrefix(addr, "unix:///"):
		return nil, fmt.Errorf("server %q: a daemon address is unix:// followed by an absolute socket path", addr)
	}
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageSize), grpc.MaxCallSendMsgSize(maxMessageSize)),
		tracing.DialOption(),
	}, auth.DialOptions(token)...)
	cc, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	return &Conn{
		cc:     cc,
		Repo:   xylemv1.NewRepoServiceClient(cc),
		Object: xylemv1.NewObjectServiceClient(cc),
		Write:  xylemv1.NewWriteServiceClient(cc),
	}, nil
}

// ResolveToken finds the bearer token: the XYLEM_TOKEN environment
// variable, then token in the workspace config (w may be nil), then token in
// the user config file (~/.config/xylem/config.toml). Empty means none.
func ResolveToken(w *Workspace) (string, error) {
	if t := strings.TrimSpace(os.Getenv("XYLEM_TOKEN")); t != "" {
		return t, nil
	}
	if w != nil && w.Config.Token != "" {
		return w.Config.Token, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", nil
	}
	var cfg struct {
		Token string `toml:"token"`
	}
	path := filepath.Join(dir, "xylem", "config.toml")
	if _, err := toml.DecodeFile(path, &cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return cfg.Token, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.cc.Close() }
