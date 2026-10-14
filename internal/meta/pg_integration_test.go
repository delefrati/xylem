//go:build integration

package meta_test

import (
	"bytes"
	"cmp"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/testutil"
)

func newStore(t *testing.T) (*meta.PG, meta.RepoID) {
	t.Helper()
	s := meta.NewPG(testutil.NewDB(t))
	repo, err := s.CreateRepo(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	return s, repo
}

func randomID() hash.ID {
	var id hash.ID
	_, _ = cryptorand.Read(id[:])
	return id
}

// row builds a synthetic changeset row; meta stores rows without decoding them.
func row(parents ...hash.ID) (meta.ChangesetRow, meta.CoreRow) {
	id := randomID()
	cs := meta.ChangesetRow{
		ID: id, Parents: parents, Body: append([]byte("body of "), id[:]...),
		Author: []byte("A <a@x>"), AuthorTime: 1700000000,
		Committer: []byte("C \xff <c@x>"), CommitterTime: 1700000001,
		Summary: "summary " + id.Short(8),
	}
	core := meta.CoreRow{ChangesetID: id, RootTree: randomID(), Gen: 1, FPJump: id}
	if len(parents) > 0 {
		core.FPParent = parents[0]
	}
	return cs, core
}

func put(path string) meta.FileRow {
	return meta.FileRow{Path: []byte(path), Op: changeset.OpPut, Mode: object.ModeFile, Size: 3, Blob: randomID()}
}

func del(path string) meta.FileRow {
	return meta.FileRow{Path: []byte(path), Op: changeset.OpDelete}
}

func insert(t *testing.T, s *meta.PG, repo meta.RepoID, cs meta.ChangesetRow, files []meta.FileRow, core meta.CoreRow) int64 {
	t.Helper()
	seq, inserted, err := s.InsertChangeset(t.Context(), repo, cs, files, core)
	if err != nil || !inserted {
		t.Fatalf("InsertChangeset: inserted=%v err=%v", inserted, err)
	}
	return seq
}

func TestRepos(t *testing.T) {
	t.Parallel()
	s := meta.NewPG(testutil.NewDB(t))
	ctx := t.Context()
	a, err := s.CreateRepo(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRepo(ctx, "b")
	if err != nil || a == b {
		t.Fatalf("CreateRepo b = %d, %v", b, err)
	}
	if _, err := s.CreateRepo(ctx, "a"); !errors.Is(err, meta.ErrExists) {
		t.Errorf("duplicate CreateRepo = %v, want ErrExists", err)
	}
	// A fresh store (no cache) finds it in the database.
	fresh := meta.NewPG(testutil.NewDB(t))
	if _, err := fresh.LookupRepo(ctx, "a"); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("lookup in another database = %v, want ErrNotFound", err)
	}
	if got, err := s.LookupRepo(ctx, "b"); err != nil || got != b {
		t.Errorf("LookupRepo(b) = %d, %v", got, err)
	}
}

func TestInsertAndGetChangesets(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	ctx := t.Context()

	root, rootCore := row()
	root.GitSHA = bytes.Repeat([]byte{0xab}, 20)
	root.AuthorTime, root.CommitterTime = math.MaxInt64, math.MinInt64 // clamped in the columns
	rootSeq := insert(t, s, repo, root, []meta.FileRow{put("b"), put("a/x")}, rootCore)

	child, childCore := row(root.ID)
	childCore.Gen, childCore.FPDepth, childCore.FPJump = 2, 1, root.ID
	childSeq := insert(t, s, repo, child, []meta.FileRow{del("a/x"), put("c")}, childCore)
	if childSeq <= rootSeq {
		t.Errorf("seq(child) = %d not > seq(root) = %d", childSeq, rootSeq)
	}

	missing := randomID()
	got, err := s.GetChangesets(ctx, repo, []hash.ID{root.ID, child.ID, missing})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("GetChangesets returned %d rows, want 2", len(got))
	}
	wantRoot := root
	wantRoot.Seq, wantRoot.AuthorTime, wantRoot.CommitterTime = rootSeq, 253402300799, -62135596800
	wantRoot.Parents = []hash.ID{}
	if diff := gocmp.Diff(wantRoot, got[root.ID]); diff != "" {
		t.Errorf("root row mismatch (-want +got):\n%s", diff)
	}
	wantChild := child
	wantChild.Seq = childSeq
	if diff := gocmp.Diff(wantChild, got[child.ID]); diff != "" {
		t.Errorf("child row mismatch (-want +got):\n%s", diff)
	}

	cores, err := s.GetCore(ctx, repo, []hash.ID{root.ID, child.ID, missing})
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(map[hash.ID]meta.CoreRow{root.ID: rootCore, child.ID: childCore}, cores); diff != "" {
		t.Errorf("core rows mismatch (-want +got):\n%s", diff)
	}

	parents, err := s.GetParents(ctx, repo, []hash.ID{root.ID, child.ID, missing})
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(map[hash.ID][]hash.ID{root.ID: {}, child.ID: {root.ID}}, parents); diff != "" {
		t.Errorf("GetParents mismatch (-want +got):\n%s", diff)
	}

	files, err := s.ChangesetFiles(ctx, repo, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || string(files[0].Path) != "a/x" || files[0].Op != changeset.OpDelete || string(files[1].Path) != "c" || files[1].Size != 3 {
		t.Errorf("ChangesetFiles = %+v", files)
	}

	if id, err := s.GetByGitSHA(ctx, repo, root.GitSHA); err != nil || id != root.ID {
		t.Errorf("GetByGitSHA = %s, %v", id, err)
	}
	if _, err := s.GetByGitSHA(ctx, repo, bytes.Repeat([]byte{1}, 20)); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("GetByGitSHA(unknown) = %v, want ErrNotFound", err)
	}
}

func TestInsertIdempotentAndConcurrent(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	cs, core := row()
	files := []meta.FileRow{put("a"), put("b")}

	const n = 10
	type result struct {
		seq      int64
		inserted bool
		err      error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			seq, inserted, err := s.InsertChangeset(t.Context(), repo, cs, files, core)
			results[i] = result{seq, inserted, err}
		})
	}
	wg.Wait()
	inserted := 0
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("concurrent insert: %v", r.err)
		}
		if r.inserted {
			inserted++
		}
		if r.seq != results[0].seq {
			t.Errorf("seq %d differs from %d", r.seq, results[0].seq)
		}
	}
	if inserted != 1 {
		t.Errorf("%d inserts reported inserted, want exactly 1", inserted)
	}
	got, err := s.ChangesetFiles(t.Context(), repo, cs.ID)
	if err != nil || len(got) != 2 {
		t.Errorf("files after concurrent insert = %d, %v; want 2", len(got), err)
	}
}

func TestInsertManyFilesUsesCopy(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	cs, core := row()
	var files []meta.FileRow
	for i := range 2500 {
		if i%5 == 0 {
			files = append(files, del(fmt.Sprintf("dir/f%05d", i)))
		} else {
			files = append(files, put(fmt.Sprintf("dir/f%05d", i)))
		}
	}
	insert(t, s, repo, cs, files, core)
	got, err := s.ChangesetFiles(t.Context(), repo, cs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(files, got); diff != "" {
		t.Errorf("files mismatch (-want +got):\n%s", diff)
	}
}

func TestInsertRejectsMismatchedCore(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	cs, _ := row()
	_, other := row()
	if _, _, err := s.InsertChangeset(t.Context(), repo, cs, nil, other); err == nil {
		t.Error("InsertChangeset with another changeset's core row succeeded")
	}
}

func TestResolvePrefix(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	// Ids starting 10 22…, 11 22…, 11 22…, ff 22….
	var ids []hash.ID
	for _, first := range []byte{0x10, 0x11, 0x11, 0xff} {
		cs, core := row()
		cs.ID[0], cs.ID[1] = first, 0x22
		core.ChangesetID = cs.ID
		insert(t, s, repo, cs, nil, core)
		ids = append(ids, cs.ID)
	}
	slices.SortFunc(ids, hash.Compare)
	resolve := func(lo, hi []byte, limit int) []hash.ID {
		t.Helper()
		got, err := s.ResolvePrefix(t.Context(), repo, lo, hi, limit)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	prefix := func(p string) ([]byte, []byte) {
		t.Helper()
		lo, hi, err := hash.ParsePrefix(p)
		if err != nil {
			t.Fatal(err)
		}
		return lo, hi
	}

	if lo, hi := prefix(ids[0].Short(9)); !slices.Equal(resolve(lo, hi, 5), ids[:1]) {
		t.Errorf("unique 9-char prefix did not resolve to exactly %s", ids[0].Short(9))
	}
	if lo, hi := prefix(ids[3].Short(8)); !slices.Equal(resolve(lo, hi, 5), ids[3:]) {
		t.Errorf("prefix of the ff id did not resolve")
	}
	if lo, hi := prefix("11220000"); len(resolve(lo, hi, 5)) != 0 {
		t.Errorf("prefix 11220000 matched something")
	}
	if got := resolve([]byte{0x11}, []byte{0x12}, 5); !slices.Equal(got, ids[1:3]) {
		t.Errorf("range [11, 12) = %v, want the two 0x11 ids", got)
	}
	if got := resolve([]byte{0xff}, nil, 5); !slices.Equal(got, ids[3:]) {
		t.Errorf("open range [ff, ∞) = %v", got)
	}
	if got := resolve([]byte{0x11}, []byte{0x12}, 1); !slices.Equal(got, ids[1:2]) {
		t.Errorf("limit 1 = %v, want the smallest match", got)
	}
}

// chain inserts n changesets on a first-parent chain and returns them.
func chain(t *testing.T, s *meta.PG, repo meta.RepoID, n int, files func(i int) []meta.FileRow) []meta.ChangesetRow {
	t.Helper()
	var out []meta.ChangesetRow
	var parents []hash.ID
	for i := range n {
		cs, core := row(parents...)
		core.FPDepth = int64(i)
		cs.Seq = insert(t, s, repo, cs, files(i), core)
		out = append(out, cs)
		parents = []hash.ID{cs.ID}
	}
	return out
}

func mainlineRows(cs []meta.ChangesetRow, from int) []meta.MainlineRow {
	var rows []meta.MainlineRow
	for i := from; i < len(cs); i++ {
		rows = append(rows, meta.MainlineRow{Depth: int64(i), ID: cs[i].ID})
	}
	return rows
}

func TestRefsAndMainline(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	ctx := t.Context()
	c := chain(t, s, repo, 6, func(int) []meta.FileRow { return nil })
	admin := meta.RefLogEntry{Actor: "test", Reason: "admin"}

	ref, err := s.CreateRef(ctx, repo, "main", c[2].ID, mainlineRows(c[:3], 0), admin)
	if err != nil || ref.Version != 1 {
		t.Fatalf("CreateRef = %+v, %v", ref, err)
	}
	if _, err := s.CreateRef(ctx, repo, "main", c[0].ID, nil, admin); !errors.Is(err, meta.ErrExists) {
		t.Errorf("duplicate CreateRef = %v, want ErrExists", err)
	}
	if _, err := s.GetRef(ctx, repo, "nope"); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("GetRef(nope) = %v, want ErrNotFound", err)
	}

	// Fast-forward to c[5].
	ref, err = s.CASRef(ctx, repo, "main", c[2].ID, c[5].ID, meta.MainlineUpdate{Rows: mainlineRows(c, 3)}, admin)
	if err != nil || ref.Version != 2 || ref.ID != c[5].ID {
		t.Fatalf("CASRef = %+v, %v", ref, err)
	}
	// Stale CAS fails and changes nothing.
	if _, err := s.CASRef(ctx, repo, "main", c[2].ID, c[4].ID, meta.MainlineUpdate{}, admin); !errors.Is(err, meta.ErrCASMismatch) {
		t.Errorf("stale CASRef = %v, want ErrCASMismatch", err)
	}
	if _, err := s.CASRef(ctx, repo, "nope", c[2].ID, c[4].ID, meta.MainlineUpdate{}, admin); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("CASRef(unknown ref) = %v, want ErrNotFound", err)
	}
	if got, err := s.GetRef(ctx, repo, "main"); err != nil || got != ref {
		t.Errorf("GetRef = %+v, %v; want %+v", got, err, ref)
	}
	for d := range 6 {
		if id, err := s.MainlineAt(ctx, repo, "main", int64(d)); err != nil || id != c[d].ID {
			t.Errorf("MainlineAt(%d) = %s, %v", d, id, err)
		}
	}
	if _, err := s.MainlineAt(ctx, repo, "main", 6); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("MainlineAt(6) = %v, want ErrNotFound", err)
	}

	// Forced rewind: keep depths 0..2, then a side chain at depths 3..4.
	side, sideCore := row(c[2].ID)
	sideCore.FPDepth = 3
	insert(t, s, repo, side, nil, sideCore)
	side2, side2Core := row(side.ID)
	side2Core.FPDepth = 4
	insert(t, s, repo, side2, nil, side2Core)
	update := meta.MainlineUpdate{Rewind: true, KeepThrough: 2, Rows: []meta.MainlineRow{{Depth: 3, ID: side.ID}, {Depth: 4, ID: side2.ID}}}
	if _, err := s.CASRef(ctx, repo, "main", c[5].ID, side2.ID, update, meta.RefLogEntry{Actor: "test", Reason: "admin"}); err != nil {
		t.Fatal(err)
	}
	depths, err := s.MainlineLookup(ctx, repo, "main", []hash.ID{c[0].ID, c[2].ID, c[3].ID, c[5].ID, side.ID, side2.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[hash.ID]int64{c[0].ID: 0, c[2].ID: 2, side.ID: 3, side2.ID: 4}
	if diff := gocmp.Diff(want, depths); diff != "" {
		t.Errorf("mainline after rewind (-want +got):\n%s", diff)
	}
	if _, err := s.MainlineAt(ctx, repo, "main", 5); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("depth 5 survived the rewind: %v", err)
	}
}

func TestCASRefConcurrent(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	ctx := t.Context()
	c := chain(t, s, repo, 1, func(int) []meta.FileRow { return nil })
	if _, err := s.CreateRef(ctx, repo, "main", c[0].ID, mainlineRows(c, 0), meta.RefLogEntry{Actor: "t", Reason: "admin"}); err != nil {
		t.Fatal(err)
	}
	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		cs, core := row(c[0].ID)
		core.FPDepth = 1
		insert(t, s, repo, cs, nil, core)
		wg.Go(func() {
			_, errs[i] = s.CASRef(ctx, repo, "main", c[0].ID, cs.ID,
				meta.MainlineUpdate{Rows: []meta.MainlineRow{{Depth: 1, ID: cs.ID}}}, meta.RefLogEntry{Actor: "t", Reason: "land"})
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, meta.ErrCASMismatch):
			t.Errorf("unexpected CAS error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d concurrent CAS succeeded, want 1", ok)
	}
}

func TestPathHistoryMainline(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	ctx := t.Context()
	// Depth i touches the listed paths.
	touched := [][]string{
		{"serv/a", "services/a"},
		{"services/b"},
		{"serv/a"},
		{"serv/sub/c", "top"},
		{"serv"}, // file named like the dir (after a delete, in real history)
		{"services/c"},
	}
	c := chain(t, s, repo, len(touched), func(i int) []meta.FileRow {
		var rows []meta.FileRow
		for _, p := range touched[i] {
			rows = append(rows, put(p))
		}
		return rows
	})
	if _, err := s.CreateRef(ctx, repo, "main", c[len(c)-1].ID, mainlineRows(c, 0), meta.RefLogEntry{Actor: "t", Reason: "admin"}); err != nil {
		t.Fatal(err)
	}
	// A draft off depth 2 that touches serv/a is not on the mainline.
	draft, draftCore := row(c[2].ID)
	insert(t, s, repo, draft, []meta.FileRow{put("serv/a")}, draftCore)

	depthsOf := func(hits []meta.PathHit) []int {
		var out []int
		for _, h := range hits {
			out = append(out, slices.IndexFunc(c, func(r meta.ChangesetRow) bool { return r.ID == h.ChangesetID }))
		}
		return out
	}
	tests := []struct {
		path      string
		maxDepth  int64
		beforeSeq int64
		limit     int
		want      []int
	}{
		{"serv", 10, math.MaxInt64, 10, []int{4, 3, 2, 0}},
		{"serv/a", 10, math.MaxInt64, 10, []int{2, 0}},
		{"services", 10, math.MaxInt64, 10, []int{5, 1, 0}},
		{"serv", 2, math.MaxInt64, 10, []int{2, 0}},
		{"serv", 10, c[3].Seq, 10, []int{2, 0}},
		{"serv", 10, math.MaxInt64, 2, []int{4, 3}},
		{"top", 10, math.MaxInt64, 10, []int{3}},
		{"nothing", 10, math.MaxInt64, 10, nil},
	}
	for _, tt := range tests {
		hits, err := s.PathHistoryMainline(ctx, repo, "main", []byte(tt.path), tt.maxDepth, tt.beforeSeq, tt.limit)
		if err != nil {
			t.Fatalf("%s: %v", tt.path, err)
		}
		if got := depthsOf(hits); !slices.Equal(got, tt.want) {
			t.Errorf("history(%q, maxDepth %d, before %d, limit %d) = depths %v, want %v", tt.path, tt.maxDepth, tt.beforeSeq, tt.limit, got, tt.want)
		}
		if !slices.IsSortedFunc(hits, func(a, b meta.PathHit) int { return cmp.Compare(b.Seq, a.Seq) }) {
			t.Errorf("history(%q) not newest first", tt.path)
		}
	}
	if _, err := s.PathHistoryMainline(ctx, repo, "main", nil, 10, math.MaxInt64, 10); err == nil {
		t.Error("empty path accepted")
	}
}

func TestListingQueries(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	ctx := t.Context()
	c := chain(t, s, repo, 7, func(i int) []meta.FileRow {
		return []meta.FileRow{put(fmt.Sprint("f", i)), put(fmt.Sprint("a/g", i))}
	})
	// ListChangesets pages in seq order.
	var seen []hash.ID
	after := int64(math.MinInt64)
	for {
		page, err := s.ListChangesets(ctx, repo, after, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			seen = append(seen, r.ID)
		}
		after = page[len(page)-1].Seq
	}
	for i := range c {
		if seen[i] != c[i].ID {
			t.Fatalf("ListChangesets order differs at %d", i)
		}
	}
	if sample, err := s.SampleChangesets(ctx, repo, 4); err != nil || len(sample) != 4 {
		t.Errorf("SampleChangesets = %d, %v", len(sample), err)
	}
	files, err := s.FilesFor(ctx, repo, []hash.ID{c[1].ID, c[4].ID, randomID()})
	if err != nil || len(files) != 2 || len(files[c[4].ID]) != 2 || string(files[c[4].ID][0].Path) != "a/g4" {
		t.Errorf("FilesFor = %v, %v", files, err)
	}

	admin := meta.RefLogEntry{Actor: "t", Reason: "admin"}
	for _, name := range []string{"main", "release/2", "release/1"} {
		if _, err := s.CreateRef(ctx, repo, name, c[6].ID, nil, admin); err != nil {
			t.Fatal(err)
		}
	}
	refs, err := s.ListRefs(ctx, repo, "release/")
	if err != nil || len(refs) != 2 || refs[0].Name != "release/1" || refs[1].Name != "release/2" {
		t.Errorf("ListRefs(release/) = %v, %v", refs, err)
	}
	if all, _ := s.ListRefs(ctx, repo, ""); len(all) != 3 {
		t.Errorf("ListRefs() = %d refs", len(all))
	}

	if _, err := s.CASRef(ctx, repo, "main", c[6].ID, c[6].ID, meta.MainlineUpdate{Rows: mainlineRows(c, 0)}, admin); err != nil {
		t.Fatal(err)
	}
	rows, err := s.MainlineRange(ctx, repo, "main", 4, 3)
	if err != nil || len(rows) != 3 || rows[0].Depth != 4 || rows[2].Depth != 2 || rows[0].ID != c[4].ID {
		t.Errorf("MainlineRange(≤4, 3) = %v, %v", rows, err)
	}
}

func TestImporterTables(t *testing.T) {
	t.Parallel()
	s, repo := newStore(t)
	ctx := t.Context()
	// GitSHAMap: only changesets with a git sha.
	a, aCore := row()
	a.GitSHA = bytes.Repeat([]byte{0xaa}, 20)
	insert(t, s, repo, a, nil, aCore)
	b, bCore := row()
	insert(t, s, repo, b, nil, bCore)
	m, err := s.GitSHAMap(ctx, repo)
	var aSHA [20]byte
	copy(aSHA[:], a.GitSHA)
	if err != nil || len(m) != 1 || m[aSHA] != a.ID {
		t.Errorf("GitSHAMap = %v, %v", m, err)
	}

	// Import state upsert.
	if _, err := s.GetImportState(ctx, repo, "file:///r", "main"); !errors.Is(err, meta.ErrNotFound) {
		t.Errorf("missing import state: %v", err)
	}
	for _, v := range []byte{1, 2} {
		var sha [20]byte
		sha[0] = v
		if err := s.PutImportState(ctx, repo, "file:///r", "main", sha); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetImportState(ctx, repo, "file:///r", "main"); err != nil || got != sha {
			t.Errorf("import state = %x, %v; want %x", got, err, sha)
		}
	}

	// Git blobs: duplicates within a batch and across batches are ignored.
	blob := func(b byte) meta.GitBlob {
		var sha [20]byte
		sha[0] = b
		return meta.GitBlob{GitSHA: sha, BlobID: randomID(), Size: int64(b)}
	}
	first := []meta.GitBlob{blob(1), blob(2), blob(1)}
	if err := s.InsertGitBlobs(ctx, repo, first); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertGitBlobs(ctx, repo, []meta.GitBlob{blob(2), blob(3)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGitBlobs(ctx, repo, [][20]byte{blob(1).GitSHA, blob(3).GitSHA, blob(9).GitSHA})
	if err != nil || len(got) != 2 || got[blob(1).GitSHA].Size != 1 {
		t.Errorf("GetGitBlobs = %v, %v", got, err)
	}
	if got[blob(1).GitSHA].BlobID != first[0].BlobID && got[blob(1).GitSHA].BlobID != first[2].BlobID {
		t.Error("git blob mapping lost its blob id")
	}
	if err := s.InsertGitBlobs(ctx, repo, nil); err != nil {
		t.Errorf("empty insert: %v", err)
	}
}
