package repo

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

func TestResolveChange(t *testing.T) {
	t.Parallel()
	id := func(s string) hash.ID { return object.ID(object.KindBlob, []byte(s)) }
	file := func(content string) *object.Entry {
		return &object.Entry{Mode: object.ModeFile, ID: id(content), Size: uint64(len(content))}
	}
	exec := func(content string) *object.Entry {
		return &object.Entry{Mode: object.ModeExec, ID: id(content), Size: uint64(len(content))}
	}
	link := func(target string) *object.Entry {
		return &object.Entry{Mode: object.ModeSymlink, ID: id(target), Size: uint64(len(target))}
	}
	dir := &object.Entry{Mode: object.ModeTree, ID: object.ID(object.KindTree, []byte("t"))}
	dir2 := &object.Entry{Mode: object.ModeTree, ID: object.ID(object.KindTree, []byte("t2"))}
	put := func(e *object.Entry) changeset.Change {
		return changeset.Change{Path: []byte("p"), Op: changeset.OpPut, Mode: e.Mode, Blob: e.ID, Size: e.Size}
	}
	del := changeset.Change{Path: []byte("p"), Op: changeset.OpDelete}
	keep := func(ch changeset.Change) *changeset.Change { return &ch }

	for _, tc := range []struct {
		name string
		b, o *object.Entry
		ch   changeset.Change
		want *changeset.Change
		kind ConflictKind
	}{
		// b == o: p untouched on onto since base → keep.
		{"untouched, modify", file("a"), file("a"), put(file("x")), keep(put(file("x"))), 0},
		{"untouched, delete", file("a"), file("a"), del, keep(del), 0},
		{"untouched, add", nil, nil, put(file("x")), keep(put(file("x"))), 0},
		{"untouched dir replaced by a file", dir, dir, put(file("x")), keep(put(file("x"))), 0},
		// o == x: already applied → drop.
		{"same modification", file("a"), file("x"), put(file("x")), nil, 0},
		{"same add", nil, file("x"), put(file("x")), nil, 0},
		{"both deleted", file("a"), nil, del, nil, 0},
		// Content changed differently on both sides.
		{"content/content", file("a"), file("o"), put(file("x")), nil, ConflictContent},
		{"content/content with symlinks", link("a"), link("o"), put(link("x")), nil, ConflictContent},
		// Delete vs modify, both ways.
		{"deleted on onto, modified", file("a"), nil, put(file("x")), nil, ConflictDeleteModify},
		{"modified on onto, deleted", file("a"), file("o"), del, nil, ConflictDeleteModify},
		{"mode changed on onto, deleted", file("a"), exec("a"), del, nil, ConflictDeleteModify},
		// Add/add with different results.
		{"add/add", nil, file("o"), put(file("x")), nil, ConflictAddAdd},
		{"add/add, mode only differs", nil, file("x"), put(exec("x")), nil, ConflictAddAdd},
		{"dir replaced on both sides", dir, file("o"), put(file("x")), nil, ConflictAddAdd},
		// File/directory.
		{"onto has a directory", file("a"), dir, put(file("x")), nil, ConflictFileDirectory},
		{"onto made a directory, deleted", file("a"), dir, del, nil, ConflictFileDirectory},
		{"onto has a new directory", nil, dir, put(file("x")), nil, ConflictFileDirectory},
		{"base dir changed on onto", dir, dir2, put(file("x")), nil, ConflictFileDirectory},
		// Mode on one side, content on the other: both survive.
		{"mode on onto, content here", file("a"), exec("a"), put(file("x")), keep(put(exec("x"))), 0},
		{"content on onto, mode here", file("a"), file("o"), put(exec("a")), keep(put(exec("o"))), 0},
		{"both changed mode identically, content here", file("a"), exec("a"), put(exec("x")), keep(put(exec("x"))), 0},
		{"both changed content identically, mode here", file("a"), file("x"), put(exec("x")), keep(put(exec("x"))), 0},
		{"merged result equals onto", file("a"), exec("x"), put(exec("a")), nil, 0},
		// Mode changed differently on both sides.
		{"mode/mode", file("a"), exec("a"), put(link("a")), nil, ConflictMode},
		{"mode/mode and content: content wins", file("a"), exec("o"), put(link("x")), nil, ConflictContent},
	} {
		got, kind := ResolveChange(tc.b, tc.o, tc.ch)
		if kind != tc.kind || !cmp.Equal(got, tc.want) {
			t.Errorf("%s: got %+v, %v; want %+v, %v", tc.name, got, kind, tc.want, tc.kind)
		}
	}
}
