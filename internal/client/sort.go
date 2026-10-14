package client

import (
	"slices"

	"github.com/delefrati/xylem/internal/treeops"
)

// sortFiles orders files depth-first by path.
func sortFiles(files []FileEntry) []FileEntry {
	slices.SortFunc(files, func(a, b FileEntry) int { return treeops.ComparePathsDFS(a.Path, b.Path) })
	return files
}
