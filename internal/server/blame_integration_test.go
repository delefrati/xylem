//go:build integration

package server_test

import (
	"context"
	"encoding/hex"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/testutil"
)

var porcelainHeader = regexp.MustCompile(`^([0-9a-f]{40}) (\d+) (\d+)`)

type origin struct {
	sha  string
	line int
	file string // the path in the origin commit
}

// gitBlame returns, per final line, the origin commit, line and file name
// from `git blame --first-parent --line-porcelain`.
func gitBlame(t *testing.T, dir, commit, path string) []origin {
	t.Helper()
	out := testutil.Git(t, dir, "blame", "--first-parent", "--line-porcelain", commit, "--", path)
	var lines []origin
	for _, l := range strings.Split(out, "\n") {
		if m := porcelainHeader.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			lines = append(lines, origin{sha: m[1], line: n})
		} else if name, ok := strings.CutPrefix(l, "filename "); ok && len(lines) > 0 {
			if unq, err := strconv.Unquote(name); err == nil && strings.HasPrefix(name, "\"") {
				name = unq // git C-quotes unusual names, e.g. "latin1-\351.txt"
			}
			lines[len(lines)-1].file = name
		}
	}
	return lines
}

// blameMatch compares Xylem's blame with git's. Lines git attributes to a
// different file name are skipped: git blame follows whole-file renames and
// Xylem does not (04 §Kind 4). It returns matching, compared and skipped lines.
func blameMatch(t *testing.T, c *clients, dir, commit, path string, byID map[hash.ID]string) (match, compared, renamed int) {
	t.Helper()
	resp, err := c.Repo.Blame(context.Background(), &xylemv1.BlameRequest{Revision: "git:" + commit, Path: []byte(path)})
	if err != nil {
		t.Fatalf("Blame(%s, %q): %v", commit[:8], path, err)
	}
	want := gitBlame(t, dir, commit, path)
	if len(resp.Lines) != len(want) {
		t.Fatalf("Blame(%s, %q): %d lines, git %d", commit[:8], path, len(resp.Lines), len(want))
	}
	for i, l := range resp.Lines {
		if want[i].file != path {
			renamed++
			continue
		}
		compared++
		var id hash.ID
		copy(id[:], l.Origin)
		if byID[id] == want[i].sha && int(l.OriginLine) == want[i].line {
			match++
		}
	}
	return match, compared, renamed
}

func setupBlame(t *testing.T, profile string) (*clients, string, map[hash.ID]string, *gitsource.Repo) {
	t.Helper()
	c := startServer(t)
	src := importFixture(t, c, profile)
	shaMap, err := c.Env.Repo.GitSHAMap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[hash.ID]string{}
	for sha, id := range shaMap {
		byID[id] = hex.EncodeToString(sha[:])
	}
	return c, testutil.GitFixture(t, profile), byID, src
}

// textFiles lists non-binary, non-submodule files at commit.
func textFiles(t *testing.T, src *gitsource.Repo, commit string) []string {
	t.Helper()
	sha, _ := gitsource.ParseSHA(commit)
	entries, err := src.LsTree(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Mode == gitsource.ModeSubmodule || e.Size > 1<<20 {
			continue
		}
		data, err := src.Blob(context.Background(), e.SHA)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data[:min(len(data), 8192)]), "\x00") {
			out = append(out, string(e.Path))
		}
	}
	return out
}

// tiny and edge: 100% of lines, every text file at every commit.
func TestBlameExactOnTinyAndEdge(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"tiny", "edge"} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()
			c, dir, byID, src := setupBlame(t, profile)
			commits := strings.Fields(testutil.Git(t, dir, "rev-list", "main"))
			total, match, renamed := 0, 0, 0
			for _, commit := range commits {
				for _, p := range textFiles(t, src, commit) {
					m, n, r := blameMatch(t, c, dir, commit, p, byID)
					if m != n {
						t.Errorf("%s:%q: %d of %d lines match git", commit[:8], p, m, n)
					}
					match, total, renamed = match+m, total+n, renamed+r
				}
			}
			t.Logf("%s: %d/%d blame lines match git (%d lines skipped: git followed a rename)", profile, match, total, renamed)
		})
	}
}

// branchy: ≥ 99% of lines on sampled files; also cold vs warm latency.
func TestBlameBranchy(t *testing.T) {
	t.Parallel()
	c, dir, byID, src := setupBlame(t, "branchy")
	tip := testutil.Git(t, dir, "rev-parse", "main")
	files := textFiles(t, src, tip)
	sort.Strings(files)
	total, match := 0, 0
	var cold, warm []time.Duration
	timed := func(path string) time.Duration {
		start := time.Now()
		if _, err := c.Repo.Blame(context.Background(), &xylemv1.BlameRequest{Path: []byte(path)}); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	for i := 0; i < len(files); i += 9 {
		cold = append(cold, timed(files[i])) // first call: computes and stores the blame chain
		warm = append(warm, timed(files[i])) // repeat: served from stored values
		m, n, _ := blameMatch(t, c, dir, tip, files[i], byID)
		match, total = match+m, total+n
	}
	pct := 100 * float64(match) / float64(total)
	t.Logf("branchy: %d/%d blame lines match git (%.2f%%) over %d files", match, total, pct, len(cold))
	median := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	t.Logf("branchy Blame RPC latency: cold p50 %v (max %v), warm p50 %v (max %v)", median(cold), slices.Max(cold), median(warm), slices.Max(warm))
	if median(warm) > 50*time.Millisecond {
		t.Errorf("warm blame p50 %v exceeds 50 ms", median(warm))
	}
	if pct < 99 {
		t.Errorf("blame parity %.2f%% < 99%%", pct)
	}
}

func TestBlameErrors(t *testing.T) {
	t.Parallel()
	c, _, _, _ := setupBlame(t, "edge")
	ctx := context.Background()
	for path, code := range map[string]codes.Code{"big.bin": codes.FailedPrecondition, "dup": codes.InvalidArgument, "nope": codes.NotFound, "": codes.InvalidArgument} {
		if _, err := c.Repo.Blame(ctx, &xylemv1.BlameRequest{Path: []byte(path)}); status.Code(err) != code {
			t.Errorf("Blame(%q) = %v, want %v", path, err, code)
		}
	}
	resp, err := c.Repo.Blame(ctx, &xylemv1.BlameRequest{Path: []byte("README")})
	if err != nil || len(resp.Origins) == 0 || string(resp.Lines[0].Content) != "edge fixture" {
		t.Errorf("README blame = %v, %v", resp, err)
	}
}
