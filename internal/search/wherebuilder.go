package search

import (
	"cmp"
	"database/sql"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/tags"
	"github.com/monbooru/monbooru/internal/upgrade"
)

// isPureTagExpr reports whether expr's data SELECT should pin
// idx_images_ingested_visible (or _filesize_visible) instead of
// letting the planner pick. Tag leaves and the cat:/rating:/tagged:/
// autotagged:/inbox: filter keywords qualify because their per-row
// EXISTS rides idx_image_tags_image cleanly. The v1.7.2 metadata
// keywords without a covering column index (width, height, date,
// ratio, pages, tagcount) also qualify: each evaluates to a per-row
// column read or a small predicate, so walking the partial sort
// index in (ingested_at, id) order with the predicate as a filter
// beats the fall-back to idx_images_missing + USE TEMP B-TREE FOR
// ORDER BY. fav / source / source_type (ai) / folder / file_type
// (mime/type) / file_size / collection / hash / duration /
// origin (via) / sd-metadata-backed (name / prompt / model /
// sampler / seed) all have their own selective column or partial
// index so the planner picks fine on its own; pinning the sort
// index there forces a 1 M-row scan that the seek would otherwise
// short-circuit.
// andDefaultVisible appends the default `i.is_missing = 0` predicate
// unless the caller's expression already pinned an explicit missing:
// filter (in which case `hasMissingFilter` is true and `where` is
// returned unchanged).
func andDefaultVisible(where string, hasMissingFilter bool) string {
	if hasMissingFilter {
		return where
	}
	if where == "" {
		return "i.is_missing = 0"
	}
	return where + " AND i.is_missing = 0"
}

// detectPureFolder returns the three-leg split of a root-level
// `folder:<val>` predicate: the equality leg, plus the half-open
// subfolder range [val+"/", val+"0") which mirrors buildFilterExpr's
// folder handling and fastCountFolder's split. '0' is the codepoint
// immediately after '/'; a bare [val, val+"0") would leak siblings
// sharing the prefix followed by an ASCII char below '/' ("anime-2024"
// and "anime " both sit between "anime" and "anime0" lexicographically).
// active is false when expr isn't a bare folder filter and the caller
// should stay on the per-image where + args it already built.
func detectPureFolder(expr Expr) (active bool, eq, lo, hi string) {
	f, ok := expr.(FilterExpr)
	if !ok || f.Key != "folder" || f.Val == "" {
		return false, "", "", ""
	}
	return true, f.Val, f.Val + "/", f.Val + "0"
}

// sortIndexHint returns the ` INDEXED BY ...` clause to pin the partial
// sort index when the planner would otherwise pick idx_images_missing
// and materialise a temp B-tree for ORDER BY. Returns "" when a more
// selective per-column hint should win, or when the query isn't a pure
// tag predicate. A non-empty columnFilterIndexHint always overrides.
func sortIndexHint(expr Expr, sort string, hasMissingFilter, ceilingRewrote bool) string {
	if h := columnFilterIndexHint(expr, sort); h != "" {
		return h
	}
	if hasMissingFilter || (expr != nil && !isPureTagExpr(expr)) {
		return ""
	}
	switch sort {
	case "filesize":
		if ceilingRewrote {
			return " INDEXED BY idx_images_filesize_rating_visible"
		}
		return " INDEXED BY idx_images_filesize_visible"
	case "", "newest":
		if ceilingRewrote {
			return " INDEXED BY idx_images_ingested_rating_visible"
		}
		return " INDEXED BY idx_images_ingested_visible"
	}
	return ""
}

// WalkLeaves visits every leaf of expr, descending through And / Or /
// Not. visit returns false to stop the walk early.
func WalkLeaves(expr Expr, visit func(Expr) bool) bool {
	switch e := expr.(type) {
	case AndExpr:
		return WalkLeaves(e.Left, visit) && WalkLeaves(e.Right, visit)
	case OrExpr:
		return WalkLeaves(e.Left, visit) && WalkLeaves(e.Right, visit)
	case NotExpr:
		return WalkLeaves(e.Expr, visit)
	}
	return visit(expr)
}

// anyLeaf reports whether any leaf satisfies pred; allLeaves whether
// every one does. A nil expr reaches pred as a leaf rather than reading
// as an empty set, and every pred here answers false for it, so both
// report false on nil.
func anyLeaf(expr Expr, pred func(Expr) bool) bool {
	return !WalkLeaves(expr, func(e Expr) bool { return !pred(e) })
}

func allLeaves(expr Expr, pred func(Expr) bool) bool {
	return WalkLeaves(expr, pred)
}

func isPureTagExpr(expr Expr) bool {
	return allLeaves(expr, func(e Expr) bool {
		switch v := e.(type) {
		case TagExpr:
			return true
		case FilterExpr:
			switch v.Key {
			case "cat", "rating", "tagged", "autotagged", "stale", "inbox",
				"width", "height", "date", "ratio", "pages", "tagcount":
				return true
			}
			return !searchkw.IsKeyword(v.Key)
		}
		return false
	})
}

// containsMissingFilter reports whether expr carries a `missing:` filter
// anywhere in the AST. Used to gate optimisations whose density
// estimates assume `is_missing = 0` (the implicit visibility filter).
func containsMissingFilter(expr Expr) bool {
	return anyLeaf(expr, func(e Expr) bool {
		v, ok := e.(FilterExpr)
		return ok && v.Key == "missing"
	})
}

// columnFilterIndexHint returns the INDEXED BY clause for a single
// column-filter at the root of expr where a partial visible index
// would be more selective than the planner's default fallback to
// idx_images_missing + TEMP B-TREE FOR ORDER BY. Returns "" when
// the expression isn't a single root-filter that needs the hint.
//
// The COLLATE NOCASE equality predicates (`source = ? COLLATE
// NOCASE`, `series = ? COLLATE NOCASE`, `folder_path = ? COLLATE
// NOCASE`) cannot ride a BINARY-collated index, so without the hint
// the planner skips the wider partial idx_images_source / series /
// folder_visible indexes and walks the visibility-only set. The
// matching NOCASE-collated partials let the seek run directly;
// pinning them avoids the planner's selectivity-based fall-back when
// stats predict a large match set.
//
// The sort axis is passed in so high-cardinality IN-list filters
// (type:image, mime:<broad>) can pin the matching sort-visible index
// and walk in order, sidestepping the temp sort that an
// idx_images_missing scan would otherwise force.
func columnFilterIndexHint(expr Expr, sort string) string {
	f, ok := expr.(FilterExpr)
	if !ok || f.Val == "" {
		return ""
	}
	sortHint := func() string {
		switch sort {
		case "filesize":
			return " INDEXED BY idx_images_filesize_visible"
		default:
			return " INDEXED BY idx_images_ingested_visible"
		}
	}
	switch f.Key {
	case "fav":
		// idx_images_favorited_visible is partial WHERE is_favorited = 1;
		// only the positive polarity rides the covering seek. fav:false
		// falls through to the planner's default plan.
		if strings.EqualFold(f.Val, "true") {
			return " INDEXED BY idx_images_favorited_visible"
		}
	case "type", "mime":
		// High-cardinality IN-lists (type:image covers ~80% of rows,
		// mime:png+jpeg about 60%) defeat the partial file-type
		// indexes - the planner sees too many matches and falls back
		// on idx_images_missing + a temp sort over the visible set.
		// Pinning the sort-axis index walks the result in sorted
		// order and pays a cheap per-row file_type IN-list test.
		return sortHint()
	case "ai":
		// ai:none matches the schema default and runs against most
		// rows on a typical library. Pin the sort index so the data
		// SELECT walks (ingested_at, id) order and stops at LIMIT
		// instead of seeking idx_images_source_type_visible and
		// temp-sorting every visible row by ingested_at.
		if strings.ToLower(f.Val) == "none" {
			return sortHint()
		}
	}
	return ""
}

// containsTagPredicate reports whether expr carries a node whose
// match set is unbounded under the cursor walk: tag-shaped EXISTS
// predicates and folder-prefix LIKEs that match a large fraction of
// the library on popular roots. Drives the random-sort bucket gate
// in ExecuteAdjacent.
func containsTagPredicate(expr Expr) bool {
	return anyLeaf(expr, func(e Expr) bool {
		switch v := e.(type) {
		case TagExpr:
			return true
		case FilterExpr:
			switch v.Key {
			case "cat", "tagged", "autotagged", "stale", "folder", "folderonly":
				return true
			}
			return !searchkw.IsKeyword(v.Key)
		}
		return false
	})
}

// andDriverThreshold caps how many image_tags rows the AND-driver shape
// is willing to materialise as a non-correlated IN(...) subquery. The
// driver replaces the smallest ANDed tag's correlated EXISTS with a
// pre-bounded image_id set so the planner stops walking the
// ingested-at index row by row and instead joins against that set.
// Above the cap, materialising the driver costs more than it saves;
// the slow path stays as is.
const andDriverThreshold = 50000

// driverIDBoundMargin is the safety multiplier applied when picking the
// recent-id bound for multi-leg INTERSECT under newest sort. The bound
// covers the (page*limit)*driverIDBoundMargin most recent visible
// images so even with NTP-scale clock skew or a sparse intersection
// inside the recent slice the page is fully populated. 100x absorbs
// drift up to several hours of ingestion volume on typical libraries.
const driverIDBoundMargin = 100

// driverIDBoundDensityCutoff gates the recent-id bound on the AND
// intersection being dense enough that the bound has plenty of
// matches. Bound applies when total*cutoff >= visibleCount, i.e.
// density >= 1/cutoff. The bound's safety margin produces (page*limit)*
// driverIDBoundMargin candidate rows; with density >= 1/20 = 5% that
// floor stays well above the page size.
const driverIDBoundDensityCutoff = 20

// collectAndedTags returns the positive TagExpr leaves (literal or
// wildcard) reachable from the root through AndExpr nodes only. Leaves
// under OrExpr, NotExpr, or any FilterExpr are skipped because dropping
// their EXISTS in favour of a top-level IN(...) driver would flip
// semantics.
func collectAndedTags(expr Expr) []TagExpr {
	var out []TagExpr
	WalkAndedLeaves(expr, func(e Expr) {
		if v, ok := e.(TagExpr); ok && v.Tag != "" {
			out = append(out, v)
		}
	})
	return out
}

// WalkAndedLeaves visits every leaf reachable from expr through AndExpr
// nodes only. Descending into Or / Not would change what the caller's
// driver predicate means, so the walk stops at them.
func WalkAndedLeaves(expr Expr, visit func(Expr)) {
	var walk func(Expr)
	walk = func(e Expr) {
		if v, ok := e.(AndExpr); ok {
			walk(v.Left)
			walk(v.Right)
			return
		}
		visit(e)
	}
	walk(expr)
}

// expensiveAdjacencyTags reports whether expr's tag predicate forces the
// newest/filesize adjacency cursor to temp-sort a broad matched set
// instead of riding a seekable leg: a wildcard tag (its LIST SUBQUERY
// scans every tag row and resolves to many canonicals) or 3+ ANDed tags.
// Those get the id-bucket bound like the random shape; a lone exact or
// cat-qualified tag keeps its own index seek unbucketed.
func expensiveAdjacencyTags(expr Expr) bool {
	if len(collectAndedTags(expr)) >= 3 {
		return true
	}
	return anyLeaf(expr, func(e Expr) bool {
		v, ok := e.(TagExpr)
		return ok && v.Wildcard != ""
	})
}

// collectAndedFilterLeaves returns the FilterExpr leaves whose Key
// matches a real tag category (e.g. `character:miku`) reachable from
// the root through AndExpr nodes. Same suppression rules as
// collectAndedTags: leaves under OrExpr / NotExpr / non-tag-category
// FilterExpr are skipped. Category recognition delegates to the where
// builder's b.categoryExists helper at resolve time so a nil db
// (test path) doesn't break this collector; here we just filter out
// obvious non-tag keywords.
func collectAndedFilterLeaves(expr Expr) []FilterExpr {
	var out []FilterExpr
	WalkAndedLeaves(expr, func(e Expr) {
		v, ok := e.(FilterExpr)
		if !ok || v.Val == "" || searchkw.IsKeyword(v.Key) {
			return
		}
		out = append(out, v)
	})
	return out
}

// andDriverLeg pairs an ANDed leaf with the canonical tag IDs the
// driver materialises for it. The leaf is a TagExpr or a
// category-qualified FilterExpr (`character:miku`) - the where builder
// checks driverLeaves on both shapes so the per-row EXISTS the leaf
// would otherwise emit is suppressed. Multiple legs feed an INTERSECT
// chain in applyAndDriver; a single leg uses the simpler IN form.
// idBound, when > 0, is pushed into each leg's IN subquery as
// `AND image_id >= ?` so the materialisation is capped to the recent
// id range covering the requested page.
type andDriverLeg struct {
	leaf      Expr
	ids       []int64
	idBound   int64
	idBoundHi int64
}

// pickAndDriverTag chooses one or more ANDed leaves (TagExpr or
// category-qualified FilterExpr) to feed the driver as a non-correlated
// `i.id IN (SELECT image_id FROM image_tags WHERE tag_id IN (...))`
// predicate that bounds the candidate set before the outer query runs.
// Each chosen leaf has its correlated EXISTS suppressed in
// buildWhereDBDriverFull so the predicate isn't paid twice. Returns
// ok=false when nothing can be picked.
//
// Two shapes:
//   - Single leg. The smallest leaf has usage <= andDriverThreshold,
//     so materialising its image set is cheap and the outer EXISTS
//     scan against the bounded candidate set is the win. This covers
//     the rare-tag-wins shape (sparse 3-AND) and any AND that includes
//     a wildcard whose LIST SUBQUERY would otherwise rescan tags per
//     EXISTS evaluation.
//   - Multi-leg INTERSECT. Every leaf is above the threshold (the
//     popular-AND shape). The slow path runs three nested EXISTS over
//     a ~1 M visible-image scan; INTERSECTing each leaf's image_id
//     stream off `idx_image_tags_tag` produces the candidate set in
//     O(sum of leaf sizes) sorted-merge.
//
// Single-literal-at-root is the one shape the helper still bails on
// by default: the planner already handles a single EXISTS via
// idx_images_missing or the partial idx_images_ingested_visible (when
// isPureTagExpr) and materialising would just shift the same work.
// allowSingleLiteral=true overrides this for the random-sort, the
// COUNT-cursor and the bucketed-adjacency callers, where there is no
// covering index for the synthetic random key (random sort), no LIMIT
// to short-circuit the per-row EXISTS scan (rank COUNT), or a bucket
// bound that caps the materialisation a lone popular leaf would
// otherwise be refused for (adjacency).
func pickAndDriverTag(database *db.DB, expr Expr, allowSingleLiteral bool) ([]andDriverLeg, bool) {
	if database == nil {
		return nil, false
	}
	tagLeaves := collectAndedTags(expr)
	filterLeaves := collectAndedFilterLeaves(expr)
	if len(tagLeaves)+len(filterLeaves) == 0 {
		return nil, false
	}
	hasWildcard := false
	for _, leaf := range tagLeaves {
		if leaf.Wildcard != "" {
			hasWildcard = true
			break
		}
	}
	// A single literal at root rides one EXISTS - the planner already
	// handles it well for indexed sorts, materialising would just shift
	// the same work. Wildcards alone are different: their LIST SUBQUERY
	// scans every tag row and the planner can't always cache the
	// result, so even one wildcard benefits from materialisation.
	// Random sort overrides this since the random key has no covering
	// index; the materialised set bounds the temp-sort input. RankInQuery
	// also flips the override on because its COUNT cursor has no LIMIT
	// to short-circuit the per-row EXISTS.
	if !hasWildcard && (len(tagLeaves)+len(filterLeaves)) < 2 && !allowSingleLiteral {
		return nil, false
	}

	type resolved struct {
		leaf  Expr
		ids   []int64
		usage int64
	}
	seenTag := make(map[TagExpr]bool, len(tagLeaves))
	seenFilter := make(map[FilterExpr]bool, len(filterLeaves))
	var legs []resolved
	for _, leaf := range tagLeaves {
		if seenTag[leaf] {
			continue
		}
		seenTag[leaf] = true
		ids, usage, ok := resolveDriverCanonicals(database, leaf)
		if !ok {
			return nil, false
		}
		if len(ids) == 0 {
			// Unknown name (or wildcard with no matching canonicals);
			// the slow path's EXISTS would return zero matches anyway.
			// Don't pick it as driver - the caller still needs an
			// empty-result fast exit elsewhere.
			continue
		}
		legs = append(legs, resolved{leaf: leaf, ids: ids, usage: usage})
	}
	for _, leaf := range filterLeaves {
		if seenFilter[leaf] {
			continue
		}
		seenFilter[leaf] = true
		ids, usage, ok := resolveFilterDriverCanonicals(database, leaf)
		if !ok {
			// Key isn't a real tag category, or the lookup failed -
			// fall back on the slow EXISTS shape (already correct).
			continue
		}
		if len(ids) == 0 {
			continue
		}
		legs = append(legs, resolved{leaf: leaf, ids: ids, usage: usage})
	}
	if len(legs) == 0 {
		return nil, false
	}

	smallestUsage := legs[0].usage
	smallestIdx := 0
	for i, leg := range legs {
		if leg.usage < smallestUsage {
			smallestUsage = leg.usage
			smallestIdx = i
		}
	}

	// Single-leg path when the smallest leaf is cheap enough to feed
	// the IN-driver alone. Materialising one ~ small set + outer EXISTS
	// for the rest is the win the rare-tag-wins shape rides on.
	if smallestUsage <= andDriverThreshold {
		return []andDriverLeg{{leaf: legs[smallestIdx].leaf, ids: legs[smallestIdx].ids}}, true
	}

	// Single popular leaf for an allowSingleLiteral caller: under
	// random sort the data SELECT's ORDER BY ((id * mixed) & ...) has
	// no covering index, so the slow path TEMP-B-TREEs every visible
	// row carrying the predicate; the IN-driver materialisation walks
	// the same row count via idx_image_tags_tag_image and feeds the
	// temp sort a bounded id stream instead of EXISTS-probing every
	// visible image. The bucketed-adjacency caller rides the same
	// branch for a lone popular wildcard, capping the materialisation
	// with the bucket's id bounds. Indexed-sort Execute callers keep
	// their existing planner choice on a single popular literal.
	if allowSingleLiteral && len(legs) == 1 {
		return []andDriverLeg{{leaf: legs[0].leaf, ids: legs[0].ids}}, true
	}

	// Multi-leg INTERSECT path: every leaf is above the threshold, so
	// the slow EXISTS scan walks visible images for every candidate
	// the outer cursor visits. INTERSECTing each leaf's image_id stream
	// off idx_image_tags_tag (sorted by image_id) reduces the candidate
	// set to the actual intersection in sorted-merge time. Skip when
	// there's only one leg above threshold (no INTERSECT to do; same
	// fall-through case the prior single-leg-or-bail logic took).
	if len(legs) < 2 {
		return nil, false
	}
	// Cap at the two least-popular leaves. Each additional materialised
	// leg adds ~smallestUsage rows to the read pool's working set; under
	// c>1 contention every thread pays that cost in parallel and the
	// extra narrowing past two legs no longer offsets it. Leaves dropped
	// here keep their correlated EXISTS via buildWhereDBDriverFull, which
	// runs against the candidate set the INTERSECT already bounded.
	sort.Slice(legs, func(i, j int) bool { return legs[i].usage < legs[j].usage })
	const maxIntersectLegs = 2
	if len(legs) > maxIntersectLegs {
		legs = legs[:maxIntersectLegs]
	}
	out := make([]andDriverLeg, len(legs))
	for i, l := range legs {
		out[i] = andDriverLeg{leaf: l.leaf, ids: l.ids}
	}
	return out, true
}

// tagNamePredicate renders a tag leaf's name match as an SQL predicate on
// col plus its bind argument, keeping the SQL byte-identical at each call
// site. ok is false on an unknown wildcard.
func tagNamePredicate(col, wildcard, tag string) (pred string, arg any, ok bool) {
	switch wildcard {
	case "":
		return col + " = ?", tag, true
	case "prefix":
		return col + ` LIKE ? ESCAPE '\'`, db.EscapeLike(tag) + "%", true
	case "suffix":
		return col + ` LIKE ? ESCAPE '\'`, "%" + db.EscapeLike(tag), true
	case "substring":
		return col + ` LIKE ? ESCAPE '\'`, "%" + db.EscapeLike(tag) + "%", true
	}
	return "", nil, false
}

// resolveDriverCanonicals reads the canonical tag IDs and the sum of
// their usage_count for a TagExpr leaf. Literal exact-name uses the
// `t.name = ?` path (matches buildTagExpr's literal branch); wildcards
// ride the LIKE pattern from buildTagExpr's prefix/substring branches.
// Returns ok=false on a query error, ok=true with an empty slice when
// the name (or pattern) matches no canonicals.
func resolveDriverCanonicals(database *db.DB, leaf TagExpr) ([]int64, int64, bool) {
	pred, arg, ok := tagNamePredicate("t.name", leaf.Wildcard, leaf.Tag)
	if !ok {
		return nil, 0, false
	}
	rows, err := database.Read.Query(
		`SELECT canon.id, canon.usage_count
		 FROM tags t
		 JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		 WHERE `+pred,
		arg,
	)
	if err != nil {
		return nil, 0, false
	}
	defer func() { _ = rows.Close() }()
	return drainCanonicalUsage(rows)
}

// resolveFilterDriverCanonicals reads the canonical tag IDs and the
// sum of their usage_count for a category-qualified FilterExpr leaf
// (e.g. `character:miku`). Same query shape as the whereBuilder's
// resolveCategoryTagByName, with usage_count added so the leg can
// participate in the smallest-first pick. Returns ok=false when the
// key isn't a real tag_categories row.
func resolveFilterDriverCanonicals(database *db.DB, leaf FilterExpr) ([]int64, int64, bool) {
	if leaf.Key == "" || leaf.Val == "" {
		return nil, 0, false
	}
	rows, err := database.Read.Query(
		`SELECT DISTINCT COALESCE(t.canonical_tag_id, t.id), canon.usage_count
		   FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		   JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		  WHERE t.name = ? AND tc.name = ?`,
		strings.ToLower(leaf.Val), leaf.Key,
	)
	if err != nil {
		return nil, 0, false
	}
	defer func() { _ = rows.Close() }()
	return drainCanonicalUsage(rows)
}

// drainCanonicalUsage scans (canonical_id, usage_count) rows into a
// deduped id slice and the usage sum. ok=false on any read error so
// callers fall back to the un-driven plan.
func drainCanonicalUsage(rows *sql.Rows) ([]int64, int64, bool) {
	seen := make(map[int64]bool)
	var ids []int64
	var usage int64
	for rows.Next() {
		var id, count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, 0, false
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		usage += count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false
	}
	return ids, usage, true
}

// applyAndDriver prepends a non-correlated image_tags filter to where
// using the leaf canonical IDs picked by pickAndDriverTag. The driver
// replaces the matched leaves' correlated EXISTS, which the where
// builder has already skipped via driverLeaves.
//
// One leg: `i.id IN (SELECT image_id FROM image_tags WHERE tag_id
// IN (...))`. Multiple legs: each leg's image_id stream is INTERSECTed
// so the bound is the matching intersection of every materialised
// leaf - the popular-AND shape's path off the slow per-row EXISTS scan.
//
// idBound / idBoundHi, when set per-leg, append `AND image_id >= ?`
// (and optionally `AND image_id <= ?`) to that leg's subquery so the
// materialisation is capped to the caller-supplied id range. The
// newest-sort callers use the lower bound to keep just the recent
// slice; ExecuteAdjacent's bucket gate uses both bounds to keep the
// materialised set inside the bucket window so the per-row EXISTS
// it would otherwise pay collapses to a small IN check.
func applyAndDriver(where string, args []any, legs []andDriverLeg) (string, []any) {
	if len(legs) == 0 {
		return where, args
	}
	driverArgs := make([]any, 0)
	parts := make([]string, len(legs))
	for i, leg := range legs {
		placeholders, idArgs := db.InPlaceholders(leg.ids)
		parts[i] = "SELECT image_id FROM image_tags WHERE tag_id IN (" + placeholders + ")"
		driverArgs = append(driverArgs, idArgs...)
		if leg.idBound > 0 {
			parts[i] += " AND image_id >= ?"
			driverArgs = append(driverArgs, leg.idBound)
		}
		if leg.idBoundHi > 0 {
			parts[i] += " AND image_id <= ?"
			driverArgs = append(driverArgs, leg.idBoundHi)
		}
	}
	var driverWhere string
	if len(parts) == 1 {
		driverWhere = "i.id IN (" + parts[0] + ")"
	} else {
		driverWhere = "i.id IN (" + strings.Join(parts, " INTERSECT ") + ")"
	}
	if where == "" || where == "1=1" {
		return driverWhere, driverArgs
	}
	return driverWhere + " AND " + where, append(driverArgs, args...)
}

type whereBuilder struct {
	parts            []string
	args             []any
	hasMissingFilter bool
	// db, when non-nil, lets FilterExpr's default branch check whether
	// an unknown `prefix:value` key matches a real tag category. On
	// miss the whole token is matched as a literal tag so names like
	// `nier:automata` remain searchable. A nil db (test path) keeps
	// the always-category-qualified behaviour.
	db *db.DB
	// ratingIDs caches the four canonical rating tag IDs for the
	// duration of one buildExpr walk. Resolved on first `rating:`
	// encounter so a query without rating predicates pays nothing.
	// Keyed by tag name; `ratingResolved` tracks "queried, cache is
	// authoritative even if some entries are missing". ratingUsage
	// carries usage_count for the same rows so a positive `rating:X`
	// against a level no image yet carries can short-circuit instead
	// of paying a full image scan to find zero matches.
	ratingIDs      map[string]int64
	ratingUsage    map[string]int64
	ratingResolved bool
	// driverLeaves names the ANDed leaves whose correlated EXISTS is
	// suppressed because the caller has prepended a non-correlated
	// `i.id IN (...)` driver covering the same rows. Keys are either
	// TagExpr (literal or wildcard) or FilterExpr (category-qualified).
	// Interface equality compares (concrete type, field values) so a
	// literal `blue` does not silence a `blue*` leaf, nor does
	// `character:miku` silence `artist:miku`. Multi-entry sets
	// correspond to the popular-AND INTERSECT path; single-entry to
	// the rare-tag-wins single-leg path.
	driverLeaves map[Expr]bool
	// ceilingRewrote records that peelCeilingForColumnRewrite swapped
	// the NOT EXISTS chain for an `i.rating_rank <= ?` predicate.
	// Callers consult buildResult to pick the rating-aware partial
	// sort index instead of the bare ingested / filesize partials so
	// the deep-page cursor stays covering.
	ceilingRewrote bool
	// relPresence caches per-source "has any row?" results so
	// repeated relation: predicates inside one builder walk share
	// the lookups. Populated lazily on first relation: encounter;
	// the relPresenceResolved bool gates re-query.
	relPresence         relationPresence
	relPresenceResolved bool
	// similarSeeds caches one resolved seed per image id so repeated
	// similar: terms against the same seed share the read.
	similarSeeds map[int64]tags.OverlapSeed
}

// similaritySeed resolves and caches the shareable tag set for one seed
// image. A nil db (test path) has no seed to read, so the caller emits
// the no-match predicate.
func (b *whereBuilder) similaritySeed(imageID int64) (tags.OverlapSeed, bool) {
	if seed, ok := b.similarSeeds[imageID]; ok {
		return seed, true
	}
	if b.db == nil {
		return tags.OverlapSeed{}, false
	}
	seed, err := tags.LoadOverlapSeed(b.db, imageID)
	if err != nil {
		return tags.OverlapSeed{}, false
	}
	if b.similarSeeds == nil {
		b.similarSeeds = map[int64]tags.OverlapSeed{}
	}
	b.similarSeeds[imageID] = seed
	return seed, true
}

// resolveRatingIDs queries the four canonical rating tag rows and caches
// the result on the builder. A row missing from the result (e.g. the tag
// was pruned at runtime) leaves the entry absent from the map; callers
// fall back to a no-match predicate when their target name is unmapped.
func (b *whereBuilder) resolveRatingIDs() {
	if b.ratingResolved {
		return
	}
	if b.db == nil {
		b.ratingResolved = true
		return
	}
	rows, err := b.db.Read.Query(
		`SELECT t.name, t.id, t.usage_count FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.is_alias = 0
		   AND t.name IN ('general','sensitive','questionable','explicit')`,
	)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	ids := make(map[string]int64, 4)
	usage := make(map[string]int64, 4)
	for rows.Next() {
		var name string
		var id, count int64
		if err := rows.Scan(&name, &id, &count); err != nil {
			return
		}
		ids[name] = id
		usage[name] = count
	}
	// Only latch the cache as authoritative on a clean read - a torn
	// cursor must not leave a partial map that turns rating:X into 1=0.
	if err := rows.Err(); err != nil {
		return
	}
	b.ratingIDs = ids
	b.ratingUsage = usage
	b.ratingResolved = true
}

// resolveCategoryTagByName reads the canonical tag_ids that match
// the named category-qualified tag name (e.g. character:hatsune_miku
// resolves to {miku} plus any aliases that point at it). Returns
// ok=false on a nil-db builder or on a query error so callers fall
// back to the 2-table join shape.
func (b *whereBuilder) resolveCategoryTagByName(category, name string) ([]int64, bool) {
	if b.db == nil || category == "" || name == "" {
		return nil, false
	}
	ids, err := db.QueryIDs(b.db.Read,
		`SELECT DISTINCT COALESCE(t.canonical_tag_id, t.id)
		   FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE t.name = ? AND tc.name = ?`,
		name, category,
	)
	if err != nil {
		return nil, false
	}
	return ids, true
}

// inlineImageTagsTagIDExists emits an EXISTS predicate against
// image_tags that checks the per-image's tag_id is in the supplied
// id list. The ids are inlined as %d literals so the planner sees a
// constant predicate it can short-circuit, and so the predicate
// rides idx_image_tags_image (image_id, tag_id) without dragging
// tags / tag_categories into the per-row evaluation.
func inlineImageTagsTagIDExists(ids []int64) string {
	if len(ids) == 0 {
		return "1=0"
	}
	var b strings.Builder
	b.WriteString("EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.tag_id IN (")
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", id)
	}
	b.WriteString("))")
	return b.String()
}

// imageIDExists builds an EXISTS predicate against the per-image-ids
// subquery FROM <fromBody> WHERE <where>. alias qualifies image_id;
// where omits the alias.image_id = i.id link, which the helper supplies.
func (b *whereBuilder) imageIDExists(fromBody, alias, where string, negate bool) string {
	op := "EXISTS"
	if negate {
		op = "NOT EXISTS"
	}
	if where == "" {
		return fmt.Sprintf("%s (SELECT 1 FROM %s WHERE %s.image_id = i.id)", op, fromBody, alias)
	}
	return fmt.Sprintf("%s (SELECT 1 FROM %s WHERE %s.image_id = i.id AND %s)", op, fromBody, alias, where)
}

// imageTagsPredicate is imageIDExists shorthand for the common
// `image_tags it`-only shape used by tag and tagged/autotagged filters.
func (b *whereBuilder) imageTagsPredicate(where string, negate bool) string {
	return b.imageIDExists("image_tags it", "it", where, negate)
}

// buildWhereDBDriverFull is buildWhereDB with a driver-leaves hint:
// leaves (TagExpr or category-qualified FilterExpr) present in the set
// emit no SQL, because the caller has prepended a non-correlated IN(...)
// (or IN INTERSECT) predicate covering the same rows. An empty/nil set
// leaves the regular build path untouched. The last return is the
// ceiling-rewrote signal: callers that pick a sort-axis INDEXED BY hint
// check it to switch to the rating-aware partial covering index
// (idx_images_ingested_rating_visible /
// idx_images_filesize_rating_visible) so the deep-page cursor walks the
// rating-rank-filtered set entirely off the index.
func buildWhereDBDriverFull(expr Expr, database *db.DB, legs []andDriverLeg) (string, []any, bool, bool) {
	var leaves map[Expr]bool
	if len(legs) > 0 {
		leaves = make(map[Expr]bool, len(legs))
		for _, l := range legs {
			leaves[l.leaf] = true
		}
	}
	b := &whereBuilder{db: database, driverLeaves: leaves}
	expr = b.peelCeilingForColumnRewrite(expr)
	if expr != nil {
		part := b.buildExpr(expr)
		if part != "" {
			b.parts = append(b.parts, part)
		}
	}
	where := strings.Join(b.parts, " AND ")
	where = cmp.Or(where, "1=1")
	return where, b.args, b.hasMissingFilter, b.ceilingRewrote
}

// peelCeilingForColumnRewrite swaps the Ceiling.Apply chain of
// `NOT EXISTS (rating:LEVEL)` for an `i.rating_rank <= ?` predicate so
// the deep-gallery cursor walks the partial
// idx_images_rating_rank_visible covering index instead of three per-
// row correlated subqueries. The remainder (the userExpr part of the
// AST minus the chain) is returned for the regular buildExpr walk.
// Skips on nil db (test path without the column) and on shapes that
// don't carry a recognisable chain.
func (b *whereBuilder) peelCeilingForColumnRewrite(expr Expr) Expr {
	if b.db == nil {
		return expr
	}
	user, levels, ok := extractCeilingShape(expr)
	if !ok {
		return expr
	}
	rank := ceilingRankFromExcluded(levels)
	if rank < 0 {
		// Unknown levels only - leave the chain alone so the slow path
		// keeps the strict-but-correct NOT EXISTS shape.
		return expr
	}
	b.parts = append(b.parts, "i.rating_rank <= ?")
	b.args = append(b.args, rank)
	b.ceilingRewrote = true
	return user
}

// categoryExists reports whether name matches a tag_categories row.
// Returns true on a nil-db (test) builder so the caller's old behaviour
// is preserved.
func (b *whereBuilder) categoryExists(name string) bool {
	if b.db == nil {
		return true
	}
	var n int
	if err := b.db.Read.QueryRow(
		`SELECT 1 FROM tag_categories WHERE name = ? LIMIT 1`, name,
	).Scan(&n); err != nil {
		return false
	}
	return true
}

func buildWhereDB(expr Expr, database *db.DB) (string, []any, bool) {
	where, args, hasMissing, _ := buildWhereDBDriverFull(expr, database, nil)
	return where, args, hasMissing
}

func (b *whereBuilder) buildExpr(expr Expr) string {
	switch e := expr.(type) {
	case AndExpr:
		left := b.buildExpr(e.Left)
		right := b.buildExpr(e.Right)
		if left == "" {
			return right
		}
		if right == "" {
			return left
		}
		return "(" + left + " AND " + right + ")"

	case OrExpr:
		left := b.buildExpr(e.Left)
		right := b.buildExpr(e.Right)
		return "(" + left + " OR " + right + ")"

	case NotExpr:
		inner := b.buildExpr(e.Expr)
		return "NOT (" + inner + ")"

	case TagExpr:
		return b.buildTagExpr(e)

	case FilterExpr:
		return b.buildFilterExpr(e)
	}
	return ""
}

func (b *whereBuilder) buildTagExpr(e TagExpr) string {
	// COALESCE(canonical_tag_id, id) collapses alias rows onto their
	// canonical so a search for the alias name still hits image_tags
	// rows that were re-pointed at the canonical.
	//
	// Wildcard branches escape `_` and `%` in the user-supplied portion
	// so a tag literal containing those characters (legal: `[a-z0-9_...]`)
	// matches itself instead of acting as a LIKE wildcard.
	if b.driverLeaves[e] {
		// Caller prepended a non-correlated IN(...) (or IN INTERSECT)
		// covering this leaf. Returning "" lets AndExpr collapse this
		// branch (only AND-only paths from root were eligible to be
		// marked as a driver leaf, so the empty result never lands
		// inside an OR or NOT).
		return ""
	}
	pred, arg, ok := tagNamePredicate("name", e.Wildcard, e.Tag)
	if !ok {
		pred, arg = "name = ?", e.Tag
	}
	b.args = append(b.args, arg)
	return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE `+pred+`)`, false)
}

// scalarComp emits template with op spliced in and n bound. ok=false
// collapses to "1=0" so each scalar filter case stays one expression.
// buildCompFilter emits a numeric filter from its column template:
// an X..Y range when the value carries one, otherwise a single
// comparison. parseVal reads one half of a range, parseComp an
// operator-plus-value.
func (b *whereBuilder) buildCompFilter(template, val string, parseVal func(string) (any, bool), parseComp func(string) (string, any, bool)) string {
	if s, ok := b.tryRangeComp(template, val, parseVal, parseVal); ok {
		return s
	}
	op, n, ok := parseComp(val)
	return b.scalarComp(template, op, n, ok)
}

func (b *whereBuilder) scalarComp(template, op string, n any, ok bool) string {
	if !ok {
		return "1=0"
	}
	b.args = append(b.args, n)
	return fmt.Sprintf(template, op)
}

// dualMetadataLike matches a substring in either metadata table's named
// column. The IN-subquery shape mirrors buildSeedFilter for the same reason:
// a correlated EXISTS makes the planner walk images and probe each metadata
// table by image_id rowid, one seek per visible row. No index can serve a
// substring LIKE, so the uncorrelated form scans the (small) metadata tables
// once instead - tens of thousands of rows against a million images.
func (b *whereBuilder) dualMetadataLike(sdCol, comfyCol, val string) string {
	if val == "" {
		return "1=0"
	}
	pat := "%" + db.EscapeLike(val) + "%"
	b.args = append(b.args, pat, pat)
	return `(i.id IN (SELECT image_id FROM sd_metadata WHERE ` + sdCol + ` LIKE ? ESCAPE '\')` +
		` OR i.id IN (SELECT image_id FROM comfyui_metadata WHERE ` + comfyCol + ` LIKE ? ESCAPE '\'))`
}

// fileTypeBuckets is the set of file_type values a type:/mime: query may
// resolve to: the membership allowlist for both, and the every-bucket cap
// for type:'s "any media" short-circuit.
var fileTypeBuckets = map[string]bool{
	"jpeg": true, "png": true, "webp": true, "gif": true,
	"mp4": true, "webm": true, "cbz": true,
}

// fileTypeInClause emits `i.file_type IN (...)`. Pass tautologyCap=nil
// to skip the every-bucket short-circuit - the type: aliases (image /
// archive / animated) want it, the mime: filter doesn't.
func fileTypeInClause(seen, tautologyCap map[string]bool) string {
	if len(seen) == 0 {
		return "1=0"
	}
	if tautologyCap != nil && len(seen) == len(tautologyCap) {
		return "1=1"
	}
	quoted := make([]string, 0, len(seen))
	for _, ft := range slices.Sorted(maps.Keys(seen)) {
		quoted = append(quoted, "'"+ft+"'")
	}
	return "i.file_type IN (" + strings.Join(quoted, ", ") + ")"
}

// filterBuilders dispatches FilterExpr.Key to the per-key builder.
// Unknown keys fall through to buildDefaultFilter (category-qualified
// tag searches plus literal colon-bearing tag names).
var filterBuilders = map[string]func(*whereBuilder, FilterExpr) string{
	"system":     (*whereBuilder).buildSystemFilter,
	"fav":        (*whereBuilder).buildFavFilter,
	"inbox":      (*whereBuilder).buildInboxFilter,
	"ai":         (*whereBuilder).buildAIFilter,
	"source":     (*whereBuilder).buildSourceFilter,
	"cat":        (*whereBuilder).buildCatFilter,
	"width":      (*whereBuilder).buildWidthFilter,
	"height":     (*whereBuilder).buildHeightFilter,
	"date":       func(b *whereBuilder, e FilterExpr) string { return b.buildDateFilter(e.Val) },
	"missing":    (*whereBuilder).buildMissingFilter,
	"type":       (*whereBuilder).buildTypeFilter,
	"collection": (*whereBuilder).buildCollectionFilter,
	"pages":      (*whereBuilder).buildPagesFilter,
	"name":       (*whereBuilder).buildNameFilter,
	"size":       (*whereBuilder).buildSizeFilter,
	"mime":       (*whereBuilder).buildMimeFilter,
	"ratio":      (*whereBuilder).buildRatioFilter,
	"tagcount":   (*whereBuilder).buildTagcountFilter,
	"duration":   (*whereBuilder).buildDurationFilter,
	"hash":       (*whereBuilder).buildHashFilter,
	"md5":        (*whereBuilder).buildMD5Filter,
	"id":         (*whereBuilder).buildIDFilter,
	"phash":      (*whereBuilder).buildPhashFilter,
	"relation":   (*whereBuilder).buildRelationFilter,
	"similar":    (*whereBuilder).buildSimilarFilter,
	"prompt":     (*whereBuilder).buildPromptFilter,
	"model":      (*whereBuilder).buildModelFilter,
	"sampler":    (*whereBuilder).buildSamplerFilter,
	"seed":       (*whereBuilder).buildSeedFilter,
	"via":        (*whereBuilder).buildViaFilter,
	"tagged":     (*whereBuilder).buildTaggedFilter,
	"autotagged": (*whereBuilder).buildAutotaggedFilter,
	"stale":      (*whereBuilder).buildStaleFilter,
	"folder":     (*whereBuilder).buildFolderFilter,
	"folderonly": (*whereBuilder).buildFolderonlyFilter,
	"generated":  (*whereBuilder).buildGeneratedFilter,
	"rating":     (*whereBuilder).buildRatingFilter,
	"lookup":     (*whereBuilder).buildLookupFilter,
	"upgrade":    (*whereBuilder).buildUpgradeFilter,
}

func (b *whereBuilder) buildFilterExpr(e FilterExpr) string {
	if h, ok := filterBuilders[e.Key]; ok {
		return h(b, e)
	}
	return b.buildDefaultFilter(e)
}

// buildSystemFilter is the autocomplete-only cheat-sheet trigger; a
// bare `system:` query must not fall into buildDefaultFilter's
// match-all branch.
func (b *whereBuilder) buildSystemFilter(_ FilterExpr) string {
	return "1=0"
}

// boolColumnFilter parses a bool from val and returns "col = 1" /
// "col = 0", or "1=0" on a parse failure (no row matches, so the
// AND with the rest of the WHERE short-circuits).
func boolColumnFilter(col, val string) string {
	b, ok := parseBoolVal(val)
	if !ok {
		return "1=0"
	}
	if b {
		return col + " = 1"
	}
	return col + " = 0"
}

func (b *whereBuilder) buildFavFilter(e FilterExpr) string {
	return boolColumnFilter("i.is_favorited", e.Val)
}

func (b *whereBuilder) buildInboxFilter(e FilterExpr) string {
	return boolColumnFilter("i.is_inbox", e.Val)
}

// buildAIFilter accepts comma-separated source_type and the legacy
// "sd" alias. "any" matches any image carrying a1111 and/or comfyui
// metadata. "none" is the schema default for non-AI images and is
// never combined with another tool in source_type, so it collapses to
// a single-column equality the partial source_type_visible index can
// seek - the four-LIKE shape below would force the planner past
// idx_images_source_type onto idx_images_missing.
func (b *whereBuilder) buildAIFilter(e FilterExpr) string {
	// Lowercased like every sibling filter, so ai:NONE takes the same
	// branch the index hint already assumes it does.
	val := strings.ToLower(e.Val)
	if val == "sd" {
		val = "a1111"
	}
	if val == "any" {
		return "(i.source_type = 'a1111' OR i.source_type = 'comfyui' OR i.source_type = 'a1111,comfyui')"
	}
	if val == "none" {
		return "i.source_type = 'none'"
	}
	b.args = append(b.args, val, "%,"+val, val+",%", "%,"+val+",%")
	return "(i.source_type = ? OR i.source_type LIKE ? OR i.source_type LIKE ? OR i.source_type LIKE ?)"
}

// buildSourceFilter matches any origin whose site equals the value, the same
// any-membership shape as collection:. `source:none` (and the bare `source:`
// form) matches images that carry no origin at all - the freshly-ingested
// triage set; `source:any` is the inverse. `any` / `none` shadow a site
// literally labelled that, as ai:/collection:/relation: do. NOCASE so a user
// who wrote "Pixiv" once and types `source:pixiv` later still finds the row.
//
// The membership test is uncorrelated on purpose: a correlated EXISTS makes
// the planner drive from images and probe image_sources once per visible row,
// which is linear in library size (1.5 s at 1M) however few origins exist.
// The IN form seeks idx_image_sources_site once and rowid-seeks back.
func (b *whereBuilder) buildSourceFilter(e FilterExpr) string {
	switch strings.ToLower(e.Val) {
	case "", "none":
		return b.imageIDExists("image_sources s", "s", "", true)
	case "any":
		return "i.id IN (SELECT image_id FROM image_sources)"
	}
	b.args = append(b.args, e.Val)
	return "i.id IN (SELECT image_id FROM image_sources WHERE site = ? COLLATE NOCASE)"
}

func (b *whereBuilder) buildCatFilter(e FilterExpr) string {
	b.args = append(b.args, e.Val)
	return b.imageIDExists("image_tags it JOIN tags t ON it.tag_id = t.id JOIN tag_categories tc ON tc.id = t.category_id", "it", "tc.name = ?", false)
}

func (b *whereBuilder) buildWidthFilter(e FilterExpr) string {
	return b.buildCompFilter("i.width %s ?", e.Val, parseIntValue, parseIntComp)
}

func (b *whereBuilder) buildHeightFilter(e FilterExpr) string {
	return b.buildCompFilter("i.height %s ?", e.Val, parseIntValue, parseIntComp)
}

// buildMissingFilter sets a flag so any explicit `missing:` opts out
// of the default `AND is_missing = 0`. Without this flag,
// `-missing:false` collapses to `NOT (is_missing = 0) AND
// is_missing = 0` and returns nothing.
func (b *whereBuilder) buildMissingFilter(e FilterExpr) string {
	b.hasMissingFilter = true
	return boolColumnFilter("i.is_missing", e.Val)
}

// buildTypeFilter emits a comma-separated union of named file-type
// buckets:
//
//	image     -> jpeg / png / webp
//	archive   -> cbz (cbz and zip archives of images; the ingest
//	             collapses both extensions onto the 'cbz' file_type)
//	animated  -> gif / mp4 / webm
//
// `-type:animated` is the inverse via the parser's NotExpr; no
// dedicated `animated:false` keyword exists.
func (b *whereBuilder) buildTypeFilter(e FilterExpr) string {
	buckets := map[string][]string{
		"image":    {"jpeg", "png", "webp"},
		"archive":  {"cbz"},
		"animated": {"gif", "mp4", "webm"},
	}
	seen := map[string]bool{}
	for _, v := range strings.Split(strings.ToLower(e.Val), ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		fts, ok := buckets[v]
		if !ok {
			continue
		}
		for _, ft := range fts {
			seen[ft] = true
		}
	}
	return fileTypeInClause(seen, fileTypeBuckets)
}

// buildCollectionFilter matches the operator-edited per-row collection
// label (the comic / manga "series" surface, generalised for plain
// image groupings). Schema column kept as `series` for backwards
// compatibility with existing databases; only the user-facing keyword
// and payload field names carry the new vocabulary. NOCASE so a label
// saved as "My Comic Series" still matches the user typing
// `collection:"my comic series"`.
func (b *whereBuilder) buildCollectionFilter(e FilterExpr) string {
	// Bare `collection:` means "no collection" - the mirror is empty
	// exactly when the image has no membership, so the indexed equality
	// stays. `any` is the inverse (at least one membership) riding the
	// same invariant; like ai:any and relation:any it shadows a
	// collection literally named "any". A named value rides the join
	// table's name index.
	if e.Val == "" {
		return "i.series = ''"
	}
	if strings.ToLower(e.Val) == "any" {
		return "i.series != ''"
	}
	b.args = append(b.args, e.Val)
	return "i.id IN (SELECT image_id FROM image_collections WHERE name = ?)"
}

// COALESCE so non-manga rows (NULL page_count) compare as 0; matches
// the contract that `pages:>=1` excludes images.
func (b *whereBuilder) buildPagesFilter(e FilterExpr) string {
	return b.buildCompFilter("COALESCE(i.page_count, 0) %s ?", e.Val, parseIntValue, parseIntComp)
}

// buildNameFilter does substring match against the filename segment
// after the last "/", so a folder named "vacation" doesn't match every
// file inside it. Empty value matches nothing (a bare `name:` is
// unlikely to be useful and would otherwise alias to "any").
// images.basename_lower is the indexed VIRTUAL column over
// lower(basename(canonical_path)); reading it directly avoids a
// per-row basename() call.
//
// SHA-256 duplicates land in the gallery as additional `image_paths`
// alias rows; the canonical_path-only match would miss any image
// found-but-renamed under a second filename even though the detail
// page lists the alias. The EXISTS clause keeps search-side parity
// with the single-image GET so typing `name:<alias-basename>` finds
// the image whose alias carries that name.
func (b *whereBuilder) buildNameFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=0"
	}
	// FTS5 trigram MATCH seek requires at least 3 characters of
	// overlap to produce a usable token. Inputs shorter than that
	// fall back to the LIKE shape - the planner has no faster path
	// for a one- or two-character substring regardless of index.
	if len([]rune(e.Val)) >= 3 {
		// Quote the user value as a single FTS5 phrase so spaces and
		// punctuation are searched literally instead of being parsed
		// as boolean operators. Double-quote escaping is "".
		ftsQuery := `"` + strings.ReplaceAll(strings.ToLower(e.Val), `"`, `""`) + `"`
		b.args = append(b.args, ftsQuery, ftsQuery)
		return `(i.id IN (SELECT rowid FROM image_basename_canonical_fts WHERE image_basename_canonical_fts MATCH ?) ` +
			`OR i.id IN (SELECT image_id FROM image_basename_alias_fts WHERE image_basename_alias_fts MATCH ?))`
	}
	pat := "%" + db.EscapeLike(strings.ToLower(e.Val)) + "%"
	// image_paths.basename_lower is the VIRTUAL twin of
	// images.basename_lower. The INDEXED BY hint pins the partial
	// `is_canonical = 0` index so the EXISTS subquery rides a seek
	// over the small alias subset rather than `idx_image_paths_image`
	// (which carries every row and pays a per-row is_canonical
	// filter); the basename_lower column drop replaces a per-row
	// lower(basename(ip.path)) function call.
	b.args = append(b.args, pat, pat)
	return `(i.basename_lower LIKE ? ESCAPE '\' ` +
		`OR EXISTS (SELECT 1 FROM image_paths ip INDEXED BY idx_image_paths_aliases WHERE ip.image_id = i.id AND ip.is_canonical = 0 AND ip.basename_lower LIKE ? ESCAPE '\'))`
}

func (b *whereBuilder) buildSizeFilter(e FilterExpr) string {
	return b.buildCompFilter("i.file_size %s ?", e.Val, parseSizeValueAny, parseSizeComp)
}

// buildMimeFilter accepts either the bare file_type bucket ("png") or
// the `image/png` / `video/webm` form. Anything else falls through to
// the empty result. Multiple values comma-separated like `mime:png,jpeg`
// build an IN list. nil tautologyCap to fileTypeInClause so a
// `mime:png,jpeg,...` listing every bucket still emits the literal IN
// list - the 1=1 shortcut belongs to the type: aliases (image /
// archive / animated) where the semantic is "any media", not "the
// union of the listed buckets".
func (b *whereBuilder) buildMimeFilter(e FilterExpr) string {
	val := strings.TrimPrefix(strings.ToLower(e.Val), "image/")
	val = strings.TrimPrefix(val, "video/")
	if val == "" {
		return "1=0"
	}
	seen := map[string]bool{}
	for _, v := range strings.Split(val, ",") {
		v = strings.TrimSpace(v)
		if fileTypeBuckets[v] {
			seen[v] = true
		}
	}
	return fileTypeInClause(seen, nil)
}

// Width and height are nullable on edge cases (a cbz cover that failed
// to decode); NULLIF guards the divide so the row drops out instead of
// erroring.
func (b *whereBuilder) buildRatioFilter(e FilterExpr) string {
	return b.buildCompFilter("(CAST(i.width AS REAL) / NULLIF(i.height, 0)) %s ?", e.Val, parseFloatValue, parseFloatComp)
}

// images.tag_count is a stored column maintained by triggers on
// image_tags (db.Bootstrap). The indexed range seek over
// idx_images_tag_count_visible is one primary-table read per visible
// row.
func (b *whereBuilder) buildTagcountFilter(e FilterExpr) string {
	return b.buildCompFilter("i.tag_count %s ?", e.Val, parseIntValue, parseIntComp)
}

// NULL duration_seconds (non-videos and pre-migration rows) drops out of
// any comparison via the IS NOT NULL guard; the COALESCE form pages:
// uses would force them into "0 seconds" matches, which silently
// advertises every image as a 0-second clip.
func (b *whereBuilder) buildDurationFilter(e FilterExpr) string {
	return b.buildCompFilter("(i.duration_seconds IS NOT NULL AND i.duration_seconds %s ?)", e.Val, parseFloatValue, parseFloatComp)
}

// buildHashFilter dispatches on digest length so pasting either stored
// hash into the bar finds the row. A 32-hex value matched nothing before
// md5 was stored, so widening it changes no existing query.
func (b *whereBuilder) buildHashFilter(e FilterExpr) string {
	val := strings.ToLower(strings.TrimSpace(e.Val))
	if !isHexDigest(val, md5HexLen) && !isHexDigest(val, sha256HexLen) {
		return "1=0"
	}
	b.args = append(b.args, val)
	if len(val) == md5HexLen {
		return "i.md5 = ?"
	}
	return "i.sha256 = ?"
}

// buildMD5Filter takes the exact digest, or the bare form as a
// has-one / doesn't-have-one test the way `phash:` reads.
func (b *whereBuilder) buildMD5Filter(e FilterExpr) string {
	val := strings.ToLower(strings.TrimSpace(e.Val))
	if val == "" {
		return "i.md5 != ''"
	}
	if !isHexDigest(val, md5HexLen) {
		return "1=0"
	}
	b.args = append(b.args, val)
	return "i.md5 = ?"
}

const (
	md5HexLen    = 32
	sha256HexLen = 64
)

func isHexDigest(s string, want int) bool {
	if len(s) != want {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func (b *whereBuilder) buildIDFilter(e FilterExpr) string {
	n, err := strconv.ParseInt(strings.TrimSpace(e.Val), 10, 64)
	if err != nil {
		return "1=0"
	}
	b.args = append(b.args, n)
	return "i.id = ?"
}

// buildPromptFilter is substring match across both SD and ComfyUI
// metadata tables; either is enough for the row to qualify. Mirrors
// the generated: filter's UNION-of-tables shape.
func (b *whereBuilder) buildPromptFilter(e FilterExpr) string {
	return b.dualMetadataLike("prompt", "prompt", e.Val)
}

func (b *whereBuilder) buildModelFilter(e FilterExpr) string {
	return b.dualMetadataLike("model", "model_checkpoint", e.Val)
}

func (b *whereBuilder) buildSamplerFilter(e FilterExpr) string {
	return b.dualMetadataLike("sampler", "sampler", e.Val)
}

// buildSeedFilter takes a 64-bit int seed in both metadata tables.
// Anything else matches nothing. The IN-subquery shape lets the
// planner answer the seek through the partial idx_sd_metadata_seed /
// idx_comfyui_metadata_seed indexes; an EXISTS form would walk images
// first and probe the metadata tables by image_id rowid, missing the
// seed indexes.
func (b *whereBuilder) buildSeedFilter(e FilterExpr) string {
	seed, err := strconv.ParseInt(strings.TrimSpace(e.Val), 10, 64)
	if err != nil {
		return "1=0"
	}
	b.args = append(b.args, seed, seed)
	return "(i.id IN (SELECT image_id FROM sd_metadata WHERE seed = ?) OR i.id IN (SELECT image_id FROM comfyui_metadata WHERE seed = ?))"
}

// buildViaFilter: origin is operator-supplied free text (app name,
// scraper label, ...). NOCASE so a row written by `via:ScraperBot`
// still surfaces when the operator types `via:scraperbot` in the
// search bar, matching the help promise that all searches are
// case-insensitive.
func (b *whereBuilder) buildViaFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=0"
	}
	b.args = append(b.args, e.Val)
	return "i.origin = ? COLLATE NOCASE"
}

func (b *whereBuilder) buildTaggedFilter(e FilterExpr) string {
	return b.boolTagsPredicate("", e.Val)
}

func (b *whereBuilder) buildAutotaggedFilter(e FilterExpr) string {
	return b.boolTagsPredicate("it.is_auto = 1", e.Val)
}

// boolTagsPredicate answers a has-any-such-tag filter: true matches
// images carrying a row the extra predicate selects, false their
// complement.
func (b *whereBuilder) boolTagsPredicate(extra, val string) string {
	v, ok := parseBoolVal(val)
	if !ok {
		return "1=0"
	}
	return b.imageTagsPredicate(extra, !v)
}

// buildLookupFilter matches on the scheduled hash lookup's per-image state.
// `due` shares its predicate with the scheduler phases (internal/lookup), so
// what the operator sees here is what tonight's run will work on; the other
// four read the recorded history directly.
func (b *whereBuilder) buildLookupFilter(e FilterExpr) string {
	switch strings.ToLower(e.Val) {
	case "never":
		return `NOT EXISTS (SELECT 1 FROM image_lookups l WHERE l.image_id = i.id)`
	case "due":
		ptr, ptrArgs := lookup.DueClause(lookup.BackendPTR, time.Now())
		booru, booruArgs := lookup.DueClause(lookup.BackendBooru, time.Now())
		b.args = append(b.args, ptrArgs...)
		b.args = append(b.args, booruArgs...)
		// Each half carries its own candidate clause: the opt-in is per
		// backend, so an image opted out of one is still due on the other.
		return "((" + lookup.CandidateClause(lookup.BackendPTR) + " AND (" + ptr + "))" +
			" OR (" + lookup.CandidateClause(lookup.BackendBooru) + " AND (" + booru + ")))"
	case "missed":
		return `EXISTS (SELECT 1 FROM image_lookups l WHERE l.image_id = i.id
		          AND l.last_result = 'miss' AND l.next_due_at IS NOT NULL)`
	case "exhausted":
		return `EXISTS (SELECT 1 FROM image_lookups l WHERE l.image_id = i.id
		          AND l.backend = 'booru' AND l.last_result = 'miss' AND l.next_due_at IS NULL)`
	case "off":
		// Either backend: the filter exists to find what the operator
		// opted out of, and half an opt-out still counts.
		return "(i.scheduled_lookup = 0 OR i.scheduled_lookup_ptr = 0)"
	}
	return "1=0"
}

// buildUpgradeFilter matches on the per-origin upgrade gate, shared with the
// [upgrade] button through internal/upgrade. `unknown` is the origins nothing
// can compare because the post claimed no md5 - what a source refresh
// resolves. Any other value reads as a site label, so `any` / `none` shadow a
// site labelled that, as source:/relation: do. Uncorrelated like
// buildSourceFilter so the planner seeks idx_image_sources_upgradable once
// instead of probing per visible row.
func (b *whereBuilder) buildUpgradeFilter(e FilterExpr) string {
	candidates := "SELECT s.image_id FROM image_sources s WHERE " + upgrade.CandidateWhere("s")
	switch strings.ToLower(e.Val) {
	case "", "any":
		return "i.id IN (" + candidates + ")"
	case "none":
		return "i.id NOT IN (" + candidates + ")"
	case "unknown":
		// Candidates drop out: a similarity match with no claim has nothing
		// to compare either, but it is already an offer rather than a
		// question a refresh would answer.
		return `(i.id IN (SELECT s.image_id FROM image_sources s WHERE s.url <> '' AND s.md5 = '')
		         AND i.id NOT IN (SELECT s.image_id FROM image_sources s WHERE s.url <> '' AND s.md5 <> '')
		         AND i.id NOT IN (` + candidates + `))`
	case "kept":
		return "i.id IN (SELECT s.image_id FROM image_sources s WHERE s.upgrade_kept = 1)"
	case "sample":
		return b.sampleSuspects(candidates)
	case "bigger":
		// The gain test needs the local row beside the origin, so this one
		// leg joins images rather than riding the candidate subquery alone.
		// Bytes answer only where the post published no dimensions.
		return `i.id IN (SELECT s.image_id FROM image_sources s JOIN images im ON im.id = s.image_id
		                 WHERE ` + upgrade.CandidateWhere("s") + `
		                   AND ((s.post_width > 0 AND s.post_height > 0 AND im.width > 0 AND im.height > 0
		                         AND s.post_width * s.post_height > im.width * im.height)
		                     OR ((s.post_width = 0 OR s.post_height = 0) AND s.post_size > im.file_size)))`
	}
	b.args = append(b.args, e.Val)
	return "i.id IN (" + candidates + " AND s.site = ? COLLATE NOCASE)"
}

// samplePatterns are the filenames a saved preview tends to keep: the
// booru sample prefix, pixiv's resized master, and the suffixes a scaled
// or small variant is served under.
var samplePatterns = []string{`sample\_%`, `%\_master1200%`, `%-scaled%`, `%:small%`, `%:medium%`, `%preview%`}

// sampleSuspects is `upgrade:sample`: an image that carries a post URL and
// looks like the preview of it rather than the post's own file, by name or
// by a geometry the sample sizes are cut to. A suspicion, never a verdict -
// it is its own filter value, never feeds `upgrade:any`, and what resolves
// it is refreshing the source until a digest can be compared. Images whose
// file a source already verified, and the ones already offering an upgrade,
// drop out: neither is a question this can answer.
func (b *whereBuilder) sampleSuspects(candidates string) string {
	names := make([]string, 0, len(samplePatterns))
	for _, p := range samplePatterns {
		b.args = append(b.args, p)
		names = append(names, `i.basename_lower LIKE ? ESCAPE '\'`)
	}
	return `(i.id IN (SELECT s.image_id FROM image_sources s WHERE s.url <> '')
	         AND i.id NOT IN (SELECT s.image_id FROM image_sources s WHERE s.md5_match = 'match')
	         AND i.id NOT IN (` + candidates + `)
	         AND ((` + strings.Join(names, " OR ") + `)
	              OR i.width = 850 OR i.width = 1200 OR i.height = 1200))`
}

// buildStaleFilter matches images carrying a source-dropped (stale) tag.
// `stale:any` (and the bare form) is any stale tag; `stale:none` is the
// inverse; `stale:<tag>` narrows to one tag going stale on the image, resolved
// through the alias canonical like every tag filter. `any` / `none` shadow a
// tag literally named that, as ai:/collection:/relation: do.
func (b *whereBuilder) buildStaleFilter(e FilterExpr) string {
	switch strings.ToLower(e.Val) {
	case "", "any":
		return b.imageTagsPredicate("it.stale = 1", false)
	case "none":
		return b.imageTagsPredicate("it.stale = 1", true)
	}
	b.args = append(b.args, tags.NormalizeTagName(e.Val))
	return b.imageTagsPredicate("it.stale = 1 AND it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)", false)
}

// buildFolderFilter does a recursive match: this folder or anywhere
// beneath it. `folder:` alone is the recursive root - every
// non-missing image lives at or below the gallery root. Use
// `folderonly:` with an empty value for "root directly". Escape LIKE
// metacharacters so a folder named `foo_bar` only matches itself (not
// `fooXbar`). NOCASE on both halves so the help promise of
// case-insensitive search holds for operator-edited folder paths the
// same way it holds for tag names.
func (b *whereBuilder) buildFolderFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=1"
	}
	b.args = append(b.args, e.Val, db.EscapeLike(e.Val)+"/%")
	return `(i.folder_path = ? COLLATE NOCASE OR i.folder_path LIKE ? ESCAPE '\' COLLATE NOCASE)`
}

func (b *whereBuilder) buildFolderonlyFilter(e FilterExpr) string {
	if e.Val == "" {
		return "i.folder_path = ''"
	}
	b.args = append(b.args, e.Val)
	return "i.folder_path = ? COLLATE NOCASE"
}

func (b *whereBuilder) buildGeneratedFilter(e FilterExpr) string {
	b.args = append(b.args, e.Val, e.Val)
	sm := b.imageIDExists("sd_metadata sm", "sm", "sm.generation_hash = ?", false)
	cm := b.imageIDExists("comfyui_metadata cm", "cm", "cm.generation_hash = ?", false)
	return "(" + sm + " OR " + cm + ")"
}

// buildRatingFilter encodes the highest-wins rule: an image matches
// `rating:X` only when it carries X AND no rating ranked above X. Self
// uses EXISTS, the strictly-higher levels are NOT EXISTS, all keyed on
// the cached rating tag IDs so the predicates hit
// idx_image_tags_image directly.
func (b *whereBuilder) buildRatingFilter(e FilterExpr) string {
	val := strings.ToLower(e.Val)
	rank := tags.RatingRank(val)
	if rank < 0 {
		return "1=0"
	}
	b.resolveRatingIDs()
	selfID, ok := b.ratingIDs[val]
	if !ok {
		return "1=0"
	}
	// No image carries this level yet (fresh install state). Skip the
	// EXISTS predicate so the LIMIT-bounded data path stops on the
	// ingested-at index instead of scanning every visible row to find
	// zero matches.
	if b.ratingUsage[val] == 0 {
		return "1=0"
	}
	b.args = append(b.args, selfID)
	parts := []string{b.imageTagsPredicate("it.tag_id = ?", false)}
	for i := rank + 1; i < len(tags.RatingLevels); i++ {
		higherID, ok := b.ratingIDs[tags.RatingLevels[i]]
		if !ok {
			continue
		}
		b.args = append(b.args, higherID)
		parts = append(parts, b.imageTagsPredicate("it.tag_id = ?", true))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// buildDefaultFilter handles unknown keys: either a category-qualified
// tag search ("character:cat") or a literal colon-bearing tag name
// ("nier:automata", ":3"). If the key matches a real category we
// split; otherwise the whole "key:val" is matched as a literal tag
// name. Tag names round-trip through the create path lowercased, so
// the value half gets the same treatment before the equality compare -
// otherwise `character:Asuka` misses a row whose tag was stored as
// `asuka`. A bare `<category>:` with no value collapses to "match any
// image carrying a tag in that category", mirroring `cat:<name>`;
// without this rewrite the gallery surface silently matched every
// image.
func (b *whereBuilder) buildDefaultFilter(e FilterExpr) string {
	if b.categoryExists(e.Key) {
		if e.Val == "" {
			b.args = append(b.args, e.Key)
			return b.imageIDExists("image_tags it JOIN tags t ON it.tag_id = t.id JOIN tag_categories tc ON tc.id = t.category_id", "it", "tc.name = ?", false)
		}
		// Caller prepended a non-correlated IN(...) (or INTERSECT)
		// covering this category-qualified leaf. Returning "" lets
		// AndExpr collapse this branch the same way buildTagExpr does
		// for matched tag leaves.
		if b.driverLeaves[e] {
			return ""
		}
		// Pre-resolve `<category>:<name>` to canonical tag IDs so the
		// per-row predicate is one image_tags seek + small IN check
		// instead of a 2-table join evaluated under every outer cursor
		// iter. ExecuteAdjacent's bucket walk pays this 2 000 times per
		// random-cat back_q render and the resolver result is constant
		// across the walk.
		if ids, ok := b.resolveCategoryTagByName(e.Key, tags.NormalizeTagName(e.Val)); ok {
			if len(ids) == 0 {
				return "1=0"
			}
			return inlineImageTagsTagIDExists(ids)
		}
		b.args = append(b.args, tags.NormalizeTagName(e.Val), e.Key)
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(t.canonical_tag_id, t.id) FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE t.name = ? AND tc.name = ?)`, false)
	}
	if e.Val == "" {
		return "1=1"
	}
	b.args = append(b.args, tags.NormalizeTagName(e.Key+":"+e.Val))
	return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)`, false)
}

// dateFilterRe matches the documented date filter shapes: YYYY,
// YYYY-MM, YYYY-MM-DD, plus the optional time component used by the
// inbox-cluster links: YYYY-MM-DDTHH, YYYY-MM-DDTHH:MM, and
// YYYY-MM-DDTHH:MM:SS. The HELP.md examples show YYYY-MM ranges
// (`date:2024-01..2024-06`) which lexicographically string-compare
// correctly against the ISO-8601 ingested_at column. `buildDateFilter`
// accepts each component (after stripping the optional comparison or
// range syntax) and rejects malformed input with `1=0` rather than
// passing it into a SQL comparison verbatim, which produced silent
// zero-result answers indistinguishable from a real "no images on
// that date" result.
var dateFilterRe = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2}(T\d{2}(:\d{2}(:\d{2})?)?)?)?)?$`)

// parseDatePrecision reads a date filter value at whatever precision the
// caller named, interpreted in the display timezone (time.Local, driven
// by TZ - the same zone every rendered timestamp uses), and returns the
// window's start instant plus the step that closes it. ok=false on a
// value the calendar rejects (month 13, day 32) even when the shape
// regexp passed.
func parseDatePrecision(val string) (start time.Time, next func(time.Time) time.Time, ok bool) {
	for _, p := range []struct {
		layout string
		next   func(time.Time) time.Time
	}{
		{"2006-01-02T15:04:05", func(t time.Time) time.Time { return t.Add(time.Second) }},
		{"2006-01-02T15:04", func(t time.Time) time.Time { return t.Add(time.Minute) }},
		{"2006-01-02T15", func(t time.Time) time.Time { return t.Add(time.Hour) }},
		{"2006-01-02", func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }},
		{"2006-01", func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }},
		{"2006", func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }},
	} {
		if t, err := time.ParseInLocation(p.layout, val, time.Local); err == nil {
			return t, p.next, true
		}
	}
	return time.Time{}, nil, false
}

// dateBoundStart / dateBoundEnd render a value's window edges as the
// UTC `YYYY-MM-DDTHH:MM:SSZ` strings ingested_at stores, so the filter
// means "that day / hour / minute in the operator's timezone" while the
// comparison stays a plain lexicographic string compare. Empty on a
// calendar-invalid value.
func dateBoundStart(val string) string {
	t, _, ok := parseDatePrecision(val)
	if !ok {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05") + "Z"
}

func dateBoundEnd(val string) string {
	t, next, ok := parseDatePrecision(val)
	if !ok {
		return ""
	}
	return next(t).Add(-time.Second).UTC().Format("2006-01-02T15:04:05") + "Z"
}

func (b *whereBuilder) buildDateFilter(val string) string {
	// Two-character operators must be checked before their one-character
	// prefixes so `>=2026-05-14` doesn't match the `>` arm and strip a
	// single char, leaving an unparseable `=2026-05-14`.
	//
	// Every payload names a window in the display timezone, rendered to
	// the UTC bounds `ingested_at` stores. The inclusive operators
	// (`<=`, `>=`) and the exclusive `>` land on the window edge that
	// matches their plain-language reading - `date:<=2026-05-14`
	// catches the last image ingested at 23:59 local, `>` means
	// "strictly after day X" - while `<` compares against the window's
	// start ("before midnight of day X").
	for _, op := range []string{">=", "<=", ">", "<"} {
		if !strings.HasPrefix(val, op) {
			continue
		}
		date := val[len(op):]
		if !dateFilterRe.MatchString(date) {
			return "1=0"
		}
		bound := dateBoundStart(date)
		if op == "<=" || op == ">" {
			bound = dateBoundEnd(date)
		}
		if bound == "" {
			return "1=0"
		}
		b.args = append(b.args, bound)
		return "i.ingested_at " + op + " ?"
	}
	// Open-ended forms collapse to a single inclusive bound: `..X` is
	// `<=X` (every day up to and including X), `X..` is `>=X` (every day
	// from X forward). Mirrors the level-2 cheat-sheet hint that includes
	// `..` next to `>=` / `<=`. The `..` check runs after the operator
	// prefixes above, not before them as the numeric filters do.
	if s, ok := b.tryRangeComp("i.ingested_at %s ?", val,
		dateRangeBound(dateBoundStart), dateRangeBound(dateBoundEnd)); ok {
		return s
	}
	// `=YYYY-MM-DD` is the explicit form of the bare `date:YYYY-MM-DD`
	// shape - the user types the operator the sibling filters (size:=,
	// pages:=, tagcount:=) all accept. Strip the leading `=` and fall
	// through to the bare-form BETWEEN.
	val = strings.TrimPrefix(val, "=")
	if !dateFilterRe.MatchString(val) {
		return "1=0"
	}
	start, end := dateBoundStart(val), dateBoundEnd(val)
	if start == "" || end == "" {
		return "1=0"
	}
	b.args = append(b.args, start, end)
	return "i.ingested_at BETWEEN ? AND ?"
}

func parseCompOp(val string) (string, string) {
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(val, op) {
			return op, val[len(op):]
		}
	}
	return "=", val
}

// parseIntComp wraps parseCompOp with strict int parsing so non-numeric
// values like `width:>=abc` produce ok=false (and an explicit empty
// result via `1=0`) instead of SQLite silently coercing the operand to
// 0 and returning everything wider than 0.
func parseIntComp(val string) (string, any, bool) {
	return compOf(val, func(raw string) (any, bool) {
		n, err := strconv.ParseInt(raw, 10, 64)
		return n, err == nil
	})
}

// parseFloatComp is the float-arg twin of parseIntComp. Used by ratio:
// and duration:. Rejects empty / non-numeric input the same way.
func parseFloatComp(val string) (string, any, bool) {
	return compOf(val, func(raw string) (any, bool) {
		n, err := strconv.ParseFloat(raw, 64)
		return n, err == nil
	})
}

// compOf splits val into its comparison operator and value half and
// runs parse over the half. The value is only read when ok, so a
// rejected parse hands back whatever zero the parser produced.
func compOf(val string, parse func(string) (any, bool)) (string, any, bool) {
	op, raw := parseCompOp(val)
	v, ok := parse(strings.TrimSpace(raw))
	return op, v, ok
}

// parseSizeComp parses a size comparison value like ">=10MB", "<2GiB",
// "= 500K", "1024" (bare bytes). Suffixes are case-insensitive and
// accept either the SI form (KB, MB, GB, TB) or the binary form (KiB,
// MiB, ...). All resolve to powers of 1024 for parity with the rest of
// the UI (humanBytes uses 1024-based MiB). Bare numbers are bytes.
// Returns ok=false on parse failures so callers emit `1=0`.
func parseSizeComp(val string) (string, any, bool) {
	return compOf(val, func(raw string) (any, bool) { return parseSizeValue(raw) })
}

// parseSizeValue parses the bare numeric-plus-unit half of a size:
// filter value (no operator prefix). Shared between parseSizeComp and
// the X..Y range path.
func parseSizeValue(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	i := 0
	for i < len(raw) {
		c := raw[i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			i++
			continue
		}
		break
	}
	numStr := raw[:i]
	unit := strings.TrimSpace(strings.ToLower(raw[i:]))
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, false
	}
	var mult int64
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	case "t", "tb", "tib":
		mult = 1 << 40
	default:
		return 0, false
	}
	return int64(n * float64(mult)), true
}

// tryRangeComp detects the `..` range form in val and emits the matching
// BETWEEN / >= / <= clause through the same `template %s ?` shape
// scalarComp uses. Returns ok=false when val has no `..`, so the caller
// falls through to its comparison helper. Parsing failures still return
// ok=true with a `1=0` clause so the silent-zero shape stays explicit.
// `..X` collapses to `<= X`, `X..` to `>= X`, `X..Y` to `BETWEEN X AND Y`;
// bare `..` is the meaningless 1=0 form date: also emits.
func (b *whereBuilder) tryRangeComp(template, val string, parseFrom, parseTo func(string) (any, bool)) (string, bool) {
	idx := strings.Index(val, "..")
	if idx < 0 {
		return "", false
	}
	fromS := strings.TrimSpace(val[:idx])
	toS := strings.TrimSpace(val[idx+2:])
	if fromS == "" && toS == "" {
		return "1=0", true
	}
	switch {
	case fromS == "":
		toV, ok := parseTo(toS)
		if !ok {
			return "1=0", true
		}
		b.args = append(b.args, toV)
		return fmt.Sprintf(template, "<="), true
	case toS == "":
		fromV, ok := parseFrom(fromS)
		if !ok {
			return "1=0", true
		}
		b.args = append(b.args, fromV)
		return fmt.Sprintf(template, ">="), true
	}
	fromV, ok := parseFrom(fromS)
	if !ok {
		return "1=0", true
	}
	toV, ok := parseTo(toS)
	if !ok {
		return "1=0", true
	}
	b.args = append(b.args, fromV, toV)
	return fmt.Sprintf(template, "BETWEEN ? AND"), true
}

// dateRangeBound wraps a day-bound renderer as a range half-parser: the
// regexp gate lives here so a malformed half collapses to 1=0 the same
// way a non-numeric one does.
func dateRangeBound(bound func(string) string) func(string) (any, bool) {
	return func(raw string) (any, bool) {
		if !dateFilterRe.MatchString(raw) {
			return "", false
		}
		v := bound(raw)
		return v, v != ""
	}
}

// parseIntValue and parseFloatValue are the bare-numeric counterparts of
// parseIntComp / parseFloatComp - tryRangeComp wraps them as the per-half
// parser for X..Y, ..X, and X.. on integer and float filters.
func parseIntValue(s string) (any, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return nil, false
	}
	return n, true
}

func parseFloatValue(s string) (any, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil, false
	}
	return n, true
}

func parseSizeValueAny(s string) (any, bool) {
	n, ok := parseSizeValue(s)
	if !ok {
		return nil, false
	}
	return n, true
}
