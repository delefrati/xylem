package server

import (
	"errors"
	"math"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/meta"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
)

func TestModeRoundTrip(t *testing.T) {
	t.Parallel()
	for _, m := range []object.Mode{object.ModeFile, object.ModeExec, object.ModeSymlink, object.ModeTree} {
		got, err := fromProtoMode(toProtoMode(m))
		if err != nil || got != m {
			t.Errorf("mode %v round trip = %v, %v", m, got, err)
		}
	}
	if _, err := fromProtoMode(xylemv1.FileMode_FILE_MODE_UNSPECIFIED); !errors.Is(err, repo.ErrInvalid) {
		t.Errorf("unspecified mode: %v", err)
	}
	if toProtoMode(0) != xylemv1.FileMode_FILE_MODE_UNSPECIFIED {
		t.Error("invalid mode not mapped to UNSPECIFIED")
	}
}

func TestSignatureRoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range []changeset.Signature{
		{NameEmail: []byte("J\xf6rg <j@x>"), Time: 1700000000, TZMinutes: -180},
		{NameEmail: []byte("A <a@x>"), Time: -1, TZMinutes: 330},
		{NameEmail: nil, Time: 0, TZMinutes: math.MinInt32},
	} {
		got, err := fromProtoSignature(toProtoSignature(s))
		if err != nil || !cmp.Equal(got.NameEmail, s.NameEmail) || got.Time != s.Time || got.TZMinutes != s.TZMinutes {
			t.Errorf("signature %+v round trip = %+v, %v", s, got, err)
		}
	}
	bad := []*xylemv1.Signature{
		nil,
		{NameEmail: []byte("A")},
		{NameEmail: []byte("A"), Time: &timestamppb.Timestamp{Seconds: 1, Nanos: 5}},
	}
	for _, b := range bad {
		if _, err := fromProtoSignature(b); !errors.Is(err, repo.ErrInvalid) {
			t.Errorf("signature %v: %v", b, err)
		}
	}
}

func TestChangeRoundTrip(t *testing.T) {
	t.Parallel()
	for _, ch := range []changeset.Change{
		{Path: []byte("a/\xff"), Op: changeset.OpPut, Mode: object.ModeExec, Size: 42, Blob: hash.ID{7}},
		{Path: []byte("link"), Op: changeset.OpPut, Mode: object.ModeSymlink, Size: 3, Blob: hash.ID{8}},
		{Path: []byte("gone"), Op: changeset.OpDelete},
	} {
		got, err := fromProtoChange(toProtoChange(ch))
		if err != nil || !cmp.Equal(got, ch) {
			t.Errorf("change %+v round trip = %+v, %v", ch, got, err)
		}
	}
	bad := []*xylemv1.FileChange{
		{Path: []byte("x")}, // OP_UNSPECIFIED
		{Path: []byte("x"), Op: xylemv1.FileChange_OP_PUT, Mode: xylemv1.FileMode_FILE_MODE_FILE, BlobId: []byte{1}},
		{Path: []byte("x"), Op: xylemv1.FileChange_OP_PUT, BlobId: make([]byte, 32)},
		{Path: []byte("x"), Op: xylemv1.FileChange_OP_DELETE, Size: 1},
	}
	for _, b := range bad {
		if _, err := fromProtoChange(b); !errors.Is(err, repo.ErrInvalid) {
			t.Errorf("change %v: %v", b, err)
		}
	}
}

func TestToProtoChangeset(t *testing.T) {
	t.Parallel()
	cs := &repo.Changeset{
		ID: hash.ID{1}, Seq: 9,
		Changeset: changeset.Changeset{
			Parents:   []hash.ID{{2}, {3}},
			Author:    changeset.Signature{NameEmail: []byte("A <a>"), Time: 5},
			Committer: changeset.Signature{NameEmail: []byte("C <c>"), Time: 6, TZMinutes: 60},
			Message:   []byte("m\n"),
			Extras:    map[string][]byte{changeset.ExtraGitSHA: []byte("abcd")},
			Changes:   []changeset.Change{{Path: []byte("f"), Op: changeset.OpDelete}},
		},
		Core: meta.CoreRow{RootTree: hash.ID{4}, Gen: 3, FPDepth: 2},
	}
	p := toProtoChangeset(cs, true, false)
	if len(p.Changes) != 0 || p.ChangeCount != 1 || p.GitSha != "abcd" || !p.OnMainline || p.Generation != 3 || p.FirstParentDepth != 2 ||
		len(p.Parents) != 2 || p.Parents[1][0] != 3 || p.RootTree[0] != 4 || p.Seq != 9 || p.Committer.TzOffsetMinutes != 60 {
		t.Errorf("toProtoChangeset = %v", p)
	}
	if with := toProtoChangeset(cs, false, true); len(with.Changes) != 1 || with.OnMainline {
		t.Errorf("include_changes: %v", with)
	}
	if clampUint32(-1) != 0 || clampUint32(5) != 5 {
		t.Error("clampUint32")
	}
}
