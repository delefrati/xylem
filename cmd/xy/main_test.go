package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/client"
	"github.com/delefrati/xylem/internal/object"
)

func runXY(args ...string) (string, error) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	if _, err := runXY("version"); client.ExitCode(err) != client.ExitOK {
		t.Errorf("version: %v", err)
	}
	if _, err := runXY("--no-such-flag"); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("unknown flag: code %d (%v)", client.ExitCode(err), err)
	}
	if _, err := runXY("version", "extra"); client.ExitCode(err) != client.ExitUsage {
		t.Errorf("extra args: code %d (%v)", client.ExitCode(err), err)
	}
	_, err := runXY("-C", t.TempDir(), "land")
	if client.ExitCode(err) != client.ExitFailure || !errors.Is(err, client.ErrNotWorkspace) {
		t.Errorf("land outside a workspace: %v", err)
	}
	if client.ExitCode(&client.ExitError{Code: client.ExitDirty, Err: errors.New("dirty")}) != client.ExitDirty {
		t.Error("ExitError code not propagated")
	}
	if client.ExitCode(nil) != 0 {
		t.Error("nil error")
	}
}

func TestCacheCommands(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := client.Init(dir, client.Config{Server: "localhost:7400", CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	c := client.NewObjectCache(w.Meta("objects"), 0)
	for i := range 3 {
		id, stored := object.Encode(object.KindBlob, []byte{byte(i)})
		if err := c.Put(id, stored); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runXY("-C", dir, "cache", "stats")
	if err != nil || !strings.HasPrefix(out, "3 objects") {
		t.Errorf("stats: %q, %v", out, err)
	}
	out, err = runXY("-C", dir, "cache", "gc", "--target", "0", "--json")
	if err != nil || !strings.Contains(out, `"removed": 3`) {
		t.Errorf("gc: %q, %v", out, err)
	}
	if _, err := runXY("-C", t.TempDir(), "cache", "stats"); err == nil {
		t.Error("cache stats outside a workspace succeeded")
	}
}

func TestCommandName(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]string{
		"":                              "",
		"status":                        "status",
		"-C /tmp/ws log -n 3":           "log",
		"--server localhost:7400 clone": "clone",
		"--json land":                   "land",
		"--repo=x blame f":              "blame",
	} {
		if got := commandName(strings.Fields(args)); got != want {
			t.Errorf("commandName(%q) = %q, want %q", args, got, want)
		}
	}
}
