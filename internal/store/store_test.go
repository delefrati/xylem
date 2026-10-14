package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

func TestKey(t *testing.T) {
	t.Parallel()
	id, err := hash.ParseHex("abcdef" + strings.Repeat("0", 58))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Key(id), "objects/ab/cd/"+id.String(); got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()
	if s, err := Open("mem://"); err != nil {
		t.Errorf("Open(mem://) = %v", err)
	} else if _, ok := s.(*Mem); !ok {
		t.Errorf("Open(mem://) = %T, want *Mem", s)
	}
	for url, root := range map[string]string{
		"fs://./data/objects": "./data/objects",
		"fs:///var/lib/xylem": "/var/lib/xylem",
		"fs://rel":            "rel",
	} {
		s, err := Open(url)
		if err != nil {
			t.Errorf("Open(%s) = %v", url, err)
			continue
		}
		if fsStore, ok := s.(*FS); !ok || fsStore.root != root {
			t.Errorf("Open(%s) = %#v, want *FS rooted at %q", url, s, root)
		}
	}
	s, err := Open("s3://xylem-objects?endpoint=localhost:59000&access_key=xylem&secret_key=xylemxylem&secure=false")
	if err != nil {
		t.Fatalf("Open(s3://…) = %v", err)
	}
	if s3, ok := s.(*S3); !ok || s3.bucket != "xylem-objects" || s3.hasConcurrency != 32 {
		t.Errorf("Open(s3://…) = %#v", s)
	}
}

func TestParseS3URL(t *testing.T) {
	t.Parallel()
	cfg, err := parseS3URL("s3://b?endpoint=h:1&access_key=a&secret_key=s&secure=false&region=r")
	if err != nil {
		t.Fatal(err)
	}
	want := S3Config{Bucket: "b", Endpoint: "h:1", AccessKey: "a", SecretKey: "s", Secure: false, Region: "r"}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Errorf("parseS3URL mismatch (-want +got):\n%s", diff)
	}
	cfg, err = parseS3URL("s3://b")
	if err != nil || !cfg.Secure || cfg.Endpoint != "" {
		t.Errorf("parseS3URL(s3://b) = %+v, %v; want secure default", cfg, err)
	}
}

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	for _, url := range []string{
		"",
		"data/objects",
		"mem://x",
		"fs://",
		"gs://bucket",
		"s3://",
		"s3://bucket/path",
		"s3://bucket?secure=maybe&secret_key=hunter2",
		"s3://bucket?endpont=typo&secret_key=hunter2",
	} {
		_, err := Open(url)
		if err == nil {
			t.Errorf("Open(%q) succeeded", url)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Open(%q) error leaks the secret key: %v", url, err)
		}
	}
}

func TestFSLayoutAndNoTempFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	s := NewFS(root)
	id, stored := object.Encode(object.KindBlob, []byte("hello"))
	if err := s.Put(ctx, id, stored); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, id, stored); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	h := id.String()
	dir := filepath.Join(root, "objects", h[:2], h[2:4])
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != h {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("files in %s = %q, want just %s", dir, names, h)
	}
	got, err := os.ReadFile(filepath.Join(dir, h))
	if err != nil || string(got) != string(stored) {
		t.Errorf("file content differs: %v", err)
	}
}

func TestMemCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := NewMem()
	id := hash.ID{1}
	in := []byte("abc")
	if err := m.Put(ctx, id, in); err != nil {
		t.Fatal(err)
	}
	in[0] = 'z'
	got, err := m.Get(ctx, id)
	if err != nil || string(got) != "abc" {
		t.Fatalf("Get = %q, %v; Put did not copy", got, err)
	}
	got[0] = 'y'
	if again, _ := m.Get(ctx, id); string(again) != "abc" {
		t.Errorf("Get did not copy: %q", again)
	}
	if _, err := m.Get(ctx, hash.ID{2}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}
