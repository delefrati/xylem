package importer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"

	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
)

// VerifyOptions selects the commits VerifyImport checks.
type VerifyOptions struct {
	Branch        string // default "main"
	Sample        int    // check this many random commits; 0 checks all
	Seed          uint64 // sample seed (deterministic)
	MaxViolations int    // default 100
}

// VerifyReport is the result of VerifyImport.
type VerifyReport struct {
	Commits    int // commits compared
	Files      int // file entries compared
	Violations []string
}

// VerifyImport compares imported changesets with git, tree for tree
// (08 §Verification): for each selected commit, `git ls-tree -r -l`
// (submodules excluded) must equal a walk of the changeset's root tree in
// paths, modes and sizes, and every blob must hold the same content (via the
// git_blobs table, or by re-hashing the git blob when no mapping exists). It
// also checks that every commit is imported and main is at the mapped tip.
func VerifyImport(ctx context.Context, r *repo.Repo, src *gitsource.Repo, opts VerifyOptions) (*VerifyReport, error) {
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	if opts.MaxViolations <= 0 {
		opts.MaxViolations = 100
	}
	rep := &VerifyReport{}
	fail := func(format string, args ...any) bool {
		rep.Violations = append(rep.Violations, fmt.Sprintf(format, args...))
		return len(rep.Violations) >= opts.MaxViolations
	}

	tip, err := src.ResolveBranch(ctx, opts.Branch)
	if err != nil {
		return nil, err
	}
	shaMap, err := r.GitSHAMap(ctx)
	if err != nil {
		return nil, err
	}
	var commits []gitsource.SHA
	for sha, err := range src.RevList(ctx, tip) {
		if err != nil {
			return nil, err
		}
		commits = append(commits, sha)
	}
	if main, err := r.GetRef(ctx, repo.MainRef); err != nil {
		fail("main: %v", err)
	} else if want, ok := shaMap[tip]; !ok || main.ID != want {
		fail("main is %s, want the mapped branch tip %s (git %s)", main.ID.Short(12), want.Short(12), tip)
	}

	if opts.Sample > 0 && opts.Sample < len(commits) {
		var seed [32]byte
		binary.LittleEndian.PutUint64(seed[:8], opts.Seed)
		src := rand.NewChaCha8(seed)
		perm := make([]gitsource.SHA, len(commits))
		copy(perm, commits)
		for i := len(perm) - 1; i > 0; i-- { // Fisher–Yates over a seeded stream
			j := int(src.Uint64()>>34) % (i + 1) // 30 random bits; commit counts are far below 2^30
			perm[i], perm[j] = perm[j], perm[i]
		}
		commits = perm[:opts.Sample]
	}

	v := &treeVerifier{r: r, src: src}
	for _, sha := range commits {
		id, ok := shaMap[sha]
		if !ok {
			if fail("commit %s is not imported", sha) {
				return rep, nil
			}
			continue
		}
		problems, files, err := v.commit(ctx, sha, id)
		if err != nil {
			return nil, err
		}
		rep.Commits++
		rep.Files += files
		for _, p := range problems {
			if fail("commit %s: %s", sha, p) {
				return rep, nil
			}
		}
	}
	return rep, nil
}

type treeVerifier struct {
	r   *repo.Repo
	src *gitsource.Repo
}

type xfile struct {
	mode object.Mode
	size uint64
	blob hash.ID
}

func (v *treeVerifier) commit(ctx context.Context, sha gitsource.SHA, id hash.ID) ([]string, int, error) {
	cs, err := v.r.Changeset(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	got := map[string]xfile{}
	if err := v.walk(ctx, cs.Core.RootTree, nil, got); err != nil {
		return nil, 0, err
	}
	entries, err := v.src.LsTree(ctx, sha)
	if err != nil {
		return nil, 0, err
	}
	var shas [][20]byte
	for _, e := range entries {
		if e.Mode != gitsource.ModeSubmodule {
			shas = append(shas, e.SHA)
		}
	}
	mapped, err := v.r.GitBlobs(ctx, shas)
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	seen := 0
	for _, e := range entries {
		if e.Mode == gitsource.ModeSubmodule {
			continue
		}
		seen++
		p := string(e.Path)
		f, ok := got[p]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: in git, missing in Xylem", strconv.Quote(p)))
			continue
		}
		delete(got, p)
		mode, present, err := xylemMode(e.Mode)
		if err != nil || !present || f.mode != mode {
			problems = append(problems, fmt.Sprintf("%s: mode %v in Xylem, %o in git", strconv.Quote(p), f.mode, e.Mode))
		}
		if e.Size < 0 || f.size != uint64(e.Size) {
			problems = append(problems, fmt.Sprintf("%s: size %d in Xylem, %d in git", strconv.Quote(p), f.size, e.Size))
		}
		want, ok := mapped[e.SHA]
		var wantID hash.ID
		if ok {
			wantID = want.BlobID
		} else { // no mapping row: re-hash the git blob
			data, err := v.src.Blob(ctx, e.SHA)
			if err != nil {
				return nil, 0, err
			}
			wantID = object.ID(object.KindBlob, data)
		}
		if f.blob != wantID {
			problems = append(problems, fmt.Sprintf("%s: content differs (Xylem blob %s, git blob %s)", strconv.Quote(p), f.blob.Short(12), e.SHA))
		}
	}
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range got {
			if !yield(p) {
				return
			}
		}
	}) {
		problems = append(problems, fmt.Sprintf("%s: in Xylem, missing in git", strconv.Quote(p)))
	}
	return problems, seen, nil
}

// walk lists every file under tree id, keyed by full path.
func (v *treeVerifier) walk(ctx context.Context, id hash.ID, prefix []byte, out map[string]xfile) error {
	tree, err := v.r.Trees().ReadTree(ctx, id)
	if err != nil {
		return err
	}
	for _, e := range tree.Entries {
		p := e.Name
		if len(prefix) > 0 {
			p = bytes.Join([][]byte{prefix, e.Name}, []byte{'/'})
		}
		if e.Mode == object.ModeTree {
			if err := v.walk(ctx, e.ID, p, out); err != nil {
				return err
			}
			continue
		}
		out[string(p)] = xfile{mode: e.Mode, size: e.Size, blob: e.ID}
	}
	return nil
}
