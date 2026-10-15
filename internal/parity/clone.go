package parity

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/delefrati/xylem/internal/client"
)

// archiveFile is a file as git archive or a working tree has it.
type archiveFile struct {
	exec    bool
	symlink bool
	content []byte // or the symlink target
}

func (f archiveFile) String() string {
	switch {
	case f.symlink:
		return fmt.Sprintf("symlink -> %q", f.content)
	case f.exec:
		return fmt.Sprintf("exec, %d bytes", len(f.content))
	}
	return fmt.Sprintf("%d bytes", len(f.content))
}

// gitArchive reads `git archive <sha>` (the reference for clone parity).
func (r *run) gitArchive(sha string) (map[string]archiveFile, error) {
	out, err := r.git.Git(r.ctx, "archive", "--format=tar", sha)
	if err != nil {
		return nil, err
	}
	files := map[string]archiveFile{}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		switch h.Typeflag {
		case tar.TypeReg:
			content, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			files[h.Name] = archiveFile{exec: h.Mode&0o100 != 0, content: content}
		case tar.TypeSymlink:
			files[h.Name] = archiveFile{symlink: true, content: []byte(h.Linkname)}
		}
	}
}

// workingTree reads a checkout's files, skipping .xylem.
func workingTree(root string) (map[string]archiveFile, error) {
	files := map[string]archiveFile{}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rt.Close() }()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == client.MetaDir:
			return filepath.SkipDir
		case d.IsDir():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := rt.Readlink(rel)
			files[rel] = archiveFile{symlink: true, content: []byte(target)}
			return err
		}
		content, err := rt.ReadFile(rel)
		files[rel] = archiveFile{exec: info.Mode().Perm()&0o100 != 0, content: content}
		return err
	})
	return files, err
}

// sparseProfiles picks n random profiles of one to three directories.
func (r *run) sparseProfiles(sha string, n int) ([][]string, error) {
	out, err := r.lit("ls-tree", "-r", "-d", "-z", "--name-only", sha)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, d := range strings.Split(string(out), "\x00") {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return nil, nil
	}
	profiles := make([][]string, n)
	for i := range profiles {
		profiles[i] = sample(r, dirs, 1+r.intn(min(3, len(dirs))))
	}
	return profiles, nil
}

// inScope mirrors the sparse rule: root files, and files under a profile dir.
func inScope(p string, profile []string) bool {
	if len(profile) == 0 || !strings.Contains(p, "/") {
		return true
	}
	return slices.ContainsFunc(profile, func(d string) bool { return strings.HasPrefix(p, d+"/") })
}

// checkClone clones the tip with xy's client (full, then three random sparse
// profiles) and compares each working tree with git archive: the file set,
// contents, exec bits and symlink targets. Every expected or unexpected file
// is one comparison.
func (r *run) checkClone() (Result, error) {
	var res Result
	if r.x.Addr == "" {
		return res, errors.New("the clone check needs the server address (Clients.Addr)")
	}
	want, err := r.gitArchive(r.tip)
	if err != nil {
		return res, err
	}
	profiles, err := r.sparseProfiles(r.tip, 3)
	if err != nil {
		return res, err
	}
	for _, profile := range append([][]string{nil}, profiles...) {
		if err := r.compareClone(&res, want, profile); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (r *run) compareClone(res *Result, want map[string]archiveFile, profile []string) error {
	tmp, err := os.MkdirTemp("", "xylem-parity-clone-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	dir := filepath.Join(tmp, "ws")
	if _, err := client.Clone(r.ctx, r.x.Addr, dir, client.CloneOptions{Repo: r.opts.Repo, Rev: rev(r.tip), Sparse: profile}); err != nil {
		return fmt.Errorf("clone %q: %w", profile, err)
	}
	got, err := workingTree(dir)
	if err != nil {
		return err
	}
	label := "full"
	if profile != nil {
		label = "sparse " + strings.Join(profile, ",")
	}
	for p, w := range want {
		if !inScope(p, profile) {
			continue
		}
		res.Compared++
		g, ok := got[p]
		switch {
		case !ok:
			r.fail(res, "%s: %q missing from the clone", label, p)
		case g.exec != w.exec || g.symlink != w.symlink || !bytes.Equal(g.content, w.content):
			r.fail(res, "%s: %q is %s in the clone, %s in git", label, p, g, w)
		default:
			res.Passed++
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok || !inScope(p, profile) {
			res.Compared++
			r.fail(res, "%s: unexpected file %q in the clone (in git: %v)", label, p, ok)
		}
	}
	return nil
}
