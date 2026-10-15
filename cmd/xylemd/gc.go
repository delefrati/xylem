package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/delefrati/xylem/internal/gc"
	"github.com/delefrati/xylem/internal/object"
)

func newGCCmd(g *globalFlags) *cobra.Command {
	var (
		dbURL, storeURL     string
		dryRun, list, jsonO bool
		minAge              time.Duration
	)
	cmd := &cobra.Command{
		Use:   "gc --dry-run",
		Short: "Report unreferenced objects (dry run only; the sweep awaits ADR-023)",
		Long: "Marks every object reachable from the changeset root trees and derived value_refs of every\n" +
			"repository, then lists stored objects that are unreferenced and older than --min-age, with\n" +
			"their total bytes by kind. Nothing is deleted: only --dry-run exists until a human approves\n" +
			"the sweep (ADR-023). The store must be listable (fs:// and s3:// are).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !dryRun {
				return errors.New("only --dry-run is implemented: deleting objects waits for ADR-023's approval")
			}
			ctx := cmd.Context()
			d, err := openDeps(ctx, dbURL, storeURL)
			if err != nil {
				return err
			}
			defer d.Close()
			out := cmd.OutOrStdout()
			var orphans []gc.Orphan
			report, err := gc.DryRun(ctx, d.meta, d.objs, gc.Options{MinAge: minAge}, func(o gc.Orphan) error {
				if list && !jsonO {
					_, err := fmt.Fprintf(out, "%s %-9s %10d %s\n", o.ID, kindName(o.Kind), o.Size, o.ModTime.UTC().Format(time.RFC3339))
					return err
				}
				if list {
					orphans = append(orphans, o)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if jsonO {
				return writeGCJSON(out, report, orphans, list)
			}
			return writeGCReport(out, report, minAge)
		},
	}
	addDBFlag(cmd, &dbURL)
	addStoreFlag(cmd, &storeURL)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report only (required)")
	cmd.Flags().DurationVar(&minAge, "min-age", gc.DefaultMinAge, "only unreferenced objects older than this count as orphans")
	cmd.Flags().BoolVar(&list, "list", false, "list each orphan: id, kind, stored bytes, modification time")
	cmd.Flags().BoolVar(&jsonO, "json", false, "print the report as JSON")
	return cmd
}

func kindName(k object.Kind) string {
	if k == 0 {
		return "unknown"
	}
	return k.String()
}

func writeGCReport(w io.Writer, r *gc.Report, minAge time.Duration) error {
	lines := []string{
		fmt.Sprintf("repositories: %d, roots: %d, reachable objects: %d", r.Repos, r.Roots, r.Reachable),
		fmt.Sprintf("stored: %d objects, %d bytes", r.Stored.Objects, r.Stored.Bytes),
		fmt.Sprintf("orphans (unreferenced, older than %v): %d objects, %d bytes", minAge, r.Orphans.Objects, r.Orphans.Bytes),
	}
	kinds := make([]object.Kind, 0, len(r.ByKind))
	for k := range r.ByKind {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	for _, k := range kinds {
		lines = append(lines, fmt.Sprintf("  %-9s %d objects, %d bytes", kindName(k), r.ByKind[k].Objects, r.ByKind[k].Bytes))
	}
	lines = append(lines, fmt.Sprintf("unreferenced but younger than %v (kept): %d objects, %d bytes", minAge, r.Young.Objects, r.Young.Bytes))
	if r.Missing > 0 {
		lines = append(lines, fmt.Sprintf("WARNING: %d reachable objects are missing from the store (run xylemd verify --objects)", r.Missing))
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l); err != nil {
			return err
		}
	}
	return nil
}

func writeGCJSON(w io.Writer, r *gc.Report, orphans []gc.Orphan, list bool) error {
	type count struct {
		Objects int   `json:"objects"`
		Bytes   int64 `json:"bytes"`
	}
	type orphan struct {
		ID      string    `json:"id"`
		Kind    string    `json:"kind"`
		Size    int64     `json:"size"`
		ModTime time.Time `json:"mod_time"`
	}
	v := struct {
		Repos     int              `json:"repos"`
		Roots     int              `json:"roots"`
		Reachable int              `json:"reachable"`
		Stored    count            `json:"stored"`
		Orphans   count            `json:"orphans"`
		ByKind    map[string]count `json:"orphans_by_kind"`
		Young     count            `json:"young"`
		Missing   int              `json:"missing"`
		List      []orphan         `json:"list,omitempty"`
	}{
		Repos: r.Repos, Roots: r.Roots, Reachable: r.Reachable, Missing: r.Missing,
		Stored: count(r.Stored), Orphans: count(r.Orphans), Young: count(r.Young), ByKind: map[string]count{},
	}
	for k, c := range r.ByKind {
		v.ByKind[kindName(k)] = count(*c)
	}
	if list {
		v.List = []orphan{}
		for _, o := range orphans {
			v.List = append(v.List, orphan{o.ID.String(), kindName(o.Kind), o.Size, o.ModTime.UTC()})
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
