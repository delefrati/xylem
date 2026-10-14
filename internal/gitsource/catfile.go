package gitsource

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
)

// CatFilePool runs long-lived `git cat-file --batch` processes. Each request
// checks one out, writes "<sha>\n" and reads "<sha> <type> <size>\n<data>\n"
// (or "<sha> missing\n").
type CatFilePool struct {
	repo  *Repo
	procs chan *catFile // idle processes, or nil slots to start lazily
	mu    sync.Mutex
	all   []*catFile
	err   error // set once closed
}

type catFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func newCatFilePool(r *Repo, n int) *CatFilePool {
	p := &CatFilePool{repo: r, procs: make(chan *catFile, n)}
	for range n {
		p.procs <- nil
	}
	return p
}

func (p *CatFilePool) start() (*catFile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	// Not tied to a request context: the process outlives requests.
	cmd := p.repo.command(context.Background(), "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting git cat-file: %w", err)
	}
	cf := &catFile{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 64<<10)}
	p.all = append(p.all, cf)
	return cf, nil
}

// Object returns an object's type ("blob", "tree", "commit", "tag") and
// content, or an error wrapping ErrNotFound.
func (p *CatFilePool) Object(ctx context.Context, sha SHA) (string, []byte, error) {
	var cf *catFile
	select {
	case cf = <-p.procs:
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
	if cf == nil {
		var err error
		if cf, err = p.start(); err != nil {
			p.procs <- nil
			return "", nil, err
		}
	}
	typ, data, err := cf.read(sha)
	if err != nil && !errors.Is(err, ErrNotFound) {
		// The stream may be out of sync: replace the process.
		_ = cf.close()
		cf = nil
	}
	p.procs <- cf
	return typ, data, err
}

func (cf *catFile) read(sha SHA) (string, []byte, error) {
	if _, err := fmt.Fprintf(cf.in, "%s\n", sha); err != nil {
		return "", nil, fmt.Errorf("git cat-file: writing request: %w", err)
	}
	header, err := cf.out.ReadBytes('\n')
	if err != nil {
		return "", nil, fmt.Errorf("git cat-file: reading header: %w", err)
	}
	fields := bytes.Fields(header)
	if len(fields) == 2 && string(fields[1]) == "missing" {
		return "", nil, fmt.Errorf("object %s: %w", sha, ErrNotFound)
	}
	if len(fields) != 3 || string(fields[0]) != sha.String() {
		return "", nil, fmt.Errorf("git cat-file: unexpected header %q", header)
	}
	size, err := strconv.Atoi(string(fields[2]))
	if err != nil || size < 0 {
		return "", nil, fmt.Errorf("git cat-file: bad size in %q", header)
	}
	data := make([]byte, size+1) // content plus the trailing newline
	if _, err := io.ReadFull(cf.out, data); err != nil {
		return "", nil, fmt.Errorf("git cat-file: reading %s: %w", sha, err)
	}
	if data[size] != '\n' {
		return "", nil, fmt.Errorf("git cat-file: %s not followed by a newline", sha)
	}
	return string(fields[1]), data[:size], nil
}

func (cf *catFile) close() error {
	_ = cf.in.Close()
	return cf.cmd.Wait()
}

// Close stops all processes. Later requests fail.
func (p *CatFilePool) Close() error {
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return nil
	}
	p.err = errors.New("git cat-file pool closed")
	all := p.all
	p.all = nil
	p.mu.Unlock()
	var errs []error
	for _, cf := range all {
		if err := cf.close(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Object reads an object through the repository's cat-file pool.
func (r *Repo) Object(ctx context.Context, sha SHA) (string, []byte, error) {
	return r.catFile.Object(ctx, sha)
}

// Blob returns a blob's content.
func (r *Repo) Blob(ctx context.Context, sha SHA) ([]byte, error) {
	typ, data, err := r.Object(ctx, sha)
	if err != nil {
		return nil, err
	}
	if typ != "blob" {
		return nil, fmt.Errorf("object %s is a %s, not a blob", sha, typ)
	}
	return data, nil
}
