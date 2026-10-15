package client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
)

// MaxStack caps stack discovery (06 §Stack discovery).
const MaxStack = 1000

// LandAttempts is the max_attempts xy land asks for. With N workspaces
// landing at once, about one wins each CAS round, so the server's default of
// 10 is too low for many concurrent landers; backoff caps each wait at
// 200 ms.
const LandAttempts = 100

// Stack returns the draft changesets from @ down to (excluding) the first
// changeset on the mainline, bottom → top (06 §Stack discovery).
func (w *Workspace) Stack(ctx context.Context, f *Fetcher) ([]ChangesetInfo, error) {
	if w.State.Parent.IsZero() {
		return nil, errors.New("the workspace has no parent changeset")
	}
	var stack []ChangesetInfo
	cur := w.State.Parent
	for {
		resp, err := f.conn.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Repo: f.repo, Ids: [][]byte{cur[:]}})
		if err != nil {
			return nil, err
		}
		c := resp.GetChangesets()[0]
		if c.GetOnMainline() {
			break
		}
		if len(stack) == MaxStack {
			return nil, fmt.Errorf("more than %d draft changesets below @", MaxStack)
		}
		stack = append(stack, NewChangesetInfo(c))
		if len(c.GetParents()) == 0 {
			return nil, fmt.Errorf("draft %s is a root: @ does not descend from the mainline", HexID(c.GetId())[:12])
		}
		copy(cur[:], c.GetParents()[0])
	}
	for i, j := 0, len(stack)-1; i < j; i, j = i+1, j-1 {
		stack[i], stack[j] = stack[j], stack[i]
	}
	return stack, nil
}

// Conflict is a file-level conflict reported by Rebase or Land.
type Conflict struct {
	Path   Path   `json:"path"`
	Kind   string `json:"kind"`
	Base   string `json:"base_blob,omitempty"`
	Ours   string `json:"ours_blob,omitempty"`   // the target (main) side
	Theirs string `json:"theirs_blob,omitempty"` // the stack side
	// Regions counts conflicting line regions when a line merge was
	// attempted; 0 means the whole file conflicts.
	Regions int `json:"regions,omitempty"`
}

// ConflictError reports conflicts; nothing moved (exit 3).
type ConflictError struct {
	Changeset string // the stack entry that conflicted
	Conflicts []Conflict
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%d conflicts in %s; nothing landed and the workspace is unchanged", len(e.Conflicts), e.Changeset[:12])
}

// Table prints the conflicts, one per line: "<kind>\t<path>".
func (e *ConflictError) Table() string {
	var b strings.Builder
	for _, c := range e.Conflicts {
		kind := c.Kind
		if c.Regions > 0 {
			kind = fmt.Sprintf("%s (%d)", c.Kind, c.Regions)
		}
		fmt.Fprintf(&b, "%-15s %s\n", kind, QuotePath(c.Path))
	}
	return b.String()
}

var conflictNames = map[xylemv1.Conflict_Kind]string{
	xylemv1.Conflict_KIND_CONTENT: "content", xylemv1.Conflict_KIND_DELETE_MODIFY: "delete/modify",
	xylemv1.Conflict_KIND_ADD_ADD: "add/add", xylemv1.Conflict_KIND_FILE_DIRECTORY: "file/directory",
	xylemv1.Conflict_KIND_MODE: "mode",
}

func conflictError(id []byte, cs []*xylemv1.Conflict) error {
	e := &ConflictError{Changeset: HexID(id)}
	for _, c := range cs {
		cf := Conflict{Path: c.GetPath(), Kind: conflictNames[c.GetKind()], Regions: int(c.GetRegionCount())}
		if len(c.GetBaseBlob()) > 0 {
			cf.Base = HexID(c.GetBaseBlob())
		}
		if len(c.GetOursBlob()) > 0 {
			cf.Ours = HexID(c.GetOursBlob())
		}
		if len(c.GetTheirsBlob()) > 0 {
			cf.Theirs = HexID(c.GetTheirsBlob())
		}
		e.Conflicts = append(e.Conflicts, cf)
	}
	return &ExitError{Code: ExitConflict, Err: e}
}

// requireClean refuses (exit 4) when tracked files have local changes or
// files are marked added; untracked files are fine.
func (w *Workspace) requireClean(ctx context.Context, f *Fetcher) error {
	status, err := w.Status(ctx, f)
	if err != nil {
		return err
	}
	var dirty [][]byte
	for _, s := range status {
		if s.Code != StatusUntracked {
			dirty = append(dirty, s.Path)
		}
	}
	if len(dirty) > 0 {
		return &ExitError{Code: ExitDirty, Err: fmt.Errorf("commit or discard local changes first: %w", &DirtyError{Paths: dirty})}
	}
	return nil
}

func stackIDs(stack []ChangesetInfo) ([][]byte, error) {
	out := make([][]byte, len(stack))
	for i, c := range stack {
		id, err := hash.ParseHex(c.ID)
		if err != nil {
			return nil, err
		}
		out[i] = id[:]
	}
	return out, nil
}

// MoveResult describes a sync or land.
type MoveResult struct {
	Stack    int       // draft changesets considered
	Moved    []hash.ID // rebased or landed changesets, bottom → top
	Skipped  int       // drafts already on main
	Top      hash.ID   // the new @
	Attempts int       // land CAS attempts
	Checkout *CheckoutResult
}

func (w *Workspace) moveTo(ctx context.Context, f *Fetcher, top hash.ID, res *MoveResult) (*MoveResult, error) {
	resp, err := f.conn.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Repo: f.repo, Revision: top.String()})
	if err != nil {
		return nil, err
	}
	var target Target
	copy(target.Changeset[:], resp.GetId())
	copy(target.Root[:], resp.GetRootTree())
	if res.Checkout, err = w.Checkout(ctx, f, target, false); err != nil {
		return nil, err
	}
	res.Top = target.Changeset
	return res, nil
}

func idsOf(raw [][]byte) []hash.ID {
	out := make([]hash.ID, len(raw))
	for i, b := range raw {
		copy(out[i][:], b)
	}
	return out
}

// Sync moves the workspace onto main's tip (06 §Commands): with no drafts it
// checks out main; with drafts it rebases the stack onto main and checks out
// the rebased top. It refuses with local changes (exit 4) and on conflicts
// (exit 3, nothing moves). The caller holds the workspace lock.
func (w *Workspace) Sync(ctx context.Context, f *Fetcher) (*MoveResult, error) {
	if err := w.requireClean(ctx, f); err != nil {
		return nil, err
	}
	stack, err := w.Stack(ctx, f)
	if err != nil {
		return nil, err
	}
	tip, err := f.conn.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Repo: f.repo, Revision: "main"})
	if err != nil {
		return nil, err
	}
	res := &MoveResult{Stack: len(stack)}
	var top hash.ID
	copy(top[:], tip.GetId())
	if len(stack) > 0 {
		ids, err := stackIDs(stack)
		if err != nil {
			return nil, err
		}
		rb, err := f.conn.Write.Rebase(ctx, &xylemv1.RebaseRequest{Repo: f.repo, Stack: ids, Onto: top.String()})
		if err != nil {
			return nil, err
		}
		if len(rb.GetConflicts()) > 0 {
			return nil, conflictError(rb.GetConflictingChangeset(), rb.GetConflicts())
		}
		res.Moved, res.Skipped = idsOf(rb.GetRebased()), len(rb.GetSkipped())
		if len(res.Moved) > 0 {
			top = res.Moved[len(res.Moved)-1]
		}
	}
	return w.moveTo(ctx, f, top, res)
}

// Land lands the stack below @ onto main and checks out the new tip (06
// §Commands). It refuses with local changes (exit 4); on conflicts it returns
// exit 3 with nothing landed and the workspace unchanged. The caller holds
// the workspace lock.
func (w *Workspace) Land(ctx context.Context, f *Fetcher) (*MoveResult, error) {
	if err := w.requireClean(ctx, f); err != nil {
		return nil, err
	}
	stack, err := w.Stack(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(stack) == 0 {
		return nil, errors.New("nothing to land: @ is already on main (commit first)")
	}
	ids, err := stackIDs(stack)
	if err != nil {
		return nil, err
	}
	resp, err := f.conn.Write.Land(ctx, &xylemv1.LandRequest{Repo: f.repo, Stack: ids, MaxAttempts: LandAttempts})
	if err != nil {
		return nil, err
	}
	if len(resp.GetConflicts()) > 0 {
		return nil, conflictError(resp.GetConflictingChangeset(), resp.GetConflicts())
	}
	res := &MoveResult{Stack: len(stack), Moved: idsOf(resp.GetLanded()), Skipped: len(resp.GetSkipped()), Attempts: int(resp.GetAttempts())}
	var top hash.ID
	copy(top[:], resp.GetNewTip())
	return w.moveTo(ctx, f, top, res)
}
