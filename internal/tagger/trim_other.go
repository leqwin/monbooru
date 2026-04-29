//go:build tagger && !linux

package tagger

// mallocTrim is glibc-specific. Non-Linux tagger builds get a no-op so
// the cache teardown still compiles; on those platforms the
// kernel-side equivalent is whatever the C runtime decides to do.
func mallocTrim() {}
