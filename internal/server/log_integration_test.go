//go:build integration

package server_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/gitsource"
	"github.com/delefrati/xylem/internal/testutil"
)

// logAll pages through Log and returns git shas (via the git-sha extra).
func logAll(t *testing.T, c *clients, req *xylemv1.LogRequest) []string {
	t.Helper()
	var out []string
	for {
		resp, err := c.Repo.Log(context.Background(), req)
		if err != nil {
			t.Fatalf("Log(%q): %v", req.Path, err)
		}
		for _, cs := range resp.Changesets {
			out = append(out, cs.GitSha)
		}
		if resp.NextPageToken == "" {
			return out
		}
		req.Page = &xylemv1.PageRequest{PageSize: req.GetPage().GetPageSize(), PageToken: resp.NextPageToken}
	}
}

func gitLog(t *testing.T, dir string, path string) []string {
	t.Helper()
	args := []string{"log", "--first-parent", "--format=%H", "main"}
	if path != "" {
		args = append(args, "--", path)
	}
	out := testutil.Git(t, dir, args...)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestLogMatchesGit(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"edge", "branchy"} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()
			c := startServer(t)
			src := importFixture(t, c, profile)
			dir := testutil.GitFixture(t, profile)

			if got, want := logAll(t, c, &xylemv1.LogRequest{Page: &xylemv1.PageRequest{PageSize: 37}}), gitLog(t, dir, ""); !slices.Equal(got, want) {
				t.Fatalf("repo log: %d entries, git %d (first difference matters)", len(got), len(want))
			}
			// Path logs: every top-level entry plus a sample of deeper paths.
			entries, err := src.LsTree(context.Background(), mustSHA(t, testutil.Git(t, dir, "rev-parse", "main")))
			if err != nil {
				t.Fatal(err)
			}
			pathsToCheck := map[string]bool{}
			for i, e := range entries {
				p := string(e.Path)
				if e.Mode == gitsource.ModeSubmodule {
					continue
				}
				pathsToCheck[strings.SplitN(p, "/", 2)[0]] = true
				if i%23 == 0 {
					pathsToCheck[p] = true
					if j := strings.LastIndexByte(p, '/'); j > 0 {
						pathsToCheck[p[:j]] = true
					}
				}
			}
			pathsToCheck["gone/deep"] = true // deleted directory (edge)
			for p := range pathsToCheck {
				got := logAll(t, c, &xylemv1.LogRequest{Path: []byte(p), Page: &xylemv1.PageRequest{PageSize: 5}})
				if want := gitLog(t, dir, p); !slices.Equal(got, want) {
					t.Errorf("path log %q: got %d entries, git %d\n got  %v\n want %v", p, len(got), len(want), got, want)
				}
			}
		})
	}
}

func mustSHA(t *testing.T, s string) gitsource.SHA {
	t.Helper()
	sha, err := gitsource.ParseSHA(s)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestLogErrors(t *testing.T) {
	t.Parallel()
	c := startServer(t)
	importFixture(t, c, "tiny")
	ctx := context.Background()
	for name, req := range map[string]*xylemv1.LogRequest{
		"bad token": {Page: &xylemv1.PageRequest{PageToken: "zz"}},
		"bad path":  {Path: []byte("a//b")},
	} {
		if _, err := c.Repo.Log(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.Repo.Log(ctx, &xylemv1.LogRequest{Revision: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown revision: %v", err)
	}
	resp, err := c.Repo.Log(ctx, &xylemv1.LogRequest{Revision: "main~2", Page: &xylemv1.PageRequest{PageSize: 100}})
	if err != nil || len(resp.Changesets) != 8 || resp.NextPageToken != "" {
		t.Errorf("log of main~2 = %d entries, token %q, %v", len(resp.GetChangesets()), resp.GetNextPageToken(), err)
	}
	if !resp.Changesets[0].OnMainline {
		t.Error("imported history should be on the mainline")
	}
}
