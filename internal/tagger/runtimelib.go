package tagger

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/monbooru/monbooru/internal/fsx"
)

// sharedLibPath finds the ONNX Runtime shared library. ORT_LIB_PATH
// overrides; otherwise the executable's own directory comes first, so a
// library shipped in a bundle wins over a distribution's, and the system
// paths answer for an install that got it from a package manager.
//
// It carries no ORT dependency of its own, which is why discovery can ask
// it whether inference could run at all before a job finds out the hard way.
func sharedLibPath() string {
	if p := os.Getenv("ORT_LIB_PATH"); p != "" {
		return p
	}
	name := "libonnxruntime.so"
	var candidates []string
	if runtime.GOOS == "windows" {
		name = "onnxruntime.dll"
	}
	if dir := fsx.ExeDir(); dir != "" {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	if runtime.GOOS == "windows" {
		// The working directory is a Windows loader convention; keeping it
		// costs one Stat and covers a run from an unpacked folder.
		if wd, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(wd, name))
		}
	} else {
		candidates = append(candidates, "/usr/lib/"+name, "/usr/local/lib/"+name)
	}
	candidates = append(candidates, name)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return name
}

// missingRuntimeLibrary names the ONNX Runtime library when it is not where
// the loader will look, and returns "" when it is. Model and label files are
// not the only thing a tagger needs, and a row that reads as available until
// a job fails at inference tells the operator nothing.
func missingRuntimeLibrary() string {
	path := sharedLibPath()
	if _, err := os.Stat(path); err == nil {
		return ""
	}
	reason := "missing " + filepath.Base(path)
	// A bare name means nothing was found anywhere; point at the folder a
	// bundled copy belongs in rather than at a path that does not exist.
	if path == filepath.Base(path) {
		if dir := fsx.ExeDir(); dir != "" {
			return reason + " (expected in " + dir + ")"
		}
		return reason
	}
	return reason + " at " + path
}
