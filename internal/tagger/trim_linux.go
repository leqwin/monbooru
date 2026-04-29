//go:build tagger && linux

package tagger

// #include <malloc.h>
import "C"

// mallocTrim is the C-heap counterpart to debug.FreeOSMemory: it asks
// glibc to munmap the main arena's free tail back to the kernel so
// bytes ORT freed on Destroy actually leave RSS. Safe on musl (Alpine):
// the symbol resolves to a cheap stub that returns 0.
func mallocTrim() {
	C.malloc_trim(0)
}
