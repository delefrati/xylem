package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/client"
)

// workdir is -C, or the current directory.
func (g *globals) workdir() (string, error) {
	if g.dir != "" {
		return g.dir, nil
	}
	return os.Getwd()
}

// workspace opens the workspace containing the working directory.
func (g *globals) workspace() (*client.Workspace, error) {
	dir, err := g.workdir()
	if err != nil {
		return nil, err
	}
	return client.Open(dir)
}

// cache returns a workspace's local object cache.
func cacheOf(w *client.Workspace) *client.ObjectCache {
	return client.NewObjectCache(w.Meta("objects"), w.Config.CacheMaxBytes)
}

// emit prints v as JSON with --json, or calls human otherwise.
func (g *globals) emit(cmd *cobra.Command, v any, human func(w io.Writer) error) error {
	if g.json {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	return human(cmd.OutOrStdout())
}

func printf(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}
