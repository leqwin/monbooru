// Package compatibility translates foreign gallery exports 
// (zipped Hydrus exports, …) into the same in-memory shape the
// monbooru-native light import consumes. Each supported application lives
// in its own file and self-registers via init():
//
//	package compatibility
//
//	func init() {
//	    Register(Provider{
//	        Name:      "myapp",
//	        Detect:    detectMyApp,
//	        Translate: translateMyApp,
//	    })
//	}
//
// Adding a new application is one new file (a Detect predicate plus a
// Translate function) — nothing under internal/web's gallery_io.go or
// gallery_merge.go has to change.
package compatibility

import (
	"archive/zip"
	"fmt"
	"path"
	"strings"
)

// Provider is one application's compat layer. Detect is called against
// every uploaded zip until one returns true; the matching provider's
// Translate is then run to produce the manifest + extraction map.
type Provider struct {
	Name      string
	Detect    func(entries []NormalizedEntry) bool
	Translate func(entries []NormalizedEntry) (Result, error)
}

// Result is what a translator emits: a manifest listing one record per
// image (sha + relative path + tag tokens) plus the zip files that should
// be extracted at each manifest path. The shape mirrors the monbooru
// native light export so the apply path stays a no-op pass-through.
type Result struct {
	Manifest Manifest
	Files    map[string]*zip.File // key: path under the gallery root
}

// Manifest is the per-image record list. Tags are emitted as plain
// strings; the bare form lands in `general`, the `category:name` form
// lands under that category at apply time (with `general` as the fallback
// for unknown categories).
type Manifest struct {
	Images []ManifestImage
}

// ManifestImage carries one image's identity + tags. SHA256 is optional:
// translators pass it through only when they trust the value (currently
// only when the source string is a 64-hex-char lowercase string). On the
// replace path Ingest computes the real sha from the file bytes; on the
// merge path an empty sha treats the entry as new and routes to
// Files[Path] for ingestion.
type ManifestImage struct {
	SHA256 string
	Path   string
	Tags   []string
}

// NormalizedEntry pairs a zip entry with its archive-rel path after a
// shared top-level directory prefix has been stripped. Some applications
// wrap their export in their name (`myapp_export/...`); some don't.
// Detectors and translators see the same flat layout either way.
type NormalizedEntry struct {
	Rel  string
	File *zip.File
}

var providers []Provider

// Register adds p to the dispatch table. Called from each
// per-application file's init().
func Register(p Provider) {
	providers = append(providers, p)
}

// Providers returns a snapshot of the registered providers in
// registration order. Mostly useful for tests.
func Providers() []Provider {
	out := make([]Provider, len(providers))
	copy(out, providers)
	return out
}

// Detect returns the name of the matching provider, or "" when no
// application's signal is present. Caller routes through Translate(...,
// returnedName) on a hit.
func Detect(files []*zip.File) string {
	entries := NormalizeEntries(files)
	for _, p := range providers {
		if p.Detect(entries) {
			return p.Name
		}
	}
	return ""
}

// Translate looks up the named provider and runs its translator. Returns
// an error when the format is unknown or the translator itself fails.
func Translate(files []*zip.File, format string) (Result, error) {
	entries := NormalizeEntries(files)
	for _, p := range providers {
		if p.Name == format {
			return p.Translate(entries)
		}
	}
	return Result{}, fmt.Errorf("unknown compat format %q", format)
}

// NormalizeEntries strips a shared top-level directory prefix when every
// entry shares one, so a Hydrus zip whose contents live under
// `hydrus_export/<sha>.png` is handled the same way as a flat zip.
// Directory entries (names ending in `/`) and the now-empty root entry
// are dropped from the result.
func NormalizeEntries(files []*zip.File) []NormalizedEntry {
	var prefix string
	havePrefix := false
	for _, f := range files {
		name := f.Name
		if name == "" {
			continue
		}
		idx := strings.Index(name, "/")
		if idx < 0 {
			havePrefix = false
			break
		}
		head := name[:idx+1]
		if !havePrefix {
			prefix = head
			havePrefix = true
			continue
		}
		if head != prefix {
			havePrefix = false
			break
		}
	}
	out := make([]NormalizedEntry, 0, len(files))
	for _, f := range files {
		rel := f.Name
		if havePrefix {
			rel = strings.TrimPrefix(rel, prefix)
		}
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		out = append(out, NormalizedEntry{Rel: rel, File: f})
	}
	return out
}

// HasMediaExt reports whether name ends in one of the supported gallery
// media extensions (case-insensitive). Shared so every translator
// classifies image entries the same way the gallery does.
func HasMediaExt(name string) bool {
	if strings.HasSuffix(name, "/") {
		return false
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".mp4", ".webm":
		return true
	}
	return false
}

// PickValidSHA256 returns s when it is a 64-character lowercase hex
// string, otherwise "". Translators use this to gate which source-side
// hash field is trustworthy enough to round-trip into the manifest. An
// empty result tells the apply path to compute the real sha from the
// file's bytes at Ingest time.
func PickValidSHA256(s string) string {
	if len(s) != 64 {
		return ""
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return s
}
