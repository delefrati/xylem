// Command genrepo writes the deterministic "branchy" fixture repository
// (10-testing.md §Fixture repositories): about 2,000 commits editing ~300
// files, with feature branches merged back into main.
//
//	go run ./scripts/genrepo [-commits 2000] [-files 300] [-seed 1] <dir>
//
// It streams a `git fast-import` script, so generation takes seconds. Feature
// branches only edit files that main leaves alone while they are open, so
// every merge is clean and its tree is main's files plus the branch's edits.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"
)

const t0 = 1700000000

func main() {
	commits := flag.Int("commits", 2000, "approximate number of commits")
	files := flag.Int("files", 300, "number of files")
	seed := flag.Uint64("seed", 1, "random seed")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: genrepo [flags] <dir>")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *commits, *files, *seed); err != nil {
		fmt.Fprintln(os.Stderr, "genrepo:", err)
		os.Exit(1)
	}
}

func run(dir string, commits, files int, seed uint64) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := gitCmd("", "init", "-q", "-b", "main", dir).Run(); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	cmd := gitCmd(dir, "fast-import", "--quiet", "--done")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git fast-import: %w", err)
	}
	w := bufio.NewWriter(stdin)
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	g := newGenerator(w, &rng{rand.NewChaCha8(key)}, files)
	g.generate(commits)
	g.printf("done\n")
	if g.err != nil {
		return fmt.Errorf("writing fast-import stream: %w", g.err)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := stdin.Close(); err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git fast-import: %w", err)
	}
	if err := gitCmd(dir, "checkout", "-q", "-f", "main").Run(); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	out, err := gitCmd(dir, "rev-parse", "main").Output()
	if err != nil {
		return err
	}
	fmt.Printf("branchy: %d commits (%d merges), main=%s", g.marks, g.merges, out)
	return nil
}

// gitCmd runs git with the user's configuration ignored.
func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...) //nolint:gosec // G204: running git with computed arguments is the design (ADR-010)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	return cmd
}

type branchState struct {
	ref   string
	tip   int               // mark of the tip commit
	files map[string]string // path → content
}

// rng is a deterministic generator for fixture data.
type rng struct{ src *rand.ChaCha8 }

// IntN returns a value in [0, n) for small n (modulo bias is irrelevant here).
func (r *rng) IntN(n int) int { return int(r.src.Uint64()>>34) % n }

type generator struct {
	w      io.Writer
	rng    *rng
	paths  []string
	marks  int
	merges int
	clock  int
	words  []string
	err    error // first write error
}

// printf writes to the fast-import stream, keeping the first error.
func (g *generator) printf(format string, args ...any) {
	if g.err == nil {
		_, g.err = fmt.Fprintf(g.w, format, args...)
	}
}

func newGenerator(w io.Writer, r *rng, files int) *generator {
	g := &generator{w: w, rng: r}
	dirs := []string{"cmd/app", "cmd/tool", "internal/core", "internal/util", "internal/api", "pkg/client", "pkg/server", "docs", "scripts", "test"}
	for i := range files {
		d := dirs[i%len(dirs)]
		g.paths = append(g.paths, fmt.Sprintf("%s/file%03d.txt", d, i))
	}
	g.words = strings.Fields("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon phi chi psi omega")
	return g
}

func (g *generator) line() string {
	n := 3 + g.rng.IntN(8)
	ws := make([]string, n)
	for i := range ws {
		ws[i] = g.words[g.rng.IntN(len(g.words))]
	}
	return strings.Join(ws, " ")
}

func (g *generator) newFile() string {
	var b strings.Builder
	for range 10 + g.rng.IntN(60) {
		b.WriteString(g.line())
		b.WriteByte('\n')
	}
	return b.String()
}

// edit changes a few lines: modify, insert or delete, so blame has history.
func (g *generator) edit(content string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for range 1 + g.rng.IntN(4) {
		i := g.rng.IntN(len(lines))
		switch g.rng.IntN(3) {
		case 0:
			lines[i] = g.line()
		case 1:
			lines = slices.Insert(lines, i, g.line())
		default:
			if len(lines) > 3 {
				lines = slices.Delete(lines, i, i+1)
			}
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

var authors = []string{"Ada Lovelace <ada@example.com>", "Grace Hopper <grace@example.com>", "Alan Turing <alan@example.com>", "Edsger Dijkstra <edsger@example.com>"}

// commit writes one commit on b. parents beyond b.tip are merged in. changes
// maps paths to new contents ("" deletes).
func (g *generator) commit(b *branchState, msg string, merge []int, changes map[string]string) {
	g.marks++
	g.clock += 60 + g.rng.IntN(600)
	author := authors[g.rng.IntN(len(authors))]
	g.printf("commit refs/heads/%s\nmark :%d\n", b.ref, g.marks)
	g.printf("author %s %d +0000\ncommitter %s %d +0000\n", author, t0+g.clock, author, t0+g.clock)
	g.printf("data %d\n%s\n", len(msg), msg)
	if b.tip > 0 {
		g.printf("from :%d\n", b.tip)
	}
	for _, m := range merge {
		g.printf("merge :%d\n", m)
	}
	for _, p := range slices.Sorted(maps.Keys(changes)) {
		if c := changes[p]; c == "" {
			g.printf("D %s\n", p)
			delete(b.files, p)
		} else {
			g.printf("M 100644 inline %s\ndata %d\n%s\n", p, len(c), c)
			b.files[p] = c
		}
	}
	g.printf("\n")
	b.tip = g.marks
}

// randomChanges edits, adds or deletes 1–5 files from allowed.
func (g *generator) randomChanges(b *branchState, allowed []string) map[string]string {
	changes := map[string]string{}
	for range 1 + g.rng.IntN(5) {
		p := allowed[g.rng.IntN(len(allowed))]
		cur, exists := b.files[p]
		switch {
		case !exists:
			changes[p] = g.newFile()
		case g.rng.IntN(25) == 0 && len(b.files) > len(g.paths)/2:
			changes[p] = "" // delete
		default:
			if prev, ok := changes[p]; ok && prev != "" {
				cur = prev
			}
			changes[p] = g.edit(cur)
		}
	}
	for p, c := range changes { // a delete that isn't a delete, or a no-op edit
		if old, ok := b.files[p]; (c == "" && !ok) || c == old {
			delete(changes, p)
		}
	}
	if len(changes) == 0 {
		p := allowed[0]
		if cur, ok := b.files[p]; ok {
			changes[p] = cur + g.line() + "\n"
		} else {
			changes[p] = g.newFile()
		}
	}
	return changes
}

func (g *generator) generate(total int) {
	main := &branchState{ref: "main", files: map[string]string{}}
	initial := map[string]string{}
	for _, p := range g.paths[:len(g.paths)*2/3] {
		initial[p] = g.newFile()
	}
	g.commit(main, "initial import", nil, initial)

	type feature struct {
		b        *branchState
		reserved map[string]bool
		left     int
	}
	var open []*feature
	reserved := map[string]bool{}
	featureN := 0
	for g.marks < total {
		switch {
		case len(open) < 3 && g.rng.IntN(12) == 0: // open a feature branch
			featureN++
			f := &feature{
				b:        &branchState{ref: fmt.Sprintf("feature/%03d", featureN), tip: main.tip, files: maps.Clone(main.files)},
				reserved: map[string]bool{}, left: 2 + g.rng.IntN(10),
			}
			for range 3 + g.rng.IntN(6) {
				p := g.paths[g.rng.IntN(len(g.paths))]
				if !reserved[p] {
					f.reserved[p], reserved[p] = true, true
				}
			}
			if len(f.reserved) == 0 {
				continue
			}
			open = append(open, f)
		case len(open) > 0 && g.rng.IntN(3) == 0: // work on a feature, or merge it
			i := g.rng.IntN(len(open))
			f := open[i]
			if f.left > 0 {
				f.left--
				g.commit(f.b, fmt.Sprintf("%s: work", f.b.ref), nil, g.randomChanges(f.b, slices.Sorted(maps.Keys(f.reserved))))
				continue
			}
			// Merge: main's files plus the feature's reserved files.
			changes := map[string]string{}
			for p := range f.reserved {
				fc, fok := f.b.files[p]
				mc, mok := main.files[p]
				switch {
				case fok && (!mok || fc != mc):
					changes[p] = fc
				case !fok && mok:
					changes[p] = ""
				}
				delete(reserved, p)
			}
			g.commit(main, fmt.Sprintf("Merge branch '%s'", f.b.ref), []int{f.b.tip}, changes)
			g.merges++
			open = slices.Delete(open, i, i+1)
		default: // a commit on main, avoiding files reserved by open features
			var allowed []string
			for _, p := range g.paths {
				if !reserved[p] {
					allowed = append(allowed, p)
				}
			}
			g.commit(main, fmt.Sprintf("main: change %d", g.marks+1), nil, g.randomChanges(main, allowed))
		}
	}
}
