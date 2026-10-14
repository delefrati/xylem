// Package client implements the xy workspace: configuration and state, the
// local object cache, the fetcher, dirstate, checkout and status
// (06-client.md, 09-fetch-and-caching.md).
package client

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
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

// Dial connects to a server address "host:port". "unix://…" addresses are
// reserved for the daemon (M8).
func Dial(addr string) (*Conn, error) {
	switch {
	case addr == "":
		return nil, errors.New("no server address: use --server or run inside a workspace")
	case strings.HasPrefix(addr, "unix://"):
		return nil, fmt.Errorf("server %q: unix socket addresses (the local daemon) are available from M8", addr)
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageSize), grpc.MaxCallSendMsgSize(maxMessageSize)))
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

// Close closes the connection.
func (c *Conn) Close() error { return c.cc.Close() }
