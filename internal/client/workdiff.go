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
	p := string(path)
	w := func(format string, args ...any) error { _, err := fmt.Fprintf(out, format, args...); return err }
	if err := w("diff --xy a/%s b/%s\n", p, p); err != nil {
		return err
	}
	switch {
	case oldMode == 0:
		if err := w("new file mode %s\n", gitModes[newMode]); err != nil {
			return err
		}
	case newMode == 0:
		if err := w("deleted file mode %s\n", gitModes[oldMode]); err != nil {
			return err
		}
	case oldMode != newMode:
		if err := w("old mode %s\nnew mode %s\n", gitModes[oldMode], gitModes[newMode]); err != nil {
			return err
		}
	}
	if string(oldC) == string(newC) {
		return nil
	}
	from, to := "a/"+p, "b/"+p
	if oldMode == 0 {
		from = "/dev/null"
	}
	if newMode == 0 {
		to = "/dev/null"
	}
	if diff.IsBinary(oldC) || diff.IsBinary(newC) {
		return w("Binary files %s and %s differ\n", from, to)
	}
	if err := w("--- %s\n+++ %s\n", from, to); err != nil {
		return err
	}
	for _, h := range diff.Hunks(oldC, newC, contextLines) {
		if err := w("%s\n%s", h.Header(), h.Body); err != nil {
			return err
		}
	}
	return nil
}
