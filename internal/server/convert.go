package server

import (
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/changeset"
	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
	"github.com/delefrati/xylem/internal/repo"
)

// Proto ↔ domain conversions (05-api.md).

func toProtoMode(m object.Mode) xylemv1.FileMode {
	switch m {
	case object.ModeFile:
		return xylemv1.FileMode_FILE_MODE_FILE
	case object.ModeExec:
		return xylemv1.FileMode_FILE_MODE_EXEC
	case object.ModeSymlink:
		return xylemv1.FileMode_FILE_MODE_SYMLINK
	case object.ModeTree:
		return xylemv1.FileMode_FILE_MODE_TREE
	}
	return xylemv1.FileMode_FILE_MODE_UNSPECIFIED
}

func fromProtoMode(m xylemv1.FileMode) (object.Mode, error) {
	switch m {
	case xylemv1.FileMode_FILE_MODE_FILE:
		return object.ModeFile, nil
	case xylemv1.FileMode_FILE_MODE_EXEC:
		return object.ModeExec, nil
	case xylemv1.FileMode_FILE_MODE_SYMLINK:
		return object.ModeSymlink, nil
	case xylemv1.FileMode_FILE_MODE_TREE:
		return object.ModeTree, nil
	}
	return 0, fmt.Errorf("%w: file mode %v", repo.ErrInvalid, m)
}

func toProtoSignature(s changeset.Signature) *xylemv1.Signature {
	return &xylemv1.Signature{
		NameEmail:       s.NameEmail,
		Time:            timestamppb.New(time.Unix(s.Time, 0)),
		TzOffsetMinutes: s.TZMinutes,
	}
}

// fromProtoSignature rejects a missing time or a time with nanoseconds (05:
// only seconds are significant; nanos must be 0).
func fromProtoSignature(s *xylemv1.Signature) (changeset.Signature, error) {
	if s == nil || s.GetTime() == nil {
		return changeset.Signature{}, fmt.Errorf("%w: signature needs a time", repo.ErrInvalid)
	}
	if s.GetTime().GetNanos() != 0 {
		return changeset.Signature{}, fmt.Errorf("%w: signature time must be whole seconds (nanos = %d)", repo.ErrInvalid, s.GetTime().GetNanos())
	}
	return changeset.Signature{NameEmail: s.GetNameEmail(), Time: s.GetTime().GetSeconds(), TZMinutes: s.GetTzOffsetMinutes()}, nil
}

func toProtoChange(ch changeset.Change) *xylemv1.FileChange {
	fc := &xylemv1.FileChange{Path: ch.Path}
	switch ch.Op {
	case changeset.OpPut:
		fc.Op = xylemv1.FileChange_OP_PUT
		fc.Mode = toProtoMode(ch.Mode)
		fc.Size = ch.Size
		fc.BlobId = ch.Blob[:]
	case changeset.OpDelete:
		fc.Op = xylemv1.FileChange_OP_DELETE
	}
	return fc
}

func fromProtoChange(fc *xylemv1.FileChange) (changeset.Change, error) {
	ch := changeset.Change{Path: fc.GetPath()}
	switch fc.GetOp() {
	case xylemv1.FileChange_OP_PUT:
		ch.Op = changeset.OpPut
		mode, err := fromProtoMode(fc.GetMode())
		if err != nil {
			return ch, err
		}
		blob, err := idFrom(fc.GetBlobId(), "blob_id")
		if err != nil {
			return ch, err
		}
		ch.Mode, ch.Size, ch.Blob = mode, fc.GetSize(), blob
	case xylemv1.FileChange_OP_DELETE:
		ch.Op = changeset.OpDelete
		if fc.GetMode() != xylemv1.FileMode_FILE_MODE_UNSPECIFIED || fc.GetSize() != 0 || len(fc.GetBlobId()) != 0 {
			return ch, fmt.Errorf("%w: DELETE of %q must not set mode, size or blob_id", repo.ErrInvalid, fc.GetPath())
		}
	default:
		return ch, fmt.Errorf("%w: change %q has op %v", repo.ErrInvalid, fc.GetPath(), fc.GetOp())
	}
	return ch, nil
}

// idFrom parses a 32-byte id from the wire.
func idFrom(b []byte, field string) (hash.ID, error) {
	var id hash.ID
	if len(b) != hash.Size {
		return id, fmt.Errorf("%w: %s has %d bytes, want %d", repo.ErrInvalid, field, len(b), hash.Size)
	}
	copy(id[:], b)
	return id, nil
}

// toProtoChangeset converts a stored changeset. on_mainline refers to main.
func toProtoChangeset(cs *repo.Changeset, onMainline, includeChanges bool) *xylemv1.Changeset {
	p := &xylemv1.Changeset{
		Id:               cs.ID[:],
		Seq:              uint64(max(cs.Seq, 0)),
		Parents:          make([][]byte, len(cs.Parents)),
		Author:           toProtoSignature(cs.Author),
		Committer:        toProtoSignature(cs.Committer),
		Message:          cs.Message,
		Extras:           cs.Extras,
		RootTree:         cs.Core.RootTree[:],
		Generation:       uint64(max(cs.Core.Gen, 0)),
		FirstParentDepth: uint64(max(cs.Core.FPDepth, 0)),
		OnMainline:       onMainline,
		GitSha:           string(cs.Extras[changeset.ExtraGitSHA]),
		ChangeCount:      clampUint32(len(cs.Changes)),
	}
	for i := range cs.Parents {
		p.Parents[i] = cs.Parents[i][:]
	}
	if includeChanges {
		p.Changes = make([]*xylemv1.FileChange, len(cs.Changes))
		for i, ch := range cs.Changes {
			p.Changes[i] = toProtoChange(ch)
		}
	}
	return p
}

func toProtoRef(r repo.Ref) *xylemv1.Ref {
	return &xylemv1.Ref{Name: r.Name, Id: r.ID[:], Version: uint64(max(r.Version, 0))}
}

// clampUint32 converts a non-negative count, clamping to the uint32 range.
func clampUint32(n int) uint32 {
	if n < 0 {
		return 0
	}
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}
