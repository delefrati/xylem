// Package importer is the git → Xylem import pipeline (08-git-import.md).
package importer

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/paths"
	"github.com/delefrati/xylem/internal/repo"
)

// ExtraSubmodules lists, newline-separated, the submodule (gitlink) paths a
// commit touched that were skipped.
const ExtraSubmodules = "git-extra-submodules"

// ErrUnsupportedPath reports a git path Xylem cannot store, e.g. one with a
// ".xylem" component. The import fails rather than rewriting history.
var ErrUnsupportedPath = errors.New("git path not supported by Xylem")

// BlobInfo is the Xylem id and size of a git blob, once uploaded.
type BlobInfo struct {
	ID   hash.ID
	Size uint64
}

// Mapped is a git commit translated into a CreateChangeset call.
type Mapped struct {
	Request   repo.CreateRequest
	Committer changeset.Signature
	// Submodules are the gitlink paths this commit touched (skipped).
	Submodules [][]byte
}

// xylemMode maps a git file mode; ok is false for paths Xylem does not store
// (absent or submodule).
func xylemMode(m uint32) (object.Mode, bool, error) {
	switch m {
	case 0, gitsource.ModeSubmodule:
		return 0, false, nil
	case gitsource.ModeFile, 0o100664: // 100664: group-writable files from very old git
		return object.ModeFile, true, nil
	case gitsource.ModeExec:
		return object.ModeExec, true, nil
	case gitsource.ModeSymlink:
		return object.ModeSymlink, true, nil
	default:
		return 0, false, fmt.Errorf("unsupported git mode %o", m)
	}
}

// BlobsNeeded returns the git blob shas a commit's changes put, deduplicated.
func BlobsNeeded(changes []gitsource.Change) []gitsource.SHA {
	var out []gitsource.SHA
	for _, ch := range changes {
		if _, present, err := xylemMode(ch.NewMode); err == nil && present && !slices.Contains(out, ch.NewSHA) {
			out = append(out, ch.NewSHA)
		}
	}
	return out
}

// MapCommit translates a git commit and its diff against the first parent
// (08 §Mapping). parents are the already-mapped Xylem ids of c.Parents, in
// order; blob returns the Xylem id and size of a git blob.
func MapCommit(c *gitsource.Commit, changes []gitsource.Change, parents []hash.ID,
	blob func(gitsource.SHA) (BlobInfo, bool),
) (*Mapped, error) {
	if len(parents) != len(c.Parents) {
		return nil, fmt.Errorf("commit %s: %d mapped parents for %d git parents", c.SHA, len(parents), len(c.Parents))
	}
	m := &Mapped{
		Request: repo.CreateRequest{
			Parents: parents,
			Author:  signature(c.Author),
			Message: c.Message,
			Extras:  map[string][]byte{changeset.ExtraGitSHA: []byte(c.SHA.String())},
		},
		Committer: signature(c.Committer),
	}
	if c.Encoding != "" {
		m.Request.Extras[changeset.ExtraGitEncoding] = []byte(c.Encoding)
	}
	for _, ch := range changes {
		oldMode, oldPresent, err := xylemMode(ch.OldMode)
		if err != nil {
			return nil, fmt.Errorf("commit %s path %q: %w", c.SHA, ch.Path, err)
		}
		newMode, newPresent, err := xylemMode(ch.NewMode)
		if err != nil {
			return nil, fmt.Errorf("commit %s path %q: %w", c.SHA, ch.Path, err)
		}
		if ch.OldMode == gitsource.ModeSubmodule || ch.NewMode == gitsource.ModeSubmodule {
			m.Submodules = append(m.Submodules, ch.Path)
		}
		if !oldPresent && !newPresent {
			continue // a submodule-only change
		}
		if err := paths.ValidPath(ch.Path); err != nil {
			return nil, fmt.Errorf("commit %s: %w: %w", c.SHA, ErrUnsupportedPath, err)
		}
		switch {
		case newPresent && oldPresent && oldMode == newMode && ch.OldSHA == ch.NewSHA:
			continue // e.g. 100664 → 100644: invisible in Xylem
		case newPresent:
			info, ok := blob(ch.NewSHA)
			if !ok {
				return nil, fmt.Errorf("commit %s path %q: blob %s not uploaded", c.SHA, ch.Path, ch.NewSHA)
			}
			m.Request.Changes = append(m.Request.Changes, changeset.Change{
				Path: ch.Path, Op: changeset.OpPut, Mode: newMode, Size: info.Size, Blob: info.ID,
			})
		default:
			m.Request.Changes = append(m.Request.Changes, changeset.Change{Path: ch.Path, Op: changeset.OpDelete})
		}
	}
	if len(m.Submodules) > 0 {
		m.Request.Extras[ExtraSubmodules] = bytes.Join(m.Submodules, []byte{'\n'})
	}
	return m, nil
}

func signature(id gitsource.Ident) changeset.Signature {
	return changeset.Signature{NameEmail: id.NameEmail, Time: id.Time, TZMinutes: id.TZMinutes}
}
