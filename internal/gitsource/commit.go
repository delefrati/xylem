package gitsource

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
)

// Ident is an author or committer line: "Name <email> <unix time> <±hhmm>".
type Ident struct {
	NameEmail []byte // "Name <email>", raw bytes
	Time      int64
	TZMinutes int32
}

// Commit is a parsed git commit object. Headers other than tree, parent,
// author, committer and encoding (e.g. gpgsig, mergetag) are not kept.
type Commit struct {
	SHA       SHA
	Tree      SHA
	Parents   []SHA
	Author    Ident
	Committer Ident
	Encoding  string // "encoding" header, empty if absent
	Message   []byte // raw bytes, not re-encoded
}

// ReadCommit reads and parses commit sha.
func (r *Repo) ReadCommit(ctx context.Context, sha SHA) (*Commit, error) {
	typ, data, err := r.Object(ctx, sha)
	if err != nil {
		return nil, err
	}
	if typ != "commit" {
		return nil, fmt.Errorf("object %s is a %s, not a commit", sha, typ)
	}
	c, err := ParseCommit(data)
	if err != nil {
		return nil, fmt.Errorf("commit %s: %w", sha, err)
	}
	c.SHA = sha
	return c, nil
}

// ParseCommit parses the raw content of a commit object.
func ParseCommit(data []byte) (*Commit, error) {
	headers, message, ok := bytes.Cut(data, []byte("\n\n"))
	if !ok {
		// A commit with no message still ends its headers with a newline.
		headers, message = bytes.TrimSuffix(data, []byte("\n")), nil
	}
	c := &Commit{Message: message}
	var haveTree, haveAuthor, haveCommitter bool
	for _, line := range bytes.Split(headers, []byte("\n")) {
		if len(line) > 0 && line[0] == ' ' {
			continue // continuation of a multi-line header (gpgsig, mergetag)
		}
		key, value, _ := bytes.Cut(line, []byte(" "))
		var err error
		switch string(key) {
		case "tree":
			c.Tree, err = ParseSHA(string(value))
			haveTree = true
		case "parent":
			var p SHA
			if p, err = ParseSHA(string(value)); err == nil {
				c.Parents = append(c.Parents, p)
			}
		case "author":
			c.Author, err = parseIdent(value)
			haveAuthor = true
		case "committer":
			c.Committer, err = parseIdent(value)
			haveCommitter = true
		case "encoding":
			c.Encoding = string(value)
		}
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", key, err)
		}
	}
	if !haveTree || !haveAuthor || !haveCommitter {
		return nil, fmt.Errorf("missing tree, author or committer header")
	}
	return c, nil
}

// parseIdent parses "Name <email> 1700000000 +0530".
func parseIdent(b []byte) (Ident, error) {
	end := bytes.LastIndexByte(b, '>')
	if end < 0 {
		return Ident{}, fmt.Errorf("ident %q has no '>'", b)
	}
	id := Ident{NameEmail: bytes.Clone(b[:end+1])}
	fields := bytes.Fields(b[end+1:])
	if len(fields) != 2 {
		return Ident{}, fmt.Errorf("ident %q: want time and time zone after the email", b)
	}
	t, err := strconv.ParseInt(string(fields[0]), 10, 64)
	if err != nil {
		return Ident{}, fmt.Errorf("ident %q: time: %w", b, err)
	}
	id.Time = t
	tz := fields[1]
	if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return Ident{}, fmt.Errorf("ident %q: time zone %q", b, tz)
	}
	hh, err1 := strconv.Atoi(string(tz[1:3]))
	mm, err2 := strconv.Atoi(string(tz[3:5]))
	if err1 != nil || err2 != nil {
		return Ident{}, fmt.Errorf("ident %q: time zone %q", b, tz)
	}
	minutes := int32(hh*60 + mm) //nolint:gosec // G115: at most 99*60+99
	if tz[0] == '-' {
		minutes = -minutes
	}
	id.TZMinutes = minutes
	return id, nil
}
