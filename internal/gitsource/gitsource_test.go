package gitsource_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/testutil"
)

func open(t *testing.T, profile string) (*gitsource.Repo, string) {
	t.Helper()
	dir := testutil.GitFixture(t, profile)
	r, err := gitsource.Open(context.Background(), dir, gitsource.Options{CatFileProcesses: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return r, dir
}

func sha(t *testing.T, s string) gitsource.SHA {
	t.Helper()
	v, err := gitsource.ParseSHA(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func revList(t *testing.T, r *gitsource.Repo, tip gitsource.SHA, exclude ...gitsource.SHA) []gitsource.SHA {
	t.Helper()
	var out []gitsource.SHA
	for s, err := range r.RevList(context.Background(), tip, exclude...) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	if _, err := gitsource.Open(context.Background(), t.TempDir(), gitsource.Options{}); err == nil {
		t.Error("Open on a non-repository succeeded")
	}
	sha256Repo := t.TempDir()
	testutil.Git(t, sha256Repo, "init", "-q", "--object-format=sha256")
	if _, err := gitsource.Open(context.Background(), sha256Repo, gitsource.Options{}); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("Open on a sha256 repository = %v", err)
	}
}

func TestRevListAndBranch(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"tiny", "edge"} {
		r, dir := open(t, profile)
		ctx := context.Background()
		tip, err := r.ResolveBranch(ctx, "main")
		if err != nil || tip.String() != testutil.Git(t, dir, "rev-parse", "main") {
			t.Fatalf("%s: ResolveBranch = %s, %v", profile, tip, err)
		}
		if _, err := r.ResolveBranch(ctx, "nope"); !errors.Is(err, gitsource.ErrNotFound) {
			t.Errorf("ResolveBranch(nope) = %v", err)
		}
		all := revList(t, r, tip)
		if want, _ := strconv.Atoi(testutil.Git(t, dir, "rev-list", "--count", "main")); len(all) != want {
			t.Fatalf("%s: RevList returned %d commits, want %d", profile, len(all), want)
		}
		// Parents come before children.
		pos := map[gitsource.SHA]int{}
		for i, c := range all {
			pos[c] = i
		}
		for i, c := range all {
			for _, p := range strings.Fields(testutil.Git(t, dir, "rev-list", "--parents", "-n1", c.String()))[1:] {
				if pos[sha(t, p)] >= i {
					t.Fatalf("%s: parent %s listed after child %s", profile, p, c)
				}
			}
		}
		// Excluding a commit lists only what came after it.
		mid := all[len(all)/2]
		rest := revList(t, r, tip, mid)
		if want, _ := strconv.Atoi(testutil.Git(t, dir, "rev-list", "--count", "main", "^"+mid.String())); len(rest) != want {
			t.Errorf("%s: RevList excluding %s = %d, want %d", profile, mid, len(rest), want)
		}
		// Breaking early stops cleanly.
		n := 0
		for range r.RevList(ctx, tip) {
			n++
			if n == 2 {
				break
			}
		}
	}
}

func TestReadCommitMatchesGit(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"tiny", "edge"} {
		r, dir := open(t, profile)
		ctx := context.Background()
		tip, _ := r.ResolveBranch(ctx, "main")
		for _, c := range revList(t, r, tip) {
			got, err := r.ReadCommit(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			// %B is the raw message for commits without an encoding header.
			f := strings.Split(testutil.Git(t, dir, "log", "-1", "--format=%T%x00%P%x00%an <%ae>%x00%at%x00%ad%x00%cn <%ce>%x00%ct%x00%cd", "--date=format:%z", c.String()), "\x00")
			if got.Tree.String() != f[0] {
				t.Errorf("%s tree %s, git %s", c, got.Tree, f[0])
			}
			var parents []string
			for _, p := range got.Parents {
				parents = append(parents, p.String())
			}
			if strings.Join(parents, " ") != f[1] {
				t.Errorf("%s parents %v, git %q", c, parents, f[1])
			}
			check := func(who string, id gitsource.Ident, name, unix, tz string) {
				if string(id.NameEmail) != name || strconv.FormatInt(id.Time, 10) != unix || fmtTZ(id.TZMinutes) != tz {
					t.Errorf("%s %s = %q %d %d, git %q %s %s", c, who, id.NameEmail, id.Time, id.TZMinutes, name, unix, tz)
				}
			}
			check("author", got.Author, f[2], f[3], f[4])
			check("committer", got.Committer, f[5], f[6], f[7])
		}
	}
}

func fmtTZ(m int32) string {
	sign := '+'
	if m < 0 {
		sign, m = '-', -m
	}
	return fmt.Sprintf("%c%02d%02d", sign, m/60, m%60)
}

func findCommit(t *testing.T, dir, subject string) gitsource.SHA {
	t.Helper()
	return sha(t, testutil.Git(t, dir, "log", "--format=%H", "--fixed-strings", "--grep", subject, "-n1", "main"))
}

func TestEdgeCommits(t *testing.T) {
	t.Parallel()
	r, dir := open(t, "edge")
	ctx := context.Background()

	enc, err := r.ReadCommit(ctx, findCommit(t, dir, "ISO-8859-1 message body"))
	if err != nil {
		t.Fatal(err)
	}
	if enc.Encoding != "ISO-8859-1" || !bytes.HasPrefix(enc.Message, []byte("caf\xe9 au lait\n")) {
		t.Errorf("encoding commit: encoding %q, message %q", enc.Encoding, enc.Message)
	}
	octopus, _ := r.ReadCommit(ctx, findCommit(t, dir, "octopus merge"))
	if len(octopus.Parents) != 4 {
		t.Errorf("octopus has %d parents, want 4", len(octopus.Parents))
	}
	tz, _ := r.ReadCommit(ctx, findCommit(t, dir, "root: assorted paths"))
	if tz.Author.TZMinutes != 330 || len(tz.Parents) != 0 {
		t.Errorf("root commit tz %d, %d parents", tz.Author.TZMinutes, len(tz.Parents))
	}
}

func diff(t *testing.T, r *gitsource.Repo, dir, subject string) []gitsource.Change {
	t.Helper()
	c, err := r.ReadCommit(context.Background(), findCommit(t, dir, subject))
	if err != nil {
		t.Fatal(err)
	}
	var parent *gitsource.SHA
	if len(c.Parents) > 0 {
		parent = &c.Parents[0]
	}
	changes, err := r.DiffTree(context.Background(), parent, c.SHA)
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func byPath(changes []gitsource.Change) map[string]gitsource.Change {
	m := map[string]gitsource.Change{}
	for _, c := range changes {
		m[string(c.Path)] = c
	}
	return m
}

func TestDiffTreeEdgeCases(t *testing.T) {
	t.Parallel()
	r, dir := open(t, "edge")

	root := byPath(diff(t, r, dir, "root: assorted paths"))
	for _, p := range []string{"latin1-\xe9.txt", "new\nline.txt", "ünïcødé/ファイル.txt", "path with spaces/file one.txt", "empty.txt"} {
		if c, ok := root[p]; !ok || c.Status != 'A' || c.NewMode != gitsource.ModeFile {
			t.Errorf("root diff entry %q = %+v, %v", p, c, ok)
		}
	}
	if c := root["run.sh"]; c.NewMode != gitsource.ModeExec {
		t.Errorf("run.sh mode %o", c.NewMode)
	}
	if c := root["empty.txt"]; c.NewSHA.String() != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Errorf("empty blob sha %s", c.NewSHA)
	}

	flip := byPath(diff(t, r, dir, "exec bit off"))["run.sh"]
	if flip.Status != 'M' || flip.OldMode != gitsource.ModeExec || flip.NewMode != gitsource.ModeFile || flip.OldSHA != flip.NewSHA {
		t.Errorf("exec flip = %+v", flip)
	}
	if c := byPath(diff(t, r, dir, "x: file -> symlink"))["x"]; c.Status != 'T' || c.NewMode != gitsource.ModeSymlink {
		t.Errorf("file -> symlink = %+v", c)
	}
	dirChange := byPath(diff(t, r, dir, "x: symlink -> directory"))
	if dirChange["x"].Status != 'D' || dirChange["x/inner.txt"].Status != 'A' {
		t.Errorf("symlink -> directory = %v", dirChange)
	}
	if c := byPath(diff(t, r, dir, "add submodule gitlink"))["vendor/sub"]; c.Status != 'A' || c.NewMode != gitsource.ModeSubmodule {
		t.Errorf("submodule add = %+v", c)
	}
	rename := byPath(diff(t, r, dir, "rename (no rename detection"))
	if rename["dup/a.txt"].Status != 'D' || rename["renamed.txt"].Status != 'A' {
		t.Errorf("rename without detection = %v", rename)
	}
	if empty := diff(t, r, dir, "empty commit"); len(empty) != 0 {
		t.Errorf("empty commit diff = %v", empty)
	}
}

// Every DiffTree result must equal git's own name-status listing.
func TestDiffTreeMatchesGit(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"tiny", "edge"} {
		r, dir := open(t, profile)
		ctx := context.Background()
		tip, _ := r.ResolveBranch(ctx, "main")
		for _, c := range revList(t, r, tip) {
			commit, _ := r.ReadCommit(ctx, c)
			var parent *gitsource.SHA
			args := []string{"diff-tree", "-r", "-z", "--name-status", "--no-renames", "--no-commit-id"}
			if len(commit.Parents) > 0 {
				parent = &commit.Parents[0]
				args = append(args, parent.String(), c.String())
			} else {
				args = append(args, "--root", c.String())
			}
			changes, err := r.DiffTree(ctx, parent, c)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, ch := range changes {
				got = append(got, string(ch.Status), string(ch.Path))
			}
			want := strings.Split(strings.TrimSuffix(testutil.Git(t, dir, args...), "\x00"), "\x00")
			if len(want) == 1 && want[0] == "" {
				want = nil
			}
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("%s %s: DiffTree differs from git", profile, c)
			}
		}
	}
}

func TestCatFile(t *testing.T) {
	t.Parallel()
	r, dir := open(t, "edge")
	ctx := context.Background()
	tip, _ := r.ResolveBranch(ctx, "main")
	entries, err := r.LsTree(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	var big gitsource.TreeEntry
	for _, e := range entries {
		if string(e.Path) == "big.bin" {
			big = e
		}
	}
	data, err := r.Blob(ctx, big.SHA)
	if err != nil || int64(len(data)) != big.Size || big.Size != 11534336 {
		t.Fatalf("big.bin: %d bytes (ls-tree %d), %v", len(data), big.Size, err)
	}
	if _, err := r.Blob(ctx, sha(t, strings.Repeat("ab", 20))); !errors.Is(err, gitsource.ErrNotFound) {
		t.Errorf("missing object: %v", err)
	}
	if _, err := r.Blob(ctx, tip); err == nil {
		t.Error("Blob of a commit succeeded")
	}
	// Many concurrent readers over 3 processes; compare with git cat-file.
	var wg sync.WaitGroup
	for g := range 64 {
		wg.Go(func() {
			e := entries[g%len(entries)]
			if e.Mode == gitsource.ModeSubmodule {
				return
			}
			got, err := r.Blob(ctx, e.SHA)
			if err != nil {
				t.Errorf("Blob(%s): %v", e.Path, err)
				return
			}
			if int64(len(got)) != e.Size {
				t.Errorf("Blob(%q) = %d bytes, ls-tree says %d", e.Path, len(got), e.Size)
			}
		})
	}
	wg.Wait()
	if want := testutil.Git(t, dir, "cat-file", "blob", "main:README"); true {
		readme, _ := r.Blob(ctx, findEntry(t, entries, "README").SHA)
		if strings.TrimSpace(string(readme)) != want {
			t.Errorf("README = %q, git %q", readme, want)
		}
	}
}

func findEntry(t *testing.T, entries []gitsource.TreeEntry, path string) gitsource.TreeEntry {
	t.Helper()
	for _, e := range entries {
		if string(e.Path) == path {
			return e
		}
	}
	t.Fatalf("no entry %q", path)
	return gitsource.TreeEntry{}
}

func TestLsTreeMatchesGit(t *testing.T) {
	t.Parallel()
	r, dir := open(t, "edge")
	ctx := context.Background()
	tip, _ := r.ResolveBranch(ctx, "main")
	entries, err := r.LsTree(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Split(strings.TrimSuffix(testutil.Git(t, dir, "ls-tree", "-r", "-z", "--name-only", "--full-tree", "main"), "\x00"), "\x00")
	if len(entries) != len(names) {
		t.Fatalf("LsTree %d entries, git %d", len(entries), len(names))
	}
	for i, e := range entries {
		if string(e.Path) != names[i] {
			t.Errorf("entry %d path %q, git %q", i, e.Path, names[i])
		}
	}
	if e := findEntry(t, entries, "x"); e.Mode != gitsource.ModeFile {
		t.Errorf("x mode %o", e.Mode)
	}
}

func TestParseCommitMalformed(t *testing.T) {
	t.Parallel()
	good := "tree " + strings.Repeat("a", 40) + "\nauthor A <a> 1 +0000\ncommitter C <c> 2 -0130\n\nmsg\n"
	c, err := gitsource.ParseCommit([]byte(good))
	if err != nil || c.Committer.TZMinutes != -90 || string(c.Message) != "msg\n" {
		t.Fatalf("ParseCommit(good) = %+v, %v", c, err)
	}
	signed := strings.Replace(good, "\n\n", "\ngpgsig -----BEGIN-----\n \n line\n -----END-----\n\n", 1)
	if c, err := gitsource.ParseCommit([]byte(signed)); err != nil || string(c.Message) != "msg\n" {
		t.Errorf("signed commit: %+v, %v", c, err)
	}
	noMessage := strings.TrimSuffix(good, "\nmsg\n")
	if c, err := gitsource.ParseCommit([]byte(noMessage)); err != nil || len(c.Message) != 0 {
		t.Errorf("no message: %+v, %v", c, err)
	}
	for name, bad := range map[string]string{
		"no tree":    "author A <a> 1 +0000\ncommitter C <c> 2 +0000\n\nm",
		"bad parent": good + "parent xyz\n",
		"no email":   strings.Replace(good, "A <a>", "A a", 1),
		"bad time":   strings.Replace(good, "<a> 1 ", "<a> x ", 1),
		"bad tz":     strings.Replace(good, "-0130", "0130", 1),
		"no tz":      strings.Replace(good, " -0130", "", 1),
	} {
		if name == "bad parent" {
			bad = strings.Replace(good, "author", "parent xyz\nauthor", 1)
		}
		if _, err := gitsource.ParseCommit([]byte(bad)); err == nil {
			t.Errorf("%s: ParseCommit succeeded", name)
		}
	}
}

func TestAncestryHelpers(t *testing.T) {
	t.Parallel()
	r, dir := open(t, "edge")
	ctx := context.Background()
	tip, _ := r.ResolveBranch(ctx, "main")
	root := findCommit(t, dir, "root: assorted paths")
	side1 := findCommit(t, dir, "side: 1")
	o1 := findCommit(t, dir, "octopus branch o1")
	for _, tt := range []struct {
		a, b gitsource.SHA
		want bool
	}{{root, tip, true}, {tip, root, false}, {tip, tip, true}, {o1, tip, true}} {
		if got, err := r.IsAncestor(ctx, tt.a, tt.b); err != nil || got != tt.want {
			t.Errorf("IsAncestor(%s, %s) = %v, %v", tt.a, tt.b, got, err)
		}
	}
	chain, err := r.FirstParentChain(ctx, tip)
	if err != nil {
		t.Fatal(err)
	}
	// main fast-forwarded to the side branch's merge, so side commits are on
	// the first-parent chain; octopus branches are not.
	if !chain[tip] || !chain[root] || !chain[side1] || chain[o1] {
		t.Errorf("first-parent chain: tip %v root %v side1 %v o1 %v", chain[tip], chain[root], chain[side1], chain[o1])
	}
	want, _ := strconv.Atoi(testutil.Git(t, dir, "rev-list", "--first-parent", "--count", "main"))
	if len(chain) != want {
		t.Errorf("chain has %d commits, git %d", len(chain), want)
	}
}
