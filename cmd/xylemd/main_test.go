package main

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// Not parallel: subtests set the link-time version and commit variables.
func TestVersionCommand(t *testing.T) {
	goInfo := fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	tests := []struct {
		name       string
		version    string
		commit     string
		wantExact  string
		wantPrefix string
		wantSuffix string
	}{
		{
			name:      "link-time values",
			version:   "v1.2.3-4-gabcdef0",
			commit:    "abcdef0",
			wantExact: "xylemd v1.2.3-4-gabcdef0 (commit abcdef0, " + goInfo + ")\n",
		},
		{
			name:       "defaults fall back to build info",
			version:    "dev",
			commit:     "",
			wantPrefix: "xylemd dev (commit ",
			wantSuffix: ", " + goInfo + ")\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldVersion, oldCommit := version, commit
			t.Cleanup(func() { version, commit = oldVersion, oldCommit })
			version, commit = tt.version, tt.commit

			var out bytes.Buffer
			root := newRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"version"})
			if err := root.Execute(); err != nil {
				t.Fatalf("xylemd version: %v", err)
			}

			got := out.String()
			if tt.wantExact != "" && got != tt.wantExact {
				t.Errorf("output = %q, want %q", got, tt.wantExact)
			}
			if !strings.HasPrefix(got, tt.wantPrefix) || !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("output = %q, want prefix %q and suffix %q", got, tt.wantPrefix, tt.wantSuffix)
			}
		})
	}
}

func TestVersionRejectsArgs(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"version", "extra"})
	if err := root.Execute(); err == nil {
		t.Fatal("xylemd version extra: want error, got nil")
	}
}
