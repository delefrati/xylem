// Package parity checks a Xylem server's answers against git on the same
// repository (10-testing.md §Parity suite). It talks to Xylem over gRPC and
// to git through the git binary, so it exercises the real API end to end.
package parity

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/gitsource"
)

// Check names, in report order.
var AllChecks = []string{"tree", "log", "log-path", "diff-names", "diff-content", "blame", "merge-base", "is-ancestor", "read-file"}

// Options configures a parity run.
type Options struct {
	Repo   string   // Xylem repository (default "default")
	Branch string   // git branch that was imported (default "main")
	Seed   uint64   // sampling seed (default 42)
	Checks []string // subset of AllChecks (default all)
	// Commits is how many commits the per-commit checks sample (tree,
	// diff-names, diff-content); 0 means all of them.
	Commits int
	// BlameThreshold is the minimum % of comparable blame lines that must
	// match (default 99). Everything else must match 100%.
	BlameThreshold float64
	// MaxFailures bounds the examples kept per check (default 20).
	MaxFailures int
}

// Result is one check's outcome.
type Result struct {
	Check     string   `json:"check"`
	Compared  int      `json:"compared"`
	Passed    int      `json:"passed"`
	Skipped   int      `json:"skipped,omitempty"` // e.g. blame lines git attributed through a rename
	Percent   float64  `json:"percent"`
	Threshold float64  `json:"threshold"`
	Pass      bool     `json:"pass"`
	Failures  []string `json:"failures,omitempty"`
}

// Report is a whole parity run.
type Report struct {
	Repo    string   `json:"repo"`
	GitDir  string   `json:"git_dir"`
	Seed    uint64   `json:"seed"`
	Results []Result `json:"results"`
}

// Pass reports whether every check passed.
func (r *Report) Pass() bool {
	for _, res := range r.Results {
		if !res.Pass {
			return false
		}
	}
	return true
}

// Print writes the report as a table.
func (r *Report) Print(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	// tabwriter buffers; write errors surface at Flush.
	_, _ = fmt.Fprintf(tw, "CHECK\tCOMPARED\tPASSED\tSKIPPED\tPERCENT\tNEEDED\tRESULT\n")
	for _, res := range r.Results {
		verdict := "PASS"
		if !res.Pass {
			verdict = "FAIL"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%.2f%%\t%.0f%%\t%s\n", res.Check, res.Compared, res.Passed, res.Skipped, res.Percent, res.Threshold, verdict)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, res := range r.Results {
		for _, f := range res.Failures {
			if _, err := fmt.Fprintf(w, "  %s: %s\n", res.Check, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteJSON writes the report to path (parity-report.json).
func (r *Report) WriteJSON(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Clients are the Xylem services the suite talks to.
type Clients struct {
	Repo   xylemv1.RepoServiceClient
	Object xylemv1.ObjectServiceClient
}

// run holds the state of one parity run.
type run struct {
	ctx     context.Context
	git     *gitsource.Repo
	x       Clients
	opts    Options
	rng     *rand.ChaCha8
	commits []string // rev-list of the branch, newest first
	tip     string
	blobIDs map[gitsource.SHA][]byte // git blob → Xylem blob id (re-hashed)
}

// Run executes the selected checks.
func Run(ctx context.Context, git *gitsource.Repo, x Clients, opts Options) (*Report, error) {
	if opts.Repo == "" {
		opts.Repo = "default"
	}
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	if opts.Seed == 0 {
		opts.Seed = 42
	}
	if len(opts.Checks) == 0 {
		opts.Checks = AllChecks
	}
	if opts.BlameThreshold == 0 {
		opts.BlameThreshold = 99
	}
	if opts.MaxFailures == 0 {
		opts.MaxFailures = 20
	}
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:8], opts.Seed)
	r := &run{ctx: ctx, git: git, x: x, opts: opts, rng: rand.NewChaCha8(seed), blobIDs: map[gitsource.SHA][]byte{}}
	out, err := git.Git(ctx, "rev-list", "refs/heads/"+opts.Branch)
	if err != nil {
		return nil, err
	}
	r.commits = strings.Fields(string(out))
	if len(r.commits) == 0 {
		return nil, fmt.Errorf("branch %s has no commits", opts.Branch)
	}
	r.tip = r.commits[0]

	report := &Report{Repo: opts.Repo, GitDir: git.Dir(), Seed: opts.Seed}
	checks := map[string]func() (Result, error){
		"tree": r.checkTree, "log": r.checkLog, "log-path": r.checkLogPath,
		"diff-names": r.checkDiffNames, "diff-content": r.checkDiffContent, "blame": r.checkBlame,
		"merge-base": r.checkMergeBase, "is-ancestor": r.checkIsAncestor, "read-file": r.checkReadFile,
	}
	for _, name := range AllChecks {
		if !slices.Contains(opts.Checks, name) {
			continue
		}
		res, err := checks[name]()
		if err != nil {
			return nil, fmt.Errorf("parity check %s: %w", name, err)
		}
		res.Check = name
		if res.Threshold == 0 {
			res.Threshold = 100
		}
		if res.Compared > 0 {
			res.Percent = 100 * float64(res.Passed) / float64(res.Compared)
		} else {
			res.Percent = 100
		}
		res.Pass = res.Percent >= res.Threshold
		report.Results = append(report.Results, res)
	}
	for _, c := range opts.Checks {
		if checks[c] == nil {
			return nil, fmt.Errorf("unknown parity check %q (have %s)", c, strings.Join(AllChecks, ", "))
		}
	}
	return report, nil
}

// intn returns a deterministic value in [0, n).
func (r *run) intn(n int) int { return int(r.rng.Uint64()>>34) % n }

// sample returns k distinct items of xs (all of them if k ≤ 0 or ≥ len).
func sample[T any](r *run, xs []T, k int) []T {
	if k <= 0 || k >= len(xs) {
		return slices.Clone(xs)
	}
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	for i := len(idx) - 1; i > 0; i-- {
		j := r.intn(i + 1)
		idx[i], idx[j] = idx[j], idx[i]
	}
	idx = idx[:k]
	slices.Sort(idx)
	out := make([]T, k)
	for i, j := range idx {
		out[i] = xs[j]
	}
	return out
}

func (r *run) fail(res *Result, format string, args ...any) {
	if len(res.Failures) < r.opts.MaxFailures {
		res.Failures = append(res.Failures, fmt.Sprintf(format, args...))
	}
}

func short(sha string) string { return sha[:min(len(sha), 10)] }

func hexID(b []byte) string { return hex.EncodeToString(b) }
