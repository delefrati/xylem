package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/canon"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
	"github.com/delefrati/xylem/internal/repo"
	"github.com/delefrati/xylem/internal/treeops"
)

// Paging defaults (05 §Conventions).
const (
	defaultPageSize = 100
	maxPageSize     = 1000
	readChunkSize   = 1 << 20 // ReadFile streams ≤ 1 MiB per message
	defaultRevision = "main"
)

func pageSize(p *xylemv1.PageRequest) int {
	n := int(p.GetPageSize())
	switch {
	case n <= 0:
		return defaultPageSize
	case n > maxPageSize:
		return maxPageSize
	}
	return n
}

func orMain(rev string) string {
	if rev == "" {
		return defaultRevision
	}
	return rev
}

// dirToken pins a listing to the directory's tree id, so later pages stay
// consistent even if the revision moves: version 1 ‖ tree id ‖ bytes(dir path)
// ‖ bytes(last path returned, relative to the directory).
type dirToken struct {
	tree  hash.ID
	dir   []byte
	after []byte
}

func (t dirToken) encode() string {
	b := []byte{1}
	b = append(b, t.tree[:]...)
	b = canon.AppendBytes(b, t.dir)
	b = canon.AppendBytes(b, t.after)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeDirToken(s string) (dirToken, error) {
	var t dirToken
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < 1+hash.Size || b[0] != 1 {
		return t, invalidf("malformed page token")
	}
	copy(t.tree[:], b[1:1+hash.Size])
	r := canon.NewReader(b[1+hash.Size:])
	if t.dir, err = r.Bytes(); err != nil {
		return t, invalidf("malformed page token")
	}
	if t.after, err = r.Bytes(); err != nil || r.Len() != 0 {
		return t, invalidf("malformed page token")
	}
	return t, nil
}

// lookupDir returns the tree id of directory path at revision rev.
func (s *Server) lookupDir(ctx context.Context, r *repo.Repo, rev string, path []byte) (hash.ID, error) {
	cs, err := s.resolve(ctx, r, orMain(rev))
	if err != nil {
		return hash.ID{}, err
	}
	if len(path) == 0 {
		return cs.Core.RootTree, nil
	}
	if err := paths.ValidPath(path); err != nil {
		return hash.ID{}, invalidf("%v", err)
	}
	e, found, err := treeops.Lookup(ctx, r.Trees(), cs.Core.RootTree, path)
	if err != nil {
		return hash.ID{}, err
	}
	if !found {
		return hash.ID{}, notFoundf("path %q", path)
	}
	if e.Mode != object.ModeTree {
		return hash.ID{}, invalidf("%q is a file, not a directory", path)
	}
	return e.ID, nil
}

func toDirEntry(name []byte, e object.Entry) *xylemv1.DirEntry {
	return &xylemv1.DirEntry{Name: name, Mode: toProtoMode(e.Mode), Id: e.ID[:], Size: e.Size}
}

// ListDirectory implements RepoService. Non-recursive listings return the
// directory's entries by name. Recursive listings return every file below it
// (not directories), depth-first in path order, with repository-relative
// paths in name, like `git ls-tree -r --full-tree`.
func (s *Server) ListDirectory(ctx context.Context, req *xylemv1.ListDirectoryRequest) (*xylemv1.ListDirectoryResponse, error) {
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return nil, err
	}
	var tok dirToken
	if t := req.GetPage().GetPageToken(); t != "" {
		if tok, err = decodeDirToken(t); err != nil {
			return nil, err
		}
	} else {
		tok.dir = req.GetPath()
		if tok.tree, err = s.lookupDir(ctx, r, req.GetRevision(), tok.dir); err != nil {
			return nil, err
		}
	}
	limit := pageSize(req.GetPage())
	resp := &xylemv1.ListDirectoryResponse{TreeId: tok.tree[:]}
	var last []byte
	more := false

	if req.GetRecursive() {
		err = treeops.WalkFiles(ctx, r.Trees(), tok.tree, tok.after, func(p []byte, e object.Entry) error {
			if len(resp.Entries) == limit {
				more = true
				return treeops.ErrStop
			}
			full := p
			if len(tok.dir) > 0 {
				full = paths.Join(tok.dir, p)
			}
			resp.Entries = append(resp.Entries, toDirEntry(full, e))
			last = p
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		tree, err := r.Trees().ReadTree(ctx, tok.tree)
		if err != nil {
			return nil, err
		}
		for _, e := range tree.Entries {
			if len(tok.after) > 0 && bytes.Compare(e.Name, tok.after) <= 0 {
				continue
			}
			if len(resp.Entries) == limit {
				more = true
				break
			}
			resp.Entries = append(resp.Entries, toDirEntry(e.Name, e))
			last = e.Name
		}
	}
	if more {
		resp.NextPageToken = dirToken{tree: tok.tree, dir: tok.dir, after: last}.encode()
	}
	return resp, nil
}

// ReadFile implements RepoService: the first message carries mode, size and
// blob id; content follows in chunks of at most 1 MiB.
func (s *Server) ReadFile(req *xylemv1.ReadFileRequest, stream xylemv1.RepoService_ReadFileServer) error {
	ctx := stream.Context()
	r, err := s.repo(ctx, req.GetRepo())
	if err != nil {
		return err
	}
	if err := paths.ValidPath(req.GetPath()); err != nil {
		return invalidf("%v", err)
	}
	cs, err := s.resolve(ctx, r, orMain(req.GetRevision()))
	if err != nil {
		return err
	}
	e, found, err := treeops.Lookup(ctx, r.Trees(), cs.Core.RootTree, req.GetPath())
	if err != nil {
		return err
	}
	if !found {
		return notFoundf("path %q", req.GetPath())
	}
	if e.Mode == object.ModeTree {
		return invalidf("%q is a directory", req.GetPath())
	}
	content, err := s.readBlob(ctx, e.ID)
	if err != nil {
		return err
	}
	first := &xylemv1.ReadFileResponse{Mode: toProtoMode(e.Mode), Size: e.Size, BlobId: e.ID[:]}
	for off := 0; ; off += readChunkSize {
		end := min(off+readChunkSize, len(content))
		msg := first
		if msg == nil {
			msg = &xylemv1.ReadFileResponse{}
		}
		msg.Chunk = content[off:end]
		if err := stream.Send(msg); err != nil {
			return err
		}
		first = nil
		if end == len(content) {
			return nil
		}
	}
}

// readBlob returns a blob's content.
func (s *Server) readBlob(ctx context.Context, id hash.ID) ([]byte, error) {
	stored, err := s.readObject(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("blob %s is referenced but missing from the object store", id)
	}
	kind, body, err := object.Decode(stored)
	if err != nil {
		return nil, err
	}
	if kind != object.KindBlob {
		return nil, fmt.Errorf("object %s is a %v, not a blob", id, kind)
	}
	return body, nil
}
