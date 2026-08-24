package tagger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/monbooru/monbooru/internal/fsx"
)

// EmbeddedDispatchRules returns the shipped default rules for one
// tagger, in file order. Empty for taggers without an embedded table.
func EmbeddedDispatchRules(taggerName string) []DispatchEntry {
	return parseEmbeddedDispatch(taggerName)
}

// OverlayDispatchRules returns the rules of the tagger's on-disk
// overlay, in file order. Empty when no overlay exists.
func OverlayDispatchRules(modelPath, taggerName string) []DispatchEntry {
	return parseOverlayDispatch(modelPath, taggerName)
}

// MergedDispatchRules returns the embedded defaults with the overlay
// applied - same-source entries replaced, new sources appended -
// source-sorted. This is the effective table the export view shows and
// the file shape a dispatch_default PR replaces.
func MergedDispatchRules(modelPath, taggerName string) []DispatchEntry {
	merged := map[string]DispatchEntry{}
	for _, e := range parseEmbeddedDispatch(taggerName) {
		merged[e.Source] = e
	}
	for _, e := range parseOverlayDispatch(modelPath, taggerName) {
		merged[e.Source] = e
	}
	out := make([]DispatchEntry, 0, len(merged))
	for _, e := range merged {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// MarshalDispatchDoc renders rules as a complete dispatch.json
// document, the same shape both the embedded defaults and the overlay
// use.
func MarshalDispatchDoc(rules []DispatchEntry) ([]byte, error) {
	return json.MarshalIndent(dispatchDoc{Version: dispatchSchemaVersion, Rules: rules}, "", "  ")
}

// overlayMu serialises the read-modify-write cycle in
// UpdateDispatchOverlay. SaveDispatchOverlay is atomic per write, but
// two edits that both start from the same on-disk snapshot still lose
// one of them without this.
var overlayMu sync.Mutex

// UpdateDispatchOverlay folds one edit into the tagger's overlay and
// rewrites it, holding the lock across the whole cycle. mutate sees the
// current rules keyed by source; a non-nil error aborts before any
// write.
func UpdateDispatchOverlay(modelPath, taggerName string, mutate func(map[string]DispatchEntry) error) error {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	overlay := map[string]DispatchEntry{}
	for _, e := range parseOverlayDispatch(modelPath, taggerName) {
		overlay[e.Source] = e
	}
	if err := mutate(overlay); err != nil {
		return err
	}
	rules := make([]DispatchEntry, 0, len(overlay))
	for _, e := range overlay {
		rules = append(rules, e)
	}
	return SaveDispatchOverlay(modelPath, taggerName, rules)
}

// SaveDispatchOverlay rewrites <modelPath>/<taggerName>/dispatch.json
// with the given rules, source-sorted. A rule identical to the
// embedded default for its source is dropped so the overlay stays a
// pure delta against stock, and an overlay left empty is deleted
// instead of written - "differs from stock" needs no bookkeeping
// beyond the file's existence. The write is atomic (temp + rename) so
// a crash can't leave a half-written table for the next Run to skip.
func SaveDispatchOverlay(modelPath, taggerName string, rules []DispatchEntry) error {
	embedded := map[string]DispatchEntry{}
	for _, e := range parseEmbeddedDispatch(taggerName) {
		embedded[e.Source] = e
	}
	kept := make([]DispatchEntry, 0, len(rules))
	for _, r := range rules {
		if def, ok := embedded[r.Source]; ok && def == r {
			continue
		}
		kept = append(kept, r)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Source < kept[j].Source })

	path := filepath.Join(modelPath, taggerName, "dispatch.json")
	if len(kept) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(dispatchDoc{Version: dispatchSchemaVersion, Rules: kept}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsx.WriteAtomic(path, ".dispatch.json.*", func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}
