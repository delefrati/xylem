// Package golden_test pins the hash-affecting encodings (03-data-model.md
// §Canonical encodings) with golden files in testdata/golden. It fails if any
// encoding or id changes. Regenerate with UPDATE_GOLDEN=1 only together with an
// ADR entry (10-testing.md §Golden vectors).
package golden_test

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/delefrati/xylem/internal/canon"
	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

var goldenDir = filepath.Join("..", "..", "testdata", "golden")

type vector struct {
	name   string
	kind   object.Kind
	inputs string // human-readable description of the inputs
	body   []byte
}

func (v vector) id() hash.ID { return object.ID(v.kind, v.body) }

func blobID(content string) hash.ID { return object.ID(object.KindBlob, []byte(content)) }

func mustTree(t *testing.T, entries ...object.Entry) []byte {
	t.Helper()
	b, err := object.EncodeTree(&object.Tree{Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustChangeset(t *testing.T, c *changeset.Changeset) []byte {
	t.Helper()
	b, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func file(name, content string) object.Entry {
	return object.Entry{Mode: object.ModeFile, Name: []byte(name), ID: blobID(content), Size: uint64(len(content))}
}

func put(path, content string, mode object.Mode) changeset.Change {
	return changeset.Change{Path: []byte(path), Op: changeset.OpPut, Mode: mode, Size: uint64(len(content)), Blob: blobID(content)}
}

func sig(nameEmail string, unix int64, tz int32) changeset.Signature {
	return changeset.Signature{NameEmail: []byte(nameEmail), Time: unix, TZMinutes: tz}
}

const t0 = 1700000000 // 2023-11-14T22:13:20Z

func vectors(t *testing.T) []vector {
	t.Helper()
	var vs []vector
	addTree := func(name string, entries ...object.Entry) hash.ID {
		v := vector{name: name, kind: object.KindTree, inputs: describeTree(entries), body: mustTree(t, entries...)}
		vs = append(vs, v)
		return v.id()
	}
	addChangeset := func(name string, c *changeset.Changeset) hash.ID {
		v := vector{name: name, kind: object.KindChangeset, inputs: describeChangeset(c), body: mustChangeset(t, c)}
		vs = append(vs, v)
		return v.id()
	}

	// Blobs (03 §Hashing example: an empty blob hashes "blob 0\x00").
	vs = append(vs,
		vector{name: "blob_empty", kind: object.KindBlob, inputs: "Empty file.", body: []byte{}},
		vector{name: "blob_hello", kind: object.KindBlob, inputs: "File content `\"hello\\n\"` (6 bytes).", body: []byte("hello\n")},
	)

	// Trees.
	addTree("tree_empty")
	everyMode := addTree("tree_every_mode",
		file("README", "hello\n"),
		object.Entry{Mode: object.ModeExec, Name: []byte("build.sh"), ID: blobID("#!/bin/sh\n"), Size: 10},
		object.Entry{Mode: object.ModeSymlink, Name: []byte("link"), ID: blobID("README"), Size: 6},
		object.Entry{Mode: object.ModeTree, Name: []byte("src"), ID: object.ID(object.KindTree, mustTree(t, file("main.go", "package main\n")))},
	)
	addTree("tree_sorting",
		file("A", "1"),
		file("a", "2"),
		file("a-b", "3"),
		object.Entry{Mode: object.ModeTree, Name: []byte("a.b"), ID: everyMode},
		file("a0", "4"),
		file("aB", "5"),
		file("ab", "6"),
		file("a\xff", "7"),
	)
	addTree("tree_sizes",
		object.Entry{Mode: object.ModeFile, Name: []byte("s0"), ID: blobID(""), Size: 0},
		object.Entry{Mode: object.ModeFile, Name: []byte("s127"), ID: blobID("x"), Size: 127},
		object.Entry{Mode: object.ModeFile, Name: []byte("s128"), ID: blobID("x"), Size: 128},
		object.Entry{Mode: object.ModeFile, Name: []byte("s16384"), ID: blobID("x"), Size: 16384},
		object.Entry{Mode: object.ModeFile, Name: []byte("s64MiB"), ID: blobID("x"), Size: 64 << 20},
		object.Entry{Mode: object.ModeFile, Name: []byte("smax"), ID: blobID("x"), Size: math.MaxUint64},
	)
	vs[len(vs)-1].inputs = "Sizes are synthetic, chosen at uvarint length boundaries (1, 2, 3, 4 and 10 bytes); " +
		"real trees carry each blob's actual length. " + vs[len(vs)-1].inputs
	addTree("tree_nested",
		object.Entry{Mode: object.ModeTree, Name: []byte("dir"), ID: everyMode},
		file("top.txt", "top\n"),
	)

	// Changesets.
	addChangeset("changeset_empty_root", &changeset.Changeset{
		Author:    sig("Xylem <xylem@localhost>", t0, 0),
		Committer: sig("Xylem <xylem@localhost>", t0, 0),
		Message:   []byte("Initialize repository\n"),
	})
	root := addChangeset("changeset_root", &changeset.Changeset{
		Author:    sig("Ada Lovelace <ada@example.com>", t0, 0),
		Committer: sig("Ada Lovelace <ada@example.com>", t0, 0),
		Message:   []byte("Initial commit\n"),
		Changes: []changeset.Change{
			put("README", "hello\n", object.ModeFile),
			put("build.sh", "#!/bin/sh\n", object.ModeExec),
			put("link", "README", object.ModeSymlink),
			put("src/main.go", "package main\n", object.ModeFile),
		},
	})
	child := addChangeset("changeset_child", &changeset.Changeset{
		Parents:   []hash.ID{root},
		Author:    sig("Ada Lovelace <ada@example.com>", t0+3600, 60),
		Committer: sig("Grace Hopper <grace@example.com>", t0+7200, -300),
		Message:   []byte("Say hello to the world\n\nLonger body.\n"),
		Changes:   []changeset.Change{put("README", "hello world\n", object.ModeFile)},
	})
	side := addChangeset("changeset_deletes", &changeset.Changeset{
		Parents:   []hash.ID{root},
		Author:    sig("Grace Hopper <grace@example.com>", t0+60, -300),
		Committer: sig("Grace Hopper <grace@example.com>", t0+60, -300),
		Message:   []byte("Remove the link, make build.sh a plain file\n"),
		Changes: []changeset.Change{
			{Path: []byte("link"), Op: changeset.OpDelete},
			put("build.sh", "#!/bin/sh\n", object.ModeFile),
			{Path: []byte("src/main.go"), Op: changeset.OpDelete},
			put("src/lib.go", "package src\n", object.ModeFile),
		},
	})
	addChangeset("changeset_merge", &changeset.Changeset{
		Parents:   []hash.ID{child, side},
		Author:    sig("Ada Lovelace <ada@example.com>", t0+9000, 0),
		Committer: sig("Ada Lovelace <ada@example.com>", t0+9000, 0),
		Message:   []byte("Merge branch 'cleanup'\n"),
		Changes: []changeset.Change{
			{Path: []byte("link"), Op: changeset.OpDelete},
			put("build.sh", "#!/bin/sh\n", object.ModeFile),
			{Path: []byte("src/main.go"), Op: changeset.OpDelete},
			put("src/lib.go", "package src\n", object.ModeFile),
		},
	})
	addChangeset("changeset_merge_no_changes", &changeset.Changeset{
		Parents:   []hash.ID{child, side, root},
		Author:    sig("Ada Lovelace <ada@example.com>", t0+9100, 0),
		Committer: sig("Ada Lovelace <ada@example.com>", t0+9100, 0),
		Message:   []byte("Octopus merge, first parent wins\n"),
	})
	addChangeset("changeset_extras", &changeset.Changeset{
		Parents:   []hash.ID{child},
		Author:    sig("Ada Lovelace <ada@example.com>", t0+10000, 0),
		Committer: sig("Xylem <xylem@localhost>", t0+10001, 0),
		Message:   []byte("Imported with extras\n"),
		Extras: map[string][]byte{
			changeset.ExtraGitSHA:      []byte("0123456789abcdef0123456789abcdef01234567"),
			changeset.ExtraRebasedFrom: []byte(child.String()),
			"zz-unknown":               {0x00, 0xff},
			"empty":                    {},
		},
		Changes: []changeset.Change{put("NEW", "new\n", object.ModeFile)},
	})
	addChangeset("changeset_non_utf8", &changeset.Changeset{
		Parents:   []hash.ID{root},
		Author:    sig("J\xf6rg M\xfcller <joerg@example.de>", -1, math.MinInt32),
		Committer: sig("J\xf6rg M\xfcller <joerg@example.de>", math.MaxInt64, math.MaxInt32),
		Message:   []byte("Gr\xfc\xdfe aus K\xf6ln\x00\n"),
		Extras:    map[string][]byte{changeset.ExtraGitEncoding: []byte("ISO-8859-1")},
		Changes: []changeset.Change{
			put("docs/\xc4nderungen.txt", "\xe4\xf6\xfc\n", object.ModeFile),
			put("\xff\xfe/bin", "\x00\x01", object.ModeExec),
		},
	})
	return vs
}

func TestGolden(t *testing.T) {
	vs := vectors(t)
	files := map[string][]byte{"README.md": readme(t, vs)}
	for _, v := range vs {
		files[v.name+".bin"] = v.body
		files[v.name+".id"] = []byte(v.id().String() + "\n")
	}

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.RemoveAll(goldenDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(goldenDir, 0o750); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(goldenDir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("wrote %d golden files to %s; an encoding change also needs an ADR", len(files), goldenDir)
		return
	}

	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("reading golden dir (generate with UPDATE_GOLDEN=1): %v", err)
	}
	for _, e := range entries {
		if _, ok := files[e.Name()]; !ok {
			t.Errorf("unexpected golden file %s", e.Name())
		}
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(goldenDir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the golden file: a hash-affecting encoding changed", name)
		}
	}
}

// Decoding each golden body and re-encoding it must reproduce it exactly.
func TestGoldenDecodeRoundTrip(t *testing.T) {
	for _, v := range vectors(t) {
		var again []byte
		var err error
		switch v.kind {
		case object.KindBlob:
			continue
		case object.KindTree:
			var tr *object.Tree
			if tr, err = object.DecodeTree(v.body); err == nil {
				again, err = object.EncodeTree(tr)
			}
		case object.KindChangeset:
			var c *changeset.Changeset
			if c, err = changeset.Decode(v.body); err == nil {
				again, err = c.Encode()
			}
		}
		if err != nil || !bytes.Equal(again, v.body) {
			t.Errorf("%s: decode/re-encode: %v", v.name, err)
		}
		if lines := annotate(v); len(lines) == 0 {
			t.Errorf("%s: no annotation", v.name)
		}
	}
}

// ---- README generation

func readme(t *testing.T, vs []vector) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString(`# Golden vectors

Generated by ` + "`test/golden/golden_test.go`" + `. Do not edit by hand.

These files freeze the hash-affecting encodings of ` + "`docs/03-data-model.md`" + ` §Canonical encodings
(ADR-004, ADR-006, ADR-015, ADR-017). ` + "`go test ./test/golden`" + ` fails if any byte or id changes.
Regenerating (` + "`UPDATE_GOLDEN=1 go test ./test/golden`" + `) requires an ADR entry.

For each vector:

- ` + "`<name>.bin`" + ` is the object **body** (no header, not compressed).
- ` + "`<name>.id`" + ` is the object id: ` + "`BLAKE3-256(\"<kind> <len>\\x00\" ‖ body)`" + `, in hex.

Ids written ` + "`blob(\"…\")`" + ` below are the ids of blobs with that content. Times are Unix seconds;
` + "`t0`" + ` = 1700000000 (2023-11-14T22:13:20Z). Byte strings are shown Go-quoted.

| Vector | Kind | Body bytes | Id |
|---|---|---|---|
`)
	for _, v := range vs {
		fmt.Fprintf(&b, "| [%s](#%s) | %s | %d | `%s` |\n", v.name, v.name, v.kind, len(v.body), v.id())
	}
	for _, v := range vs {
		fmt.Fprintf(&b, "\n## %s\n\n**Inputs.** %s\n\n", v.name, v.inputs)
		fmt.Fprintf(&b, "**Header.** `%s`\n\n", strconv.Quote(string(object.Header(v.kind, len(v.body)))))
		fmt.Fprintf(&b, "**Id.** `%s`\n\n", v.id())
		if v.kind != object.KindBlob {
			b.WriteString("**Body, field by field.**\n\n| Offset | Bytes | Field |\n|---:|---|---|\n")
			for _, l := range annotate(v) {
				fmt.Fprintf(&b, "| %d | `%s` | %s |\n", l.offset, l.hex, l.field)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**Body hex** (%d bytes).\n\n```\n%s```\n", len(v.body), hexDump(v.body))
	}
	return []byte(b.String())
}

func hexDump(b []byte) string {
	if len(b) == 0 {
		return "(empty)\n"
	}
	var s strings.Builder
	for i := 0; i < len(b); i += 32 {
		s.WriteString(hex.EncodeToString(b[i:min(i+32, len(b))]))
		s.WriteString("\n")
	}
	return s.String()
}

func q(b []byte) string { return strconv.Quote(string(b)) }

func idName(id hash.ID) string {
	for _, c := range []string{
		"", "hello\n", "hello world\n", "#!/bin/sh\n", "README", "package main\n", "package src\n",
		"x", "1", "2", "3", "4", "5", "6", "7", "top\n", "new\n", "\xe4\xf6\xfc\n", "\x00\x01",
	} {
		if id == blobID(c) {
			return "blob(" + strconv.Quote(c) + ")"
		}
	}
	return id.Short(16) + "…"
}

func describeTree(entries []object.Entry) string {
	if len(entries) == 0 {
		return "No entries (an empty root tree)."
	}
	var b strings.Builder
	b.WriteString("Entries:\n\n| Mode | Name | Id | Size |\n|---|---|---|---|\n")
	for _, e := range entries {
		size := strconv.FormatUint(e.Size, 10)
		if e.Mode == object.ModeTree {
			size = "—"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", strings.ToUpper(e.Mode.String()), q(e.Name), idName(e.ID), size)
	}
	return b.String()
}

func describeSig(s changeset.Signature) string {
	return fmt.Sprintf("`%s`, time %d, tz %+d min", q(s.NameEmail), s.Time, s.TZMinutes)
}

func describeChangeset(c *changeset.Changeset) string {
	var b strings.Builder
	b.WriteString("\n\n")
	if len(c.Parents) == 0 {
		b.WriteString("- Parents: none (root)\n")
	}
	for i, p := range c.Parents {
		fmt.Fprintf(&b, "- Parent %d: `%s`\n", i, p)
	}
	fmt.Fprintf(&b, "- Author: %s\n- Committer: %s\n- Message: `%s`\n", describeSig(c.Author), describeSig(c.Committer), q(c.Message))
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range c.Extras {
			if !yield(k) {
				return
			}
		}
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "- Extra `%s` = `%s`\n", strconv.Quote(k), q(c.Extras[k]))
	}
	if len(c.Changes) == 0 {
		b.WriteString("- Changes: none\n")
	}
	for _, ch := range c.Changes {
		if ch.Op == changeset.OpDelete {
			fmt.Fprintf(&b, "- DELETE `%s`\n", q(ch.Path))
		} else {
			fmt.Fprintf(&b, "- PUT `%s` %s, size %d, %s\n", q(ch.Path), strings.ToUpper(ch.Mode.String()), ch.Size, idName(ch.Blob))
		}
	}
	return b.String()
}

// ---- independent annotation of encoded bodies

type line struct {
	offset int
	hex    string
	field  string
}

type annotator struct {
	body  []byte
	r     *canon.Reader
	lines []line
	err   error
}

func (a *annotator) mark(start int, field string) {
	end := len(a.body) - a.r.Len()
	a.lines = append(a.lines, line{start, abbrevHex(a.body[start:end]), field})
}

func (a *annotator) pos() int { return len(a.body) - a.r.Len() }

func (a *annotator) uvarint(field string) uint64 {
	start := a.pos()
	v, err := a.r.Uvarint()
	a.fail(err)
	a.mark(start, fmt.Sprintf("%s = %d (uvarint)", field, v))
	return v
}

func (a *annotator) varint(field string) int64 {
	start := a.pos()
	v, err := a.r.Varint()
	a.fail(err)
	a.mark(start, fmt.Sprintf("%s = %d (zig-zag varint)", field, v))
	return v
}

func (a *annotator) bytes(field string) []byte {
	start := a.pos()
	v, err := a.r.Bytes()
	a.fail(err)
	a.mark(start, fmt.Sprintf("%s = `%s` (uvarint length %d ‖ bytes)", field, q(v), len(v)))
	return v
}

func (a *annotator) byte(field string, names map[byte]string) byte {
	start := a.pos()
	v, err := a.r.Byte()
	a.fail(err)
	a.mark(start, fmt.Sprintf("%s = 0x%02x %s (u8)", field, v, names[v]))
	return v
}

func (a *annotator) id(field string) {
	start := a.pos()
	v, err := a.r.ID()
	a.fail(err)
	a.mark(start, fmt.Sprintf("%s = %s (32 bytes)", field, idName(v)))
}

func (a *annotator) fail(err error) {
	if err != nil && a.err == nil {
		a.err = err
	}
}

func abbrevHex(b []byte) string {
	if len(b) > 12 {
		return hex.EncodeToString(b[:6]) + "…" + hex.EncodeToString(b[len(b)-2:])
	}
	return hex.EncodeToString(b)
}

var modeNames = map[byte]string{1: "FILE", 2: "EXEC", 3: "SYMLINK", 4: "TREE"}

func annotate(v vector) []line {
	a := &annotator{body: v.body, r: canon.NewReader(v.body)}
	switch v.kind {
	case object.KindTree:
		n := a.uvarint("entry_count")
		for i := range n {
			mode := a.byte(fmt.Sprintf("entry %d mode", i), modeNames)
			a.bytes(fmt.Sprintf("entry %d name", i))
			a.id(fmt.Sprintf("entry %d id", i))
			if mode != 4 {
				a.uvarint(fmt.Sprintf("entry %d size", i))
			}
		}
	case object.KindChangeset:
		a.uvarint("format version")
		n := a.uvarint("parent_count")
		for i := range n {
			a.id(fmt.Sprintf("parent %d", i))
		}
		for _, who := range []string{"author", "committer"} {
			a.bytes(who)
			a.varint(who + " time")
			a.varint(who + " tz minutes")
		}
		a.bytes("message")
		n = a.uvarint("extra_count")
		for i := range n {
			a.bytes(fmt.Sprintf("extra %d key", i))
			a.bytes(fmt.Sprintf("extra %d value", i))
		}
		n = a.uvarint("change_count")
		for i := range n {
			a.bytes(fmt.Sprintf("change %d path", i))
			op := a.byte(fmt.Sprintf("change %d op", i), map[byte]string{1: "PUT", 2: "DELETE"})
			if op == 1 {
				a.byte(fmt.Sprintf("change %d mode", i), modeNames)
				a.uvarint(fmt.Sprintf("change %d size", i))
				a.id(fmt.Sprintf("change %d blob", i))
			}
		}
	default:
		return nil
	}
	if a.err != nil || a.r.Len() != 0 {
		return nil
	}
	return a.lines
}
