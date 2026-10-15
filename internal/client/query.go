package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/diff"
	"github.com/delefrati/xylem/internal/hash"
)

// ResolveRev expands the client-side revision "@" (the workspace parent,
// optionally followed by ~N or ^N) into a revision the server understands
// (03 §Revision syntax). Other revisions pass through unchanged.
func ResolveRev(rev string, w *Workspace) (string, error) {
	rest, ok := strings.CutPrefix(rev, "@")
	if !ok || rest != "" && rest[0] != '~' && rest[0] != '^' {
		return rev, nil
	}
	if w == nil {
		return "", Usage(errors.New(`"@" needs a workspace`))
	}
	if w.State.Parent.IsZero() {
		return "", errors.New(`"@": the workspace has no parent yet`)
	}
	return w.State.Parent.String() + rest, nil
}

// Signature is a changeset signature in JSON.
type Signature struct {
	NameEmail Path      `json:"name_email"`
	Time      time.Time `json:"time"`
	TZOffset  int32     `json:"tz_offset_minutes"`
}

// Name returns the name part of "Name <email>".
func (s Signature) Name() string {
	n, _, _ := strings.Cut(string(s.NameEmail), " <")
	return n
}

// LocalTime is the signature time in its own time zone.
func (s Signature) LocalTime() time.Time {
	return s.Time.In(time.FixedZone("", int(s.TZOffset)*60))
}

// ChangesetInfo is changeset metadata in JSON (the --json output of log,
// show and blame). Free-form byte fields use Path's encoding.
type ChangesetInfo struct {
	ID          string    `json:"id"`
	Seq         uint64    `json:"seq"`
	Parents     []string  `json:"parents"`
	Author      Signature `json:"author"`
	Committer   Signature `json:"committer"`
	Message     Path      `json:"message"`
	Generation  uint64    `json:"generation"`
	OnMainline  bool      `json:"on_mainline"`
	GitSHA      string    `json:"git_sha,omitempty"`
	ChangeCount uint32    `json:"change_count"`
}

func signatureOf(s *xylemv1.Signature) Signature {
	return Signature{NameEmail: s.GetNameEmail(), Time: s.GetTime().AsTime().UTC(), TZOffset: s.GetTzOffsetMinutes()}
}

// HexID renders an id from a message as hex.
func HexID(b []byte) string {
	var id hash.ID
	copy(id[:], b)
	return id.String()
}

// QuotePath returns p for display, quoted if it is not valid UTF-8.
func QuotePath(p []byte) string {
	if !utf8.Valid(p) {
		return strconv.Quote(string(p))
	}
	return string(p)
}

// NewChangesetInfo converts a changeset message.
func NewChangesetInfo(c *xylemv1.Changeset) ChangesetInfo {
	parents := make([]string, len(c.GetParents()))
	for i, p := range c.GetParents() {
		parents[i] = HexID(p)
	}
	return ChangesetInfo{
		ID: HexID(c.GetId()), Seq: c.GetSeq(), Parents: parents,
		Author: signatureOf(c.GetAuthor()), Committer: signatureOf(c.GetCommitter()),
		Message: c.GetMessage(), Generation: c.GetGeneration(), OnMainline: c.GetOnMainline(),
		GitSHA: c.GetGitSha(), ChangeCount: c.GetChangeCount(),
	}
}

// Subject is the first line of the message.
func (c ChangesetInfo) Subject() string {
	s, _, _ := bytes.Cut(c.Message, []byte("\n"))
	return string(s)
}

// WriteChangeset prints metadata in a git-log-like layout.
func WriteChangeset(out io.Writer, c ChangesetInfo) error {
	var b strings.Builder
	fmt.Fprintf(&b, "changeset %s\n", c.ID)
	if c.GitSHA != "" {
		fmt.Fprintf(&b, "Git:    %s\n", c.GitSHA)
	}
	if len(c.Parents) > 1 {
		short := make([]string, len(c.Parents))
		for i, p := range c.Parents {
			short[i] = p[:12]
		}
		fmt.Fprintf(&b, "Merge:  %s\n", strings.Join(short, " "))
	}
	fmt.Fprintf(&b, "Author: %s\nDate:   %s\n\n", c.Author.NameEmail, c.Author.LocalTime().Format("Mon Jan 2 15:04:05 2006 -0700"))
	for _, line := range strings.Split(strings.TrimRight(string(c.Message), "\n"), "\n") {
		fmt.Fprintf(&b, "    %s\n", line)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

var protoGitModes = map[xylemv1.FileMode]string{
	xylemv1.FileMode_FILE_MODE_FILE: "100644", xylemv1.FileMode_FILE_MODE_EXEC: "100755",
	xylemv1.FileMode_FILE_MODE_SYMLINK: "120000", xylemv1.FileMode_FILE_MODE_TREE: "040000",
}

// GitMode returns a mode's git octal string.
func GitMode(m xylemv1.FileMode) string { return protoGitModes[m] }

// WriteServerDiff prints a server Diff response in git's format.
func WriteServerDiff(out io.Writer, files []*xylemv1.FileDiff) error {
	for _, f := range files {
		d := fileDiff{
			path: f.GetPath(), oldMode: protoGitModes[f.GetOldMode()], newMode: protoGitModes[f.GetNewMode()],
			same: bytes.Equal(f.GetOldBlob(), f.GetNewBlob()), binary: f.GetBinary(),
		}
		for _, h := range f.GetHunks() {
			d.hunks = append(d.hunks, diff.Hunk{
				OldStart: int(h.GetOldStart()), OldLines: int(h.GetOldLines()),
				NewStart: int(h.GetNewStart()), NewLines: int(h.GetNewLines()), Body: h.GetBody(),
			})
		}
		if err := d.write(out); err != nil {
			return err
		}
	}
	return nil
}
