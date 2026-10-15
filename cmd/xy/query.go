package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/client"
)

// query is the context of a read-only command: an optional workspace and a
// server connection.
type query struct {
	g    *globals
	w    *client.Workspace // nil outside a workspace (needs --server)
	conn *client.Conn
	repo string
}

func (g *globals) query() (*query, error) {
	w, err := g.workspace()
	if errors.Is(err, client.ErrNotWorkspace) {
		w, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	conn, err := client.Dial(g.serverAddr(w))
	if err != nil {
		return nil, err
	}
	return &query{g: g, w: w, conn: conn, repo: g.repoName(w)}, nil
}

func (q *query) close() { _ = q.conn.Close() }

// rev resolves "@" client-side; an empty rev is "@" in a workspace with a
// parent, else "main".
func (q *query) rev(rev string) (string, error) {
	if rev == "" {
		if q.w == nil || q.w.State.Parent.IsZero() {
			return "main", nil
		}
		rev = "@"
	}
	return client.ResolveRev(rev, q.w)
}

// path converts a command-line path: relative to the working directory in
// a workspace, repository-relative otherwise.
func (q *query) path(arg string) ([]byte, error) {
	if q.w == nil {
		return []byte(strings.Trim(strings.TrimPrefix(arg, "./"), "/")), nil
	}
	ps, err := q.g.repoPaths(q.w, []string{arg})
	if err != nil {
		return nil, err
	}
	return ps[0], nil
}

func newLogCmd(g *globals) *cobra.Command {
	var rev string
	var limit int
	var oneline bool
	cmd := &cobra.Command{
		Use:   "log [path]",
		Short: "First-parent history (of a path), newest first",
		Args:  args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			q, err := g.query()
			if err != nil {
				return err
			}
			defer q.close()
			req := &xylemv1.LogRequest{Repo: q.repo, Page: &xylemv1.PageRequest{}}
			if req.Revision, err = q.rev(rev); err != nil {
				return err
			}
			if len(a) == 1 {
				if req.Path, err = q.path(a[0]); err != nil {
					return err
				}
			}
			var out []client.ChangesetInfo
			for limit <= 0 || len(out) < limit {
				if limit > 0 {
					req.Page.PageSize = uint32(min(limit-len(out), 1000)) //nolint:gosec // G115: bounded by 1000
				}
				resp, err := q.conn.Repo.Log(cmd.Context(), req)
				if err != nil {
					return err
				}
				for _, c := range resp.GetChangesets() {
					out = append(out, client.NewChangesetInfo(c))
				}
				if req.Page.PageToken = resp.GetNextPageToken(); req.Page.PageToken == "" {
					break
				}
			}
			if limit > 0 && len(out) > limit {
				out = out[:limit]
			}
			return g.emit(cmd, out, func(o io.Writer) error {
				for i, c := range out {
					var err error
					switch {
					case oneline:
						err = printf(o, "%s %s\n", c.ID[:12], c.Subject())
					case i > 0:
						if err = printf(o, "\n"); err == nil {
							err = client.WriteChangeset(o, c)
						}
					default:
						err = client.WriteChangeset(o, c)
					}
					if err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&rev, "rev", "r", "", "revision (default @ in a workspace, else main)")
	cmd.Flags().IntVarP(&limit, "limit", "n", 0, "show at most N changesets (0: all)")
	cmd.Flags().BoolVar(&oneline, "oneline", false, "one line per changeset")
	return cmd
}

// fileDiffInfo is a FileDiff in JSON.
type fileDiffInfo struct {
	Path    client.Path `json:"path"`
	Status  string      `json:"status"`
	OldMode string      `json:"old_mode,omitempty"`
	NewMode string      `json:"new_mode,omitempty"`
	Binary  bool        `json:"binary,omitempty"`
	Hunks   []hunkInfo  `json:"hunks,omitempty"`
}

type hunkInfo struct {
	Header string      `json:"header"`
	Body   client.Path `json:"body"`
}

var diffStatus = map[xylemv1.FileDiff_Status]string{
	xylemv1.FileDiff_STATUS_ADDED: "added", xylemv1.FileDiff_STATUS_DELETED: "deleted",
	xylemv1.FileDiff_STATUS_MODIFIED: "modified", xylemv1.FileDiff_STATUS_MODE_CHANGED: "mode_changed",
}

func newShowCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "show [rev]",
		Short: "Changeset metadata and its diff against the first parent",
		Args:  args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			q, err := g.query()
			if err != nil {
				return err
			}
			defer q.close()
			rev := ""
			if len(a) == 1 {
				rev = a[0]
			}
			if rev, err = q.rev(rev); err != nil {
				return err
			}
			ctx := cmd.Context()
			res, err := q.conn.Repo.ResolveRevision(ctx, &xylemv1.ResolveRevisionRequest{Repo: q.repo, Revision: rev})
			if err != nil {
				return err
			}
			cs, err := q.conn.Repo.GetChangesets(ctx, &xylemv1.GetChangesetsRequest{Repo: q.repo, Ids: [][]byte{res.GetId()}})
			if err != nil {
				return err
			}
			if len(cs.GetChangesets()) != 1 {
				return fmt.Errorf("changeset %s not returned", rev)
			}
			info := client.NewChangesetInfo(cs.GetChangesets()[0])
			stream, err := q.conn.Repo.Diff(ctx, &xylemv1.DiffRequest{Repo: q.repo, ToRevision: info.ID, IncludeHunks: true})
			if err != nil {
				return err
			}
			var files []*xylemv1.FileDiff
			for {
				resp, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				files = append(files, resp.GetFiles()...)
			}
			out := struct {
				Changeset client.ChangesetInfo `json:"changeset"`
				Files     []fileDiffInfo       `json:"files"`
			}{Changeset: info, Files: make([]fileDiffInfo, len(files))}
			for i, f := range files {
				fi := fileDiffInfo{
					Path: f.GetPath(), Status: diffStatus[f.GetStatus()],
					OldMode: client.GitMode(f.GetOldMode()), NewMode: client.GitMode(f.GetNewMode()), Binary: f.GetBinary(),
				}
				for _, h := range f.GetHunks() {
					fi.Hunks = append(fi.Hunks, hunkInfo{
						Header: fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.GetOldStart(), h.GetOldLines(), h.GetNewStart(), h.GetNewLines()),
						Body:   h.GetBody(),
					})
				}
				out.Files[i] = fi
			}
			return g.emit(cmd, out, func(o io.Writer) error {
				if err := client.WriteChangeset(o, info); err != nil {
					return err
				}
				if len(files) > 0 {
					if err := printf(o, "\n"); err != nil {
						return err
					}
				}
				return client.WriteServerDiff(o, files)
			})
		},
	}
}

func newBlameCmd(g *globals) *cobra.Command {
	var rev string
	cmd := &cobra.Command{
		Use:   "blame <path>",
		Short: "Line-by-line origin of a file (first-parent history)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			q, err := g.query()
			if err != nil {
				return err
			}
			defer q.close()
			req := &xylemv1.BlameRequest{Repo: q.repo}
			if req.Revision, err = q.rev(rev); err != nil {
				return err
			}
			if req.Path, err = q.path(a[0]); err != nil {
				return err
			}
			resp, err := q.conn.Repo.Blame(cmd.Context(), req)
			if err != nil {
				return err
			}
			origins := map[string]client.ChangesetInfo{}
			for _, c := range resp.GetOrigins() {
				info := client.NewChangesetInfo(c)
				origins[info.ID] = info
			}
			type line struct {
				Origin     string      `json:"origin"`
				OriginLine uint32      `json:"origin_line"`
				Line       int         `json:"line"`
				Content    client.Path `json:"content"`
			}
			lines := make([]line, len(resp.GetLines()))
			nameWidth := 0
			for i, l := range resp.GetLines() {
				lines[i] = line{Origin: client.HexID(l.GetOrigin()), OriginLine: l.GetOriginLine(), Line: i + 1, Content: l.GetContent()}
				nameWidth = max(nameWidth, len(origins[lines[i].Origin].Author.Name()))
			}
			out := struct {
				Lines   []line                          `json:"lines"`
				Origins map[string]client.ChangesetInfo `json:"origins"`
			}{lines, origins}
			numWidth := len(fmt.Sprint(len(lines)))
			return g.emit(cmd, out, func(o io.Writer) error {
				for _, l := range lines {
					c := origins[l.Origin]
					if err := printf(o, "%s (%-*s %s %*d) %s\n", l.Origin[:12], nameWidth, c.Author.Name(),
						c.Author.LocalTime().Format("2006-01-02"), numWidth, l.Line, l.Content); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&rev, "rev", "r", "", "revision (default @ in a workspace, else main)")
	return cmd
}

func newCatCmd(g *globals) *cobra.Command {
	var rev string
	cmd := &cobra.Command{
		Use:   "cat <path>",
		Short: "Print a file at a revision (works without a workspace given --server)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			q, err := g.query()
			if err != nil {
				return err
			}
			defer q.close()
			req := &xylemv1.ReadFileRequest{Repo: q.repo}
			if req.Revision, err = q.rev(rev); err != nil {
				return err
			}
			if req.Path, err = q.path(a[0]); err != nil {
				return err
			}
			stream, err := q.conn.Repo.ReadFile(cmd.Context(), req)
			if err != nil {
				return err
			}
			for {
				resp, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if _, err := cmd.OutOrStdout().Write(resp.GetChunk()); err != nil {
					return err
				}
			}
		},
	}
	cmd.Flags().StringVarP(&rev, "rev", "r", "", "revision (default @ in a workspace, else main)")
	return cmd
}

func newLsCmd(g *globals) *cobra.Command {
	var rev string
	var recursive bool
	cmd := &cobra.Command{
		Use:   "ls [path]",
		Short: "List a directory at a revision",
		Args:  args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, a []string) error {
			q, err := g.query()
			if err != nil {
				return err
			}
			defer q.close()
			req := &xylemv1.ListDirectoryRequest{Repo: q.repo, Recursive: recursive, Page: &xylemv1.PageRequest{PageSize: 1000}}
			if req.Revision, err = q.rev(rev); err != nil {
				return err
			}
			arg := "."
			if len(a) == 1 {
				arg = a[0]
			}
			if req.Path, err = q.path(arg); err != nil {
				return err
			}
			type entry struct {
				Name client.Path `json:"name"`
				Mode string      `json:"mode"`
				Type string      `json:"type"`
				ID   string      `json:"id"`
				Size uint64      `json:"size"`
			}
			var out []entry
			for {
				resp, err := q.conn.Repo.ListDirectory(cmd.Context(), req)
				if err != nil {
					return err
				}
				for _, e := range resp.GetEntries() {
					typ := "blob"
					if e.GetMode() == xylemv1.FileMode_FILE_MODE_TREE {
						typ = "tree"
					}
					out = append(out, entry{e.GetName(), client.GitMode(e.GetMode()), typ, client.HexID(e.GetId()), e.GetSize()})
				}
				if req.Page.PageToken = resp.GetNextPageToken(); req.Page.PageToken == "" {
					break
				}
			}
			return g.emit(cmd, out, func(o io.Writer) error {
				for _, e := range out {
					size := "-"
					if e.Type == "blob" {
						size = fmt.Sprint(e.Size)
					}
					if err := printf(o, "%s %s %s %8s\t%s\n", e.Mode, e.Type, e.ID[:12], size, client.QuotePath(e.Name)); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&rev, "rev", "r", "", "revision (default @ in a workspace, else main)")
	cmd.Flags().BoolVarP(&recursive, "recursive", "R", false, "list every file below the directory")
	return cmd
}
