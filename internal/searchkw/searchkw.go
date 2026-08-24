// Package searchkw declares the search query language's filter-keyword
// vocabulary as a leaf data package. The parser and executor in
// internal/search read it to dispatch `key:value` filters; the gallery
// search bar's `system:` cheat-sheet iterates Keywords for its dropdown
// rows; the tag service in internal/tags borrows the same list as the
// reserved category-name set. Putting the source of truth here breaks
// the cycle that would otherwise form via internal/gallery's tag-service
// usage and internal/search's gallery-using test fixtures.
package searchkw

import "strings"

// Keywords lists every search-filter keyword in the order the
// `system:` cheat-sheet dropdown surfaces them. Adding a future filter
// is one edit here.
var Keywords = []string{
	"fav",
	"inbox",
	"ai",
	"source",
	"cat",
	"width",
	"height",
	"date",
	"missing",
	"tagged",
	"autotagged",
	"stale",
	"folder",
	"folderonly",
	"generated",
	"rating",
	"type",
	"collection",
	"pages",
	"name",
	"size",
	"mime",
	"ratio",
	"tagcount",
	"duration",
	"hash",
	"md5",
	"prompt",
	"model",
	"sampler",
	"seed",
	"via",
	"phash",
	"relation",
	"similar",
	"id",
	"lookup",
	"upgrade",
}

// keywordSet is the membership-test view of Keywords. Built once at
// init so IsKeyword is a single map lookup. `system` joins it here
// rather than in Keywords: it is the cheat-sheet's own namespace, not a
// filter the dropdown lists, but a query carrying it must not be read
// as a category-qualified tag and probed for a category first.
var keywordSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Keywords)+1)
	m["system"] = struct{}{}
	for _, k := range Keywords {
		m[k] = struct{}{}
	}
	return m
}()

// IsKeyword reports whether s is one of the recognised search-filter
// keywords. Returns false for tag-name colons like "nier:automata".
func IsKeyword(s string) bool {
	_, ok := keywordSet[s]
	return ok
}

// Expansions lists the second-level autocomplete rows for the
// `system:<key>:` cheat-sheet drill-in: comparison operators for ordinal
// filters and closed-vocabulary values otherwise. `cat:` is data-driven
// from `tag_categories` and intentionally absent here; `folder:`,
// `folderonly:`, and `generated:` accept open input and have no static
// expansion.
var Expansions = map[string][]string{
	"fav":        {"true", "false"},
	"inbox":      {"true", "false"},
	"ai":         {"a1111", "comfyui", "none", "any", "sd"},
	"source":     {"none", "any"},
	"width":      {">=", "<=", ">", "<", "=", ".."},
	"height":     {">=", "<=", ">", "<", "=", ".."},
	"date":       {">", "<", ">=", "<=", "=", ".."},
	"missing":    {"true", "false"},
	"tagged":     {"true", "false"},
	"autotagged": {"true", "false"},
	"stale":      {"any", "none"},
	"rating":     {"general", "sensitive", "questionable", "explicit"},
	"type":       {"image", "archive", "animated"},
	"pages":      {">=", "<=", ">", "<", "=", ".."},
	"size":       {">=", "<=", ">", "<", "=", ".."},
	"ratio":      {">=", "<=", ">", "<", "=", ".."},
	"tagcount":   {">=", "<=", ">", "<", "=", ".."},
	"duration":   {">=", "<=", ">", "<", "=", ".."},
	"mime":       {"jpeg", "png", "webp", "gif", "mp4", "webm", "cbz"},
	"via":        {"ingest", "upload"},
	"relation":   {"duplicate", "original", "alternate", "version", "derivative", "source", "collection", "any", "none"},
	"lookup":     {"never", "due", "missed", "exhausted", "off"},
	"upgrade":    {"any", "bigger", "none", "unknown", "sample", "kept"},
}

// rangeKeys are excluded from closed-vocabulary validation: their Expansions
// rows are hints (comparison operators for the numeric filters, the any/none
// shortcuts for stale and source) rather than the full set of accepted
// values. stale: also takes an open tag name, source: and upgrade: an open
// site label, so their values are never flagged as unrecognised.
var rangeKeys = map[string]bool{
	"width": true, "height": true, "date": true, "size": true,
	"ratio": true, "tagcount": true, "duration": true, "pages": true,
	"stale": true, "source": true, "upgrade": true,
}

// closedVocab is the membership-test view of Expansions for the keys
// whose values are a fixed set (type, mime, the bool filters, ...).
// Range keys and open-input keys (folder, source, name, ...) are absent,
// so ValueKnown treats them as accepting anything.
var closedVocab = func() map[string]map[string]struct{} {
	m := make(map[string]map[string]struct{}, len(Expansions))
	for key, vals := range Expansions {
		if rangeKeys[key] {
			continue
		}
		set := make(map[string]struct{}, len(vals))
		for _, v := range vals {
			set[v] = struct{}{}
		}
		m[key] = set
	}
	return m
}()

// ValueKnown reports whether val is a recognised value for a
// closed-vocabulary key. Open-input and range keys always return true.
// Comma-separated unions (type:, mime:) hold only when every element is
// recognised, matching how the executor unions the buckets.
func ValueKnown(key, val string) bool {
	set, ok := closedVocab[key]
	if !ok {
		return true
	}
	for _, part := range strings.Split(strings.ToLower(val), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := set[part]; !ok {
			return false
		}
	}
	return true
}

// Descriptions maps each filter keyword to a short English label the
// cheat-sheet dropdown shows just left of the "system" column. Tag
// categories surface in the level-1 list with a generic "category"
// label applied at render time.
var Descriptions = map[string]string{
	"fav":        "favorite images",
	"inbox":      "in inbox",
	"ai":         "AI generation tool",
	"source":     "external source label",
	"cat":        "by tag category",
	"width":      "image width",
	"height":     "image height",
	"date":       "ingestion date",
	"missing":    "files gone from disk",
	"tagged":     "has any tag",
	"autotagged": "has auto-tag",
	"stale":      "tags a source dropped",
	"folder":     "folder (recursive)",
	"folderonly": "folder (exact)",
	"generated":  "generation recipe",
	"rating":     "safety rating",
	"type":       "image / archive / animated",
	"collection": "collection label",
	"pages":      "page count",
	"name":       "file name",
	"size":       "file size",
	"mime":       "file type",
	"ratio":      "aspect ratio (width / height)",
	"tagcount":   "number of tags",
	"duration":   "video duration in seconds",
	"hash":       "sha256 or md5 digest",
	"md5":        "md5 digest",
	"prompt":     "SD / ComfyUI prompt",
	"model":      "SD / ComfyUI model",
	"sampler":    "SD / ComfyUI sampler",
	"seed":       "SD / ComfyUI seed",
	"via":        "added via",
	"phash":      "perceptual hash (16-hex; ~d for Hamming distance)",
	"relation":   "declared relation",
	"similar":    "tag similarity to image id (~score 0..1 for a threshold)",
	"id":         "image id",
	"lookup":     "scheduled lookup state",
	"upgrade":    "source serves a different file",
}

// numericComparisons is the operator vocabulary every plain numeric range
// filter shares. size:, ratio: and duration: word theirs after the unit
// they carry, which is what makes these four a real duplicate rather than
// a coincidence. Read-only: ExpansionDescriptions is only ever read (by
// the system: cheat sheet), so the four keys sharing one map is safe.
var numericComparisons = map[string]string{
	">=": "at least",
	"<=": "at most",
	">":  "more than",
	"<":  "less than",
	"=":  "exactly",
	"..": "range",
}

// ExpansionDescriptions maps level-2 rows to a short English label.
// Boolean expansions (true / false) and rating values are intentionally
// absent - they're self-explanatory in their bare form. Operators and
// `source:` values benefit from disambiguation.
var ExpansionDescriptions = map[string]map[string]string{
	"date": {
		">":  "after",
		"<":  "before",
		">=": "on or after",
		"<=": "on or before",
		"=":  "exactly",
		"..": "range",
	},
	"width":  numericComparisons,
	"height": numericComparisons,
	"ai": {
		"a1111":   "A1111 / Forge",
		"comfyui": "ComfyUI",
		"none":    "no metadata",
		"any":     "any AI tool",
		"sd":      "alias of a1111",
	},
	"pages": numericComparisons,
	"type": {
		"image":    "regular images (jpeg / png / webp)",
		"archive":  "cbz / zip archives",
		"animated": "gif / mp4 / webm",
	},
	"size": {
		">=": "at least (bytes; suffix KB/MB/GB)",
		"<=": "at most",
		">":  "more than",
		"<":  "less than",
		"=":  "exactly",
		"..": "range",
	},
	"ratio": {
		">=": "wider than (e.g. 1.5 = 3:2)",
		"<=": "taller than",
		">":  "wider than",
		"<":  "taller than",
		"=":  "exact ratio",
		"..": "range",
	},
	"tagcount": numericComparisons,
	"duration": {
		">=": "at least N seconds",
		"<=": "at most N seconds",
		">":  "longer than",
		"<":  "shorter than",
		"=":  "exactly N seconds",
		"..": "range",
	},
	"via": {
		"ingest": "watcher or sync",
		"upload": "web upload form",
	},
	"source": {
		"none": "no source at all",
		"any":  "any source",
	},
	"lookup": {
		"never":     "not tried yet",
		"due":       "queued for next scheduled lookup",
		"missed":    "waiting out a backoff",
		"exhausted": "nothing found",
		"off":       "never look this up",
	},
}
