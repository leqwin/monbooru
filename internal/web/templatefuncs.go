package web

import (
	"cmp"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
	"github.com/monbooru/monbooru/internal/upgrade"
)

// originTagGroup / originImplicationGroup bundle a relation list by the
// provenance label that declared each row, so the tag-detail relation
// sections can head each subgroup with its source.
type originTagGroup struct {
	Origin string
	Stale  bool // the PTR's latest refresh no longer listed these rows
	Tags   []models.Tag
}

type originImplicationGroup struct {
	Origin       string
	Stale        bool // the PTR's latest refresh no longer carried these edges
	Implications []models.Implication
}

// templateFuncs is the FuncMap every template render sees. Lives apart
// from NewServer so router.go stays routes + middleware + server
// lifecycle.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"seq": func(start, end int) []int {
			r := make([]int, 0, end-start+1)
			for i := start; i <= end; i++ {
				r = append(r, i)
			}
			return r
		},
		"add": func(a, b int) int { return a + b },
		// catColor renders a tag-category colour as a value a theme can
		// restate. The variable is keyed on the colour itself rather than
		// on the category, so a theme maps the palette monbooru ships
		// (the shipped hues are tuned for the dark ground and read washed
		// out on a light one) while a colour the operator picked at
		// /categories - which no theme names - falls through to itself.
		// Typed template.CSS to survive the style-attribute sanitizer,
		// which is why the input is validated first: only the #rgb /
		// #rrggbb shape the Categories form enforces gets through.
		"catColor": func(color string) template.CSS { return template.CSS(categoryColor(color)) },
		// catDefault is the colour the Categories page's Reset writes back,
		// empty for a category the operator created.
		"catDefault": tags.DefaultCategoryColor,
		// urlQ percent-encodes a query value with uppercase hex pairs so
		// the links the sidebar emits match the case the browser writes
		// back into the address bar (browsers normalize to uppercase per
		// RFC 3986). Without this the user's autocomplete history grows
		// two entries per logical query (one with lowercase hex, one
		// uppercase). url.QueryEscape emits lowercase; we re-case the
		// %XX sequences without touching the surrounding letters.
		//
		// Returns template.URL so html/template's href-context URL
		// autoescaper leaves the value alone. As a plain string it would
		// re-percent-encode every `%`, double-encoding the link and
		// turning `folder:"path"` into a literal query with no matches.
		"urlQ": func(s string) template.URL {
			return template.URL(uppercasePercentEscapes(url.QueryEscape(s)))
		},
		// qval backslash-escapes a label so it survives interpolation
		// into a quoted `key:"<value>"` search term (collection / source
		// links). The parser's unescapeQuoted reverses it, so a label
		// containing a double-quote round-trips instead of truncating the
		// query at the inner quote.
		"qval": search.QuoteValue,
		"sub":  func(a, b int) int { return a - b },
		"pct":  func(f float64) int { return int(math.Round(f * 100)) },
		"list": func(vs ...any) []any { return vs },
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				k, _ := pairs[i].(string)
				m[k] = pairs[i+1]
			}
			return m
		},
		"groupByCategory": func(tagList []models.Tag) []tagGroup {
			return groupOrdered(tagList, nil,
				func(t models.Tag) string { return t.CategoryName },
				func(t models.Tag) *tagGroup { return &tagGroup{Name: t.CategoryName, Color: t.CategoryColor} },
				func(g *tagGroup, t models.Tag) { g.Tags = append(g.Tags, t) })
		},
		"deref":    derefOr[int],
		"deref64":  derefOr[int64],
		"deref64f": derefOr[float64],
		// elideHash keeps both ends of a content hash, which is what anyone
		// comparing two of them reads; the full value stays one click away
		// on the copy button.
		"elideHash": func(h string) string {
			if len(h) <= 19 {
				return h
			}
			return h[:8] + "..." + h[len(h)-8:]
		},
		"phashHex": func(p *int64) string {
			if p == nil {
				return ""
			}
			return fmt.Sprintf("%016x", uint64(*p))
		},
		"upgradable":     upgrade.Eligible,
		"upgradeCompare": upgradeCompare,
		// The "ptr" source has no post page: its fetch action is a hash
		// lookup instead of a url refetch, so templates branch on the label.
		"isPTRSite": func(site string) bool {
			return strings.EqualFold(strings.TrimSpace(site), "ptr")
		},
		"groupTagsByOrigin": func(list []models.Tag) []originTagGroup {
			return groupByOriginStale(list,
				func(t models.Tag) (string, bool) { return t.Origin, t.Stale },
				func(t models.Tag) originTagGroup { return originTagGroup{Origin: t.Origin, Stale: t.Stale} },
				func(g *originTagGroup, t models.Tag) { g.Tags = append(g.Tags, t) })
		},
		"groupImplicationsByOrigin": func(list []models.Implication) []originImplicationGroup {
			return groupByOriginStale(list,
				func(im models.Implication) (string, bool) { return im.Origin, im.Stale },
				func(im models.Implication) originImplicationGroup {
					return originImplicationGroup{Origin: im.Origin, Stale: im.Stale}
				},
				func(g *originImplicationGroup, im models.Implication) { g.Implications = append(g.Implications, im) })
		},
		// originLabel spells a provenance label for a relation subheading,
		// mirroring the meta line's user/ptr handling.
		"originLabel": func(kinds map[string]string, origin string) string {
			switch {
			case origin == "":
				return "an unrecorded source"
			case kinds[origin] == "user":
				return "the user"
			case kinds[origin] == "ptr" || strings.EqualFold(origin, "ptr"):
				return "the Public Tag Repository"
			default:
				return origin
			}
		},
		"cancelTitle": cancelTitle,
		"humanBytes":  humanBytesFmt,
		// localTime renders a stored-UTC timestamp in the process timezone
		// (time.Local, driven by TZ) so displayed times match the operator's
		// wall clock. Storage stays UTC; only the display converts.
		"localTime": func(t time.Time) string {
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		},
		"localTimePtr": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		},
		// localDate is localTime without the clock, for lines where the day
		// is the whole answer (a lookup's next due date is weeks out).
		"localDate":          localDay,
		"monloaderOffReason": monloaderOffReason,
		// lookupResultLabel renders a recorded outcome as the tail of the
		// "Last looked up" line. An error leaves last_result untouched by
		// design, so only a concluded hit or miss reaches this.
		"lookupResultLabel": func(result string) string {
			if result == "hit" {
				return "tags applied"
			}
			return "no match"
		},
		"browseSortLabel": browseSortLabel,
		"isLongValue": func(s string) bool {
			return len(s) > 200 || strings.ContainsAny(s, "\n\r")
		},
		"schedDuration": func(d time.Duration) string {
			// Round to the nearest second for anything over 1s; keep
			// millisecond precision below so sub-second scheduler passes
			// (the typical case on an idle gallery) still render usefully.
			if d >= time.Second {
				return d.Round(time.Second).String()
			}
			return d.Round(time.Millisecond).String()
		},
		"minusDuration": func(a, b time.Duration) time.Duration {
			return a - b
		},
		"int64Duration": func(d time.Duration) int64 {
			return int64(d)
		},
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"abbrevCount": abbrevCount,
		"comfyRefTarget": func(s string) string {
			// Displayed ComfyUI references start with "→ " followed by the
			// referenced node's key. Strip the arrow+space so the template
			// can build `href="#comfy-node-<key>"` for in-page navigation.
			return strings.TrimPrefix(s, "→ ")
		},
		"hasPrefix":     strings.HasPrefix,
		"providerLabel": providerDisplayLabel,
		// urlDomain returns the host of a URL (without a leading "www.") for
		// display; the full URL still drives the link's href. Falls back to
		// the input when it doesn't parse as an absolute URL with a host.
		"urlDomain": func(s string) string {
			u, err := url.Parse(s)
			if err != nil || u.Host == "" {
				return s
			}
			return strings.TrimPrefix(u.Host, "www.")
		},
		"truncate": truncateRunes,
		// hasFavFilter reports whether the search query contains a `fav:true`
		// token, regardless of position or surrounding tags. Drives the gallery
		// header's ♥ toggle's active class so the button doesn't go inactive
		// the moment the user combines `fav:true` with any other tag.
		"hasFavFilter": func(query string) bool {
			for _, tok := range strings.Fields(query) {
				if strings.EqualFold(tok, "fav:true") {
					return true
				}
			}
			return false
		},
		"pageLoadMs": func(t time.Time) int64 {
			if t.IsZero() {
				return 0
			}
			return time.Since(t).Milliseconds()
		},
	}
}

// localDay renders a date in the server's zone, the shape every date-only
// line uses. Shared with the code paths that build such a line in Go.
func localDay(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

// cancelTitles is the tooltip on the job-status × button. Only the job
// types that observe ctx.Done() in their worker loop appear here; anything
// else falls back to the bare verb.
var cancelTitles = map[string]string{
	"autotag":        "Stop auto-tagging",
	"sync":           "Stop syncing",
	"delete":         "Stop deleting",
	"re-extract":     "Stop re-extraction",
	"rebuild-thumbs": "Stop thumbnail rebuild",
	"prune-thumbs":   "Stop thumbnail prune",
	"hashes":         "Stop hash backfill",
	"relations":      "Stop find-pairs",
	"lookup":         "Stop the lookup",
	"move":           "Stop moving",
	"tag":            "Stop tagging",
}

func cancelTitle(jobType string) string {
	return cmp.Or(cancelTitles[jobType], "Stop")
}

// runningJobNames names a job inside a sentence, where cancelTitles names
// it on a button. The values open the sentence, so they carry their own
// article and capital.
var runningJobNames = map[string]string{
	"autotag":        "Auto-tagging",
	"sync":           "A gallery sync",
	"delete":         "A delete",
	"re-extract":     "A re-extraction",
	"rebuild-thumbs": "A thumbnail rebuild",
	"prune-thumbs":   "A thumbnail prune",
	"hashes":         "A hash backfill",
	"relations":      "A find-pairs run",
	"lookup":         "A lookup",
	"move":           "A move",
	"tag":            "A tagging job",
	"transfer":       "A transfer",
	"vacuum":         "A vacuum",
	"free-memory":    "A memory reclaim",
	"fold":           "A fold",
}

func runningJobName(jobType string) string {
	return cmp.Or(runningJobNames[jobType], "A job")
}

// browseSortLabels names each /relations/browse sort for its button; an
// unmapped value renders as itself.
var browseSortLabels = map[string]string{
	"recent":         "Recent",
	"size":           "Size",
	"original_added": "Original added",
	"length":         "Length",
	"newest_member":  "Newest member",
}

func browseSortLabel(s string) string { return cmp.Or(browseSortLabels[s], s) }

// monloaderOffReasons titles a monloader-backed control that renders but
// cannot act, so a paused link reads as paused rather than as breakage.
var monloaderOffReasons = map[string]string{
	"paused":   "monloader is paused",
	"rejected": "monloader rejected the token",
}

func monloaderOffReason(conn string) string {
	return cmp.Or(monloaderOffReasons[conn], "monloader is not responding")
}

// abbrevCount shortens a usage count to at most four glyphs so the sidebar's
// count column keeps a fixed gutter at any library size; the exact figure
// rides the cell's title. The decimal is kept above 1000 rather than
// trimmed, so the column reads as one width, and the unit promotes at
// 999500 because rounding any higher would spill a fifth glyph.
func abbrevCount(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 999500:
		return abbrevUnit(n, 1000, "k")
	default:
		return abbrevUnit(n, 1000000, "M")
	}
}

func abbrevUnit(n, div int, suffix string) string {
	if tenths := (n*10 + div/2) / div; tenths < 100 {
		return fmt.Sprintf("%d.%d%s", tenths/10, tenths%10, suffix)
	}
	return fmt.Sprintf("%d%s", (n+div/2)/div, suffix)
}

// groupByOriginStale buckets items by (origin, stale) in first-appearance
// order and moves the stale buckets to the end, the shape both
// provenance groupers render. key returns the origin and whether the
// item is stale; newGroup builds a bucket from its first item; add folds
// an item into its bucket.
func groupByOriginStale[T, G any](items []T, key func(T) (string, bool), newGroup func(T) G, add func(*G, T)) []G {
	idx := map[string]int{}
	var groups []G
	var isStale []bool
	for _, it := range items {
		origin, stale := key(it)
		k := origin
		if stale {
			k += "\x00stale"
		}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, newGroup(it))
			isStale = append(isStale, stale)
		}
		add(&groups[i], it)
	}
	out := make([]G, 0, len(groups))
	for _, want := range []bool{false, true} {
		for i, g := range groups {
			if isStale[i] == want {
				out = append(out, g)
			}
		}
	}
	return out
}

// upgradeCompare is the file comparison the [upgrade] prompt carries:
// what the post serves against what is on disk, a line each. It lives in
// the prompt rather than on the source row, which has no width for it.
// The return value carries its own separator - a bare space when the post
// published nothing, so the prompt still reads as one sentence.
func upgradeCompare(s models.ImageSource, img models.Image) string {
	post := fileFacts(s.PostWidth, s.PostHeight, s.PostExt, s.PostSize)
	if post == "" {
		return " "
	}
	label := s.Site
	if label == "" {
		label = "the post"
	}
	pad := max(len("yours"), len(label)) + 2
	return fmt.Sprintf("\n\n%-*s%s\n%-*s%s\n\n",
		pad, "yours", fileFacts(derefOr(img.Width), derefOr(img.Height), strings.ToLower(img.FileType), img.FileSize),
		pad, label, post)
}

// fileFacts joins whatever of "WxH ext - size" is known, empty when none
// of it is.
func fileFacts(w, h int, ext string, size int64) string {
	var parts []string
	if w > 0 && h > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", w, h))
	}
	if ext != "" {
		parts = append(parts, ext)
	}
	line := strings.Join(parts, " ")
	if size > 0 {
		if line != "" {
			line += " - "
		}
		line += humanBytesFmt(size)
	}
	return line
}

// derefOr is the nil-safe read the pointer-valued row fields need: a column
// the decode never filled renders as its zero value rather than panicking.
func derefOr[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// categoryColor resolves a stored category colour to the value templates and
// the markup renderer both emit. Only the #rgb / #rrggbb shape the Categories
// form enforces reaches the variable form; anything else falls back, so an
// unvalidated string can never reach a style attribute.
func categoryColor(color string) string {
	if !tags.IsValidCategoryColor(color) {
		return tags.SafeCategoryColor(color)
	}
	return "var(--cat-" + strings.ToLower(strings.TrimPrefix(color, "#")) + "," + color + ")"
}
