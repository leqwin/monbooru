package fsx

import (
	"os"
	"path/filepath"
)

// ExeDir is the directory holding this executable, symlinks resolved so a
// link in ~/.local/bin points at the unpacked folder rather than at itself.
// Empty when the path cannot be determined.
//
// It is what makes a bundle work: a tool shipped beside the binary is not on
// PATH and nothing sets anything up before the process starts.
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}
