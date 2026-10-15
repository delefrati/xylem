//go:build integration

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/store"
	"github.com/delefrati/xylem/internal/testutil"
)

func TestGCCommand(t *testing.T) {
	t.Parallel()
	pool := testutil.NewDB(t)
	storeDir := t.TempDir()
	flags := []string{"--db", pool.Config().ConnString(), "--store", "fs://" + storeDir}
	if _, err := run(t, append([]string{"admin", "init-repo", "proj"}, flags...)...); err != nil {
		t.Fatal(err)
	}
	id, stored := object.Encode(object.KindBlob, []byte("never committed\n"))
	if err := store.NewFS(storeDir).Put(context.Background(), id, stored); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, append([]string{"gc"}, flags...)...); err == nil || !strings.Contains(err.Error(), "only --dry-run") {
		t.Errorf("gc without --dry-run: %v", err)
	}
	out, err := run(t, append([]string{"gc", "--dry-run"}, flags...)...)
	if err != nil || !strings.Contains(out, "orphans (unreferenced, older than 168h0m0s): 0 objects") ||
		!strings.Contains(out, "younger than 168h0m0s (kept): 1 objects") {
		t.Errorf("gc --dry-run:\n%s%v", out, err)
	}
	out, err = run(t, append([]string{"gc", "--dry-run", "--min-age", "0s", "--list"}, flags...)...)
	if err != nil || !strings.Contains(out, id.String()+" blob") || !strings.Contains(out, "  blob      1 objects") {
		t.Errorf("gc --dry-run --list:\n%s%v", out, err)
	}
	out, err = run(t, append([]string{"gc", "--dry-run", "--min-age", "0s", "--json", "--list"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Repos     int `json:"repos"`
		Reachable int `json:"reachable"`
		Orphans   struct {
			Objects int `json:"objects"`
		} `json:"orphans"`
		List []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"list"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Repos != 1 || rep.Reachable != 1 || rep.Orphans.Objects != 1 || len(rep.List) != 1 || rep.List[0].ID != id.String() || rep.List[0].Kind != "blob" {
		t.Errorf("json report: %+v", rep)
	}
}
