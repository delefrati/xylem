package client

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/delefrati/xylem/internal/diff"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
)

var gitModes = map[object.Mode]string{object.ModeFile: "100644", object.ModeExec: "100755", object.ModeSymlink: "120000"}

// WriteDiff prints a unified diff of the working copy against its parent
// for the changed paths (all, or those at or under one of filter), in git's
// format. Base blobs come from the cache (fetched if missing).
func (w *Workspace) WriteDiff(ctx context.Context, f *Fetcher, out io.Writer, filter [][]byte, contextLines int) error {
	status, err := w.Status(ctx, f)
	if err != nil {
		return err
	}
	ds, err := LoadDirstate(w.Meta("dirstate"))
	if err != nil {
		return err
	}
	var bases []hash.ID
	for _, s := range status {
		if e, ok := ds.Get(s.Path); ok {
			bases = append(bases, e.Blob)
		}
	}
	if err := f.Ensure(ctx, bases); err != nil {
		return err
	}
	for _, s := range status {
		if s.Code == StatusUntracked || !under(s.Path, filter) {
			continue
		}
		var oldMode, newMode object.Mode
		var oldContent, newContent []byte
		if e, ok := ds.Get(s.Path); ok {
			oldMode = e.Mode
			if _, oldContent, err = f.cache.GetVerified(e.Blob); err != nil {
				return err
			}
		}
		if s.Code != StatusDeleted {
			abs := filepath.Join(w.Root, string(s.Path))
			info, err := os.Lstat(abs)
			if err != nil {
				return err
			}
			newMode, _ = FileMode(info)
			if newMode == object.ModeSymlink {
				target, err := os.Readlink(abs)
				if err != nil {
					return err
				}
				newContent = []byte(target)
			} else if newContent, err = os.ReadFile(abs); err != nil {
				return err
			}
		}
		if err := writeFileDiff(out, s.Path, oldMode, newMode, oldContent, newContent, contextLines); err != nil {
			return err
		}
	}
	return nil
}

func under(p []byte, filter [][]byte) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if paths.IsUnder(p, f) {
			return true
		}
	}
	return false
}

func writeFileDiff(out io.Writer, path []byte, oldMode, newMode object.Mode, oldC, newC []byte, contextLines int) error {
	fd := fileDiff{path: path, oldMode: gitModes[oldMode], newMode: gitModes[newMode], same: string(oldC) == string(newC)}
	if !fd.same {
		if fd.binary = diff.IsBinary(oldC) || diff.IsBinary(newC); !fd.binary {
			fd.hunks = diff.Hunks(oldC, newC, contextLines)
		}
	}
	return fd.write(out)
}

// fileDiff is one file's diff, printed in git's format. Modes are git octal
// strings, empty for a missing side.
type fileDiff struct {
	path             []byte
	oldMode, newMode string
	same             bool // content unchanged (mode-only change)
	binary           bool
	hunks            []diff.Hunk
}

func (d fileDiff) write(out io.Writer) error {
	p := string(d.path)
	w := func(format string, args ...any) error { _, err := fmt.Fprintf(out, format, args...); return err }
	if err := w("diff --xy a/%s b/%s\n", p, p); err != nil {
		return err
	}
	switch {
	case d.oldMode == "":
		if err := w("new file mode %s\n", d.newMode); err != nil {
			return err
		}
	case d.newMode == "":
		if err := w("deleted file mode %s\n", d.oldMode); err != nil {
			return err
		}
	case d.oldMode != d.newMode:
		if err := w("old mode %s\nnew mode %s\n", d.oldMode, d.newMode); err != nil {
			return err
		}
	}
	if d.same || !d.binary && len(d.hunks) == 0 { // mode-only, or an empty file added or deleted
		return nil
	}
	from, to := "a/"+p, "b/"+p
	if d.oldMode == "" {
		from = "/dev/null"
	}
	if d.newMode == "" {
		to = "/dev/null"
	}
	if d.binary {
		return w("Binary files %s and %s differ\n", from, to)
	}
	if err := w("--- %s\n+++ %s\n", from, to); err != nil {
		return err
	}
	for _, h := range d.hunks {
		if err := w("%s\n%s", h.Header(), h.Body); err != nil {
			return err
		}
	}
	return nil
}
