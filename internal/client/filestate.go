package client

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/delefrati/xylem/internal/hash"
	"github.com/delefrati/xylem/internal/object"
)

// fileCheck is what is on disk at a path, relative to the dirstate.
type fileCheck struct {
	exists bool
	mode   object.Mode
	blob   hash.ID // set when hashed or trusted from the dirstate
	info   fs.FileInfo
	clean  bool // matches the dirstate entry (mode and content)
	stat   bool // decided from stat alone (no hashing)
}

// checkFile compares the working file at path with its dirstate entry (06
// §Status): same size, mtime and mode → clean without reading it, unless the
// mtime is at or after the dirstate's write time (racy); otherwise hash.
func checkFile(root string, ds *Dirstate, path []byte) (fileCheck, error) {
	abs := filepath.Join(root, string(path))
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return fileCheck{}, nil // ENOTDIR: a parent is a file or symlink, so the path cannot exist
	}
	if err != nil {
		return fileCheck{}, err
	}
	mode, ok := FileMode(info)
	if !ok {
		return fileCheck{exists: true, info: info}, nil // a directory or special file
	}
	fc := fileCheck{exists: true, mode: mode, info: info}
	e, tracked := ds.Get(path)
	if tracked && e.MtimeNs != 0 && e.Mode == mode && e.Size == info.Size() &&
		e.MtimeNs == info.ModTime().UnixNano() && info.ModTime().UnixNano() < ds.WrittenAtNs {
		fc.blob, fc.clean, fc.stat = e.Blob, true, true
		return fc, nil
	}
	id, _, err := HashFile(abs, mode)
	if err != nil {
		return fileCheck{}, err
	}
	fc.blob = id
	fc.clean = tracked && e.Mode == mode && e.Blob == id
	return fc, nil
}
