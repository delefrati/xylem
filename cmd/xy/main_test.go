package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/client"
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
	_, err := runXY("land")
	if client.ExitCode(err) != client.ExitFailure || !strings.Contains(err.Error(), "not yet implemented (M6)") {
		t.Errorf("land: %v", err)
	}
	if client.ExitCode(&client.ExitError{Code: client.ExitDirty, Err: errors.New("dirty")}) != client.ExitDirty {
		t.Error("ExitError code not propagated")
	}
	if client.ExitCode(nil) != 0 {
		t.Error("nil error")
	}
}

func TestDialRejectsUnixUntilM8(t *testing.T) {
	t.Parallel()
	if _, err := client.Dial("unix:///run/x.sock"); err == nil || !strings.Contains(err.Error(), "M8") {
		t.Errorf("unix:// = %v", err)
	}
	if _, err := client.Dial(""); err == nil {
		t.Error("empty address accepted")
	}
	c, err := client.Dial("localhost:7400") // lazy: no connection attempt yet
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}
