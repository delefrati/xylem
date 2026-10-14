package parity

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/diff"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/object"
)

// emptyTree is git's empty tree object, the "parent" of a root commit.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func rev(sha string) string { return "git:" + sha }

// lit runs git with literal pathspecs (no glob or magic in paths).
func (r *run) lit(args ...string) ([]byte, error) {
	return r.git.Git(r.ctx, append([]string{"--literal-pathspecs"}, args...)...)
}

// ---- tree

type gitFile struct {
	mode uint32
	size int64
	sha  gitsource.SHA
}

func (r *run) gitTree(sha string) (map[string]gitFile, error) {
	s, err := gitsource.ParseSHA(sha)
	if err != nil {
		return nil, err
	}
	entries, err := r.git.LsTree(r.ctx, s)
	if err != nil {
		return nil, err
	}
	out := map[string]gitFile{}
	for _, e := range entries {
		if e.Mode != gitsource.ModeSubmodule { // skipped by design
			out[string(e.Path)] = gitFile{e.Mode, e.Size, e.SHA}
		}
	}
	return out, nil
}

func (r *run) xylemTree(sha string) (map[string]*xylemv1.DirEntry, error) {
	out := map[string]*xylemv1.DirEntry{}
	req := &xylemv1.ListDirectoryRequest{Repo: r.opts.Repo, Revision: rev(sha), Recursive: true, Page: &xylemv1.PageRequest{PageSize: 1000}}
	for {
		resp, err := r.x.Repo.ListDirectory(r.ctx, req)
		if err != nil {
			return nil, err
		}
		for _, e := range resp.Entries {
			out[string(e.Name)] = e
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		req.Page.PageToken = resp.NextPageToken
	}
}

var gitToXylemMode = map[uint32]xylemv1.FileMode{
	gitsource.ModeFile: xylemv1.FileMode_FILE_MODE_FILE, 0o100664: xylemv1.FileMode_FILE_MODE_FILE,
	gitsource.ModeExec: xylemv1.FileMode_FILE_MODE_EXEC, gitsource.ModeSymlink: xylemv1.FileMode_FILE_MODE_SYMLINK,
}

// xylemBlobID re-hashes a git blob into the Xylem id of the same content.
func (r *run) xylemBlobID(sha gitsource.SHA) ([]byte, error) {
	if id, ok := r.blobIDs[sha]; ok {
		return id, nil
	}
	data, err := r.git.Blob(r.ctx, sha)
	if err != nil {
		return nil, err
	}
	id := object.ID(object.KindBlob, data)
	r.blobIDs[sha] = id[:]
	return id[:], nil
}

func (r *run) sampleCommits() []string {
	commits := sample(r, r.commits, r.opts.Commits)
	if !slices.Contains(commits, r.tip) {
		commits = append([]string{r.tip}, commits...)
	}
	return commits
}

func (r *run) checkTree() (Result, error) {
	var res Result
	for _, sha := range r.sampleCommits() {
		want, err := r.gitTree(sha)
		if err != nil {
			return res, err
		}
		got, err := r.xylemTree(sha)
		if err != nil {
			return res, err
		}
		res.Compared++
		ok := len(got) == len(want)
		if !ok {
			r.fail(&res, "%s: %d files in Xylem, %d in git", short(sha), len(got), len(want))
		}
		for p, g := range want {
			x, found := got[p]
			if !found {
				r.fail(&res, "%s: %q missing in Xylem", short(sha), p)
				ok = false
				continue
			}
			id, err := r.xylemBlobID(g.sha)
			if err != nil {
				return res, err
			}
			if x.Mode != gitToXylemMode[g.mode] || g.size < 0 || x.Size != uint64(g.size) || !bytes.Equal(x.Id, id) {
				r.fail(&res, "%s: %q differs (mode %v/%o, size %d/%d)", short(sha), p, x.Mode, g.mode, x.Size, g.size)
				ok = false
			}
		}
		if ok {
			res.Passed++
		}
	}
	return res, nil
}

// ---- log

func (r *run) xylemLog(path string) ([]string, error) {
	var out []string
	req := &xylemv1.LogRequest{Repo: r.opts.Repo, Revision: rev(r.tip), Path: []byte(path), Page: &xylemv1.PageRequest{PageSize: 1000}}
	for {
		resp, err := r.x.Repo.Log(r.ctx, req)
		if err != nil {
			return nil, err
		}
		for _, cs := range resp.Changesets {
			out = append(out, cs.GitSha)
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		req.Page.PageToken = resp.NextPageToken
	}
}

func (r *run) compareLog(res *Result, path string) error {
	args := []string{"log", "--first-parent", "--format=%H", r.tip}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := r.lit(args...)
	if err != nil {
		return err
	}
	want := strings.Fields(string(out))
	got, err := r.xylemLog(path)
	if err != nil {
		return err
	}
	res.Compared++
	if slices.Equal(got, want) {
		res.Passed++
	} else {
		r.fail(res, "log %q: %d entries in Xylem, %d in git", path, len(got), len(want))
	}
	return nil
}

func (r *run) checkLog() (Result, error) {
	var res Result
	return res, r.compareLog(&res, "")
}

func (r *run) checkLogPath() (Result, error) {
	var res Result
	files, err := r.gitTree(r.tip)
	if err != nil {
		return res, err
	}
	all := slices.Sorted(func(yield func(string) bool) {
		for p := range files {
			if !yield(p) {
				return
			}
		}
	})
	dirSet := map[string]bool{}
	for _, p := range all {
		for i := strings.LastIndexByte(p, '/'); i > 0; i = strings.LastIndexByte(p[:i], '/') {
			dirSet[p[:i]] = true
		}
	}
	dirs := slices.Sorted(func(yield func(string) bool) {
		for d := range dirSet {
			if !yield(d) {
				return
			}
		}
	})
	for _, p := range append(sample(r, all, 50), sample(r, dirs, 10)...) {
		if err := r.compareLog(&res, p); err != nil {
			return res, err
		}
	}
	return res, nil
}

// ---- diff

type gitChange struct {
	status         string // A, D, M (M covers git's T)
	oldSHA, newSHA gitsource.SHA
}

// gitDiff returns path → change from `git diff --raw`, ignoring submodules
// (a submodule ↔ file change appears as the file's add or delete).
func (r *run) gitDiff(a, b string, paths []string) (map[string]gitChange, error) {
	args := []string{"diff", "--raw", "-z", "--no-renames", "--abbrev=40", a, b}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	out, err := r.lit(args...)
	if err != nil {
		return nil, err
	}
	m := map[string]gitChange{}
	recs := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	for i := 0; i+1 < len(recs); i += 2 {
		f := strings.Fields(strings.TrimPrefix(recs[i], ":"))
		if len(f) != 5 {
			continue
		}
		oldMode, _ := strconv.ParseUint(f[0], 8, 32)
		newMode, _ := strconv.ParseUint(f[1], 8, 32)
		oldIn := oldMode != 0 && oldMode != uint64(gitsource.ModeSubmodule)
		newIn := newMode != 0 && newMode != uint64(gitsource.ModeSubmodule)
		c := gitChange{}
		c.oldSHA, _ = gitsource.ParseSHA(f[2])
		c.newSHA, _ = gitsource.ParseSHA(f[3])
		switch {
		case !oldIn && !newIn:
			continue
		case !oldIn:
			c.status = "A"
		case !newIn:
			c.status = "D"
		default:
			c.status = "M"
		}
		m[recs[i+1]] = c
	}
	return m, nil
}

type diffPair struct {
	from, to    string // git shas; from may be emptyTree
	firstParent bool   // request Xylem's default (from = first parent)
}

func (r *run) diffPairs() ([]diffPair, error) {
	var pairs []diffPair
	for _, sha := range r.sampleCommits() {
		out, err := r.git.Git(r.ctx, "rev-list", "--parents", "-n1", sha)
		if err != nil {
			return nil, err
		}
		f := strings.Fields(string(out))
		from := emptyTree
		if len(f) > 1 {
			from = f[1]
		}
		pairs = append(pairs, diffPair{from: from, to: sha, firstParent: true})
	}
	for range 30 {
		pairs = append(pairs, diffPair{from: r.commits[r.intn(len(r.commits))], to: r.commits[r.intn(len(r.commits))]})
	}
	return pairs, nil
}

func (r *run) xylemDiff(p diffPair, hunks bool) ([]*xylemv1.FileDiff, error) {
	req := &xylemv1.DiffRequest{Repo: r.opts.Repo, ToRevision: rev(p.to), IncludeHunks: hunks}
	if !p.firstParent {
		req.FromRevision = rev(p.from)
	}
	stream, err := r.x.Repo.Diff(r.ctx, req)
	if err != nil {
		return nil, err
	}
	var files []*xylemv1.FileDiff
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		files = append(files, m.Files...)
	}
}

func compatible(git string, x xylemv1.FileDiff_Status) bool {
	switch git {
	case "A":
		return x == xylemv1.FileDiff_STATUS_ADDED
	case "D":
		return x == xylemv1.FileDiff_STATUS_DELETED
	default:
		return x == xylemv1.FileDiff_STATUS_MODIFIED || x == xylemv1.FileDiff_STATUS_MODE_CHANGED
	}
}

func (r *run) checkDiffNames() (Result, error) {
	var res Result
	pairs, err := r.diffPairs()
	if err != nil {
		return res, err
	}
	for _, p := range pairs {
		want, err := r.gitDiff(p.from, p.to, nil)
		if err != nil {
			return res, err
		}
		got, err := r.xylemDiff(p, false)
		if err != nil {
			return res, err
		}
		res.Compared++
		ok := len(got) == len(want)
		for _, f := range got {
			if c, found := want[string(f.Path)]; !found || !compatible(c.status, f.Status) {
				ok = false
				r.fail(&res, "%s..%s: %q is %v in Xylem, %q in git", short(p.from), short(p.to), f.Path, f.Status, c.status)
			}
		}
		if ok {
			res.Passed++
		} else if len(got) != len(want) {
			r.fail(&res, "%s..%s: %d files in Xylem, %d in git", short(p.from), short(p.to), len(got), len(want))
		}
	}
	return res, nil
}

// checkDiffContent applies Xylem's hunks to git's old file and expects git's
// new file (equivalent alignments are fine).
func (r *run) checkDiffContent() (Result, error) {
	var res Result
	pairs, err := r.diffPairs()
	if err != nil {
		return res, err
	}
	for _, p := range pairs {
		changes, err := r.gitDiff(p.from, p.to, nil)
		if err != nil {
			return res, err
		}
		files, err := r.xylemDiff(p, true)
		if err != nil {
			return res, err
		}
		for _, f := range files {
			c, ok := changes[string(f.Path)]
			if !ok || f.Binary || f.Status == xylemv1.FileDiff_STATUS_MODE_CHANGED {
				continue
			}
			var old, newC []byte
			if !c.oldSHA.IsZero() && c.status != "A" {
				if old, err = r.git.Blob(r.ctx, c.oldSHA); err != nil {
					return res, err
				}
			}
			if !c.newSHA.IsZero() && c.status != "D" {
				if newC, err = r.git.Blob(r.ctx, c.newSHA); err != nil {
					return res, err
				}
			}
			var hunks []diff.Hunk
			for _, h := range f.Hunks {
				hunks = append(hunks, diff.Hunk{OldStart: int(h.OldStart), OldLines: int(h.OldLines), NewStart: int(h.NewStart), NewLines: int(h.NewLines), Body: h.Body})
			}
			res.Compared++
			if got, err := diff.Apply(old, hunks); err == nil && bytes.Equal(got, newC) {
				res.Passed++
			} else {
				r.fail(&res, "%s..%s: hunks for %q do not reproduce git's file (%v)", short(p.from), short(p.to), f.Path, err)
			}
		}
	}
	return res, nil
}

// ---- blame

var porcelainHeader = regexp.MustCompile(`^([0-9a-f]{40}) (\d+) (\d+)`)

type blameOrigin struct {
	sha  string
	line int
	file string
}

func (r *run) gitBlame(sha, path string) ([]blameOrigin, error) {
	out, err := r.lit("blame", "--first-parent", "--line-porcelain", sha, "--", path)
	if err != nil {
		return nil, err
	}
	var lines []blameOrigin
	for _, l := range strings.Split(string(out), "\n") {
		if m := porcelainHeader.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			lines = append(lines, blameOrigin{sha: m[1], line: n})
		} else if name, ok := strings.CutPrefix(l, "filename "); ok && len(lines) > 0 {
			if strings.HasPrefix(name, `"`) {
				if unq, err := strconv.Unquote(name); err == nil {
					name = unq // git C-quotes unusual names
				}
			}
			lines[len(lines)-1].file = name
		}
	}
	return lines, nil
}

// textFiles returns files at sha that are text (no NUL in the first 8 KiB)
// and at most 1 MiB.
func (r *run) textFiles(sha string) ([]string, error) {
	files, err := r.gitTree(sha)
	if err != nil {
		return nil, err
	}
	var out []string
	for p, f := range files {
		if f.size > 1<<20 {
			continue
		}
		data, err := r.git.Blob(r.ctx, f.sha)
		if err != nil {
			return nil, err
		}
		if !diff.IsBinary(data) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

// checkBlame compares blame line by line. Lines git attributes to another
// file name (git follows whole-file renames, Xylem does not) are skipped.
func (r *run) checkBlame() (Result, error) {
	res := Result{Threshold: r.opts.BlameThreshold}
	type target struct{ sha, path string }
	var targets []target
	tipFiles, err := r.textFiles(r.tip)
	if err != nil {
		return res, err
	}
	for _, p := range sample(r, tipFiles, 40) {
		targets = append(targets, target{r.tip, p})
	}
	for range 10 {
		sha := r.commits[r.intn(len(r.commits))]
		files, err := r.textFiles(sha)
		if err != nil {
			return res, err
		}
		if len(files) > 0 {
			targets = append(targets, target{sha, files[r.intn(len(files))]})
		}
	}
	for _, t := range targets {
		want, err := r.gitBlame(t.sha, t.path)
		if err != nil {
			return res, err
		}
		got, err := r.x.Repo.Blame(r.ctx, &xylemv1.BlameRequest{Repo: r.opts.Repo, Revision: rev(t.sha), Path: []byte(t.path)})
		if err != nil {
			return res, err
		}
		if len(got.Lines) != len(want) {
			r.fail(&res, "%s:%q: %d lines in Xylem, %d in git", short(t.sha), t.path, len(got.Lines), len(want))
			res.Compared += len(want)
			continue
		}
		gitSHAOf := map[string]string{}
		for _, o := range got.Origins {
			gitSHAOf[hexID(o.Id)] = o.GitSha
		}
		for i, l := range got.Lines {
			if want[i].file != t.path {
				res.Skipped++
				continue
			}
			res.Compared++
			if gitSHAOf[hexID(l.Origin)] == want[i].sha && int(l.OriginLine) == want[i].line {
				res.Passed++
			} else {
				r.fail(&res, "%s:%q line %d: Xylem %s:%d, git %s:%d", short(t.sha), t.path, i+1, short(gitSHAOf[hexID(l.Origin)]), l.OriginLine, short(want[i].sha), want[i].line)
			}
		}
	}
	return res, nil
}

// ---- ancestry

func (r *run) pair() (string, string) {
	return r.commits[r.intn(len(r.commits))], r.commits[r.intn(len(r.commits))]
}

func (r *run) checkMergeBase() (Result, error) {
	var res Result
	for range 200 {
		a, b := r.pair()
		out, code, err := r.git.GitStatus(r.ctx, "merge-base", "--all", a, b)
		if err != nil {
			return res, err
		}
		want := strings.Fields(string(out))
		if code != 0 && code != 1 {
			return res, errors.New("git merge-base failed")
		}
		resp, err := r.x.Repo.MergeBase(r.ctx, &xylemv1.MergeBaseRequest{Repo: r.opts.Repo, A: rev(a), B: rev(b)})
		if err != nil {
			return res, err
		}
		var got []string
		if len(resp.Ids) > 0 {
			cs, err := r.x.Repo.GetChangesets(r.ctx, &xylemv1.GetChangesetsRequest{Repo: r.opts.Repo, Ids: resp.Ids})
			if err != nil {
				return res, err
			}
			for _, c := range cs.Changesets {
				got = append(got, c.GitSha)
			}
		}
		slices.Sort(got)
		slices.Sort(want)
		res.Compared++
		if slices.Equal(got, want) {
			res.Passed++
		} else {
			r.fail(&res, "merge-base %s %s: Xylem %v, git %v", short(a), short(b), got, want)
		}
	}
	return res, nil
}

func (r *run) checkIsAncestor() (Result, error) {
	var res Result
	for range 500 {
		a, b := r.pair()
		_, code, err := r.git.GitStatus(r.ctx, "merge-base", "--is-ancestor", a, b)
		if err != nil {
			return res, err
		}
		if code > 1 {
			return res, errors.New("git merge-base --is-ancestor failed")
		}
		resp, err := r.x.Repo.IsAncestor(r.ctx, &xylemv1.IsAncestorRequest{Repo: r.opts.Repo, Ancestor: rev(a), Descendant: rev(b)})
		if err != nil {
			return res, err
		}
		res.Compared++
		if resp.Result == (code == 0) {
			res.Passed++
		} else {
			r.fail(&res, "is-ancestor %s %s: Xylem %v, git %v", short(a), short(b), resp.Result, code == 0)
		}
	}
	return res, nil
}

// ---- read file

func (r *run) checkReadFile() (Result, error) {
	var res Result
	for range 200 {
		sha := r.commits[r.intn(len(r.commits))]
		files, err := r.gitTree(sha)
		if err != nil {
			return res, err
		}
		if len(files) == 0 {
			continue
		}
		names := slices.Sorted(func(yield func(string) bool) {
			for p := range files {
				if !yield(p) {
					return
				}
			}
		})
		p := names[r.intn(len(names))]
		want, err := r.git.Blob(r.ctx, files[p].sha)
		if err != nil {
			return res, err
		}
		stream, err := r.x.Repo.ReadFile(r.ctx, &xylemv1.ReadFileRequest{Repo: r.opts.Repo, Revision: rev(sha), Path: []byte(p)})
		if err != nil {
			return res, err
		}
		var got []byte
		var mode xylemv1.FileMode
		for {
			m, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return res, err
			}
			if mode == 0 {
				mode = m.Mode
			}
			got = append(got, m.Chunk...)
		}
		res.Compared++
		if bytes.Equal(got, want) && mode == gitToXylemMode[files[p].mode] {
			res.Passed++
		} else {
			r.fail(&res, "%s:%q: %d bytes (mode %v) in Xylem, %d in git", short(sha), p, len(got), mode, len(want))
		}
	}
	return res, nil
}
