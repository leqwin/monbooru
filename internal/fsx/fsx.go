// Package fsx holds the filesystem primitives more than one package needs
// and none of them owns: it sits below every domain package so a caller as
// low as internal/config can reach it without inverting the import order.
package fsx

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic runs write against a temp file beside path and renames it
// into place, so a concurrent reader never sees a partial file. The temp
// is removed on every failure path.
//
// The rename is atomic for the directory entry, not for the bytes behind
// it. A caller that needs the content durable across a crash - not just
// consistent - calls Sync on the file at the end of write and SyncDir on
// the containing directory afterwards. Most callers here write derived
// files that regenerate, and pay neither.
func WriteAtomic(path, pattern string, write func(*os.File) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// SyncDir flushes a directory entry so a rename survives a crash. Best
// effort: a filesystem that refuses the open or the fsync still has the
// renamed file, it just has not promised it yet.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
