package search

import (
	"database/sql"
	"strings"

	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/tags"
)

// fastTagTotal returns a visible-image count for an Expr by reading
// tags.usage_count instead of EXISTS-scanning image_tags. ok=false
// falls back to COUNT(*) for shapes the helper can't bound.
//
// Recognised shapes (each delegates to a fastCount* helper):
//   - TagExpr literal, no wildcard, single canonical - exact.
//   - TagExpr wildcard - sum over canonicals capped at the visible
//     count; upper bound when an image carries more than one matching
//     tag.
//   - NotExpr{TagExpr literal, no wildcard} - exact.
//   - AndExpr/OrExpr of recognised sub-shapes - min/sum, both upper bounds.
//   - FilterExpr{cat:X} - sum over category; upper bound.
//   - FilterExpr where Key is a real tag category and Val is the tag
//     name (e.g. character:miku) - exact.
//
// The upper-bound shapes (wildcard, And, Or, cat:X) are gated by
// fastApproxThreshold: below it the slow EXISTS COUNT finishes within
// budget on the documented large fixture and is exact, so this helper
// only short-circuits when the slow path would actually be slow.
// Pagination's totalPages may over-shoot when the bound is loose;
// rendered pages past the actual end come back empty.
func fastTagTotal(database *db.DB, expr Expr) (int, bool) {
	if n, ok := fastCountCeiling(database, expr); ok {
		return n, true
	}
	switch e := expr.(type) {
	case TagExpr:
		return fastCountTag(database, e)
	case NotExpr:
		return fastCountNot(database, e)
	case AndExpr:
		return fastCountAnd(database, e)
	case OrExpr:
		return fastCountOr(database, e)
	case FilterExpr:
		return fastCountFilter(database, e)
	}
	return 0, false
}

// adjacencyTotalEstimate returns an upper-bound row count for expr
// without applying the fastApproxThreshold gate that fastCountAnd /
// fastCountOr use to fall back to the slow exact COUNT. ExecuteAdjacent's
// bucket decision needs to know whether the candidate set is small
// regardless of size: the gate inside fastTagTotal hides exactly the
// case the bucket harms (sparse intersections that fit in a fast
// unbounded cursor but get capped to 0-1 matches per id-bucket).
//
// AND/OR are walked here so the recursion never crosses the gated
// fastCountAnd / fastCountOr; leaves and NotExpr delegate to
// fastTagTotal, which carries its own leaf-level gates - those handle
// shapes (multi-canonical wildcard sum, cat: sum) where a loose bound
// is still preferable to firing the bucket on a borderline case.
func adjacencyTotalEstimate(database *db.DB, expr Expr) (int, bool) {
	switch e := expr.(type) {
	case AndExpr:
		return fastCountBinary(database, e.Left, e.Right, adjacencyTotalEstimate, minInt)
	case OrExpr:
		sum, ok := fastCountBinary(database, e.Left, e.Right, adjacencyTotalEstimate, addInts)
		if !ok {
			return 0, false
		}
		return capToVisible(database, sum)
	}
	return fastTagTotal(database, expr)
}

// fastCountCeiling matches the cookie-ceiling AST shape: a chain of
// NotExpr{FilterExpr{Key:"rating"}} ANDed together, optionally wrapped
// as AndExpr{userExpr, chain} when the cookie is combined with a user
// search. For the bare chain it counts the visible images whose
// effective rating passes the ceiling straight off the maintained
// images.rating_rank column (the highest rank an image carries, -1 when
// unrated). Counting that single per-image rank stays exact when an
// image carries more than one rating tag - summing the excluded tags'
// usage_count would subtract such a row once per excluded level it
// carries and under-report the total. The render path filters the same
// `rating_rank <= ?` predicate, so the count and the rendered page agree.
//
// A user search ANDed onto the ceiling defers to the slow exact COUNT:
// the chain count says nothing about how the user predicate intersects
// it, and a loose bound would advertise phantom trailing pages. The
// slow COUNT then seeds the adjacency cache so later renders ride the
// fast path with no SQL.
func fastCountCeiling(database *db.DB, expr Expr) (int, bool) {
	user, excluded, ok := extractCeilingShape(expr)
	if !ok || len(excluded) == 0 {
		return 0, false
	}
	if user != nil {
		return 0, false
	}
	rank := ceilingRankFromExcluded(excluded)
	if rank < -1 {
		return 0, false
	}
	// Pin idx_images_rating_rank_visible: rating_rank has only five
	// distinct values, so the sampled sqlite_stat1 (analysis_limit=400)
	// underestimates its per-value cardinality and the planner otherwise
	// counts through the wider idx_images_missing, reading every visible
	// images row (seconds on a cold million-row library). The covering
	// partial index answers the range from a few MB of index pages.
	var total int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images INDEXED BY idx_images_rating_rank_visible
		 WHERE is_missing = 0 AND rating_rank <= ?`, rank,
	).Scan(&total); err != nil {
		return 0, false
	}
	return total, true
}

// extractCeilingShape splits an AST into a userExpr remainder and the
// list of excluded rating levels carried by a NotExpr{FilterExpr{
// Key:"rating"}} chain ANDed onto it. Recognises:
//   - A pure chain (no userExpr; user is nil).
//   - AndExpr{userExpr, chain} - the wrapped shape Ceiling.Apply emits.
//   - Chains nested arbitrarily inside AndExpr nodes; the non-rating
//     leaves recombine into the returned userExpr.
//
// Anything outside an AndExpr/NotExpr/FilterExpr{rating} branch (Or, a
// non-rating Filter or Tag at the top level) is treated as part of the
// userExpr remainder. ok=false only when the entire tree contains zero
// ceiling-rating leaves.
func extractCeilingShape(expr Expr) (Expr, []string, bool) {
	if expr == nil {
		return nil, nil, false
	}
	var levels []string
	user := peelCeilingChain(expr, &levels)
	if len(levels) == 0 {
		return nil, nil, false
	}
	return user, levels, true
}

// ceilingRankFromExcluded resolves the highest rank that should pass
// the cookie ceiling, given the rating levels the ceiling chain
// excludes. Returns -2 when no documented level is excluded (the
// caller should skip the rewrite and fall through to the
// per-leaf NOT EXISTS path). Documented levels are general=0,
// sensitive=1, questionable=2, explicit=3; unknown level names get
// filtered so a typo doesn't push the ceiling rank to the floor.
func ceilingRankFromExcluded(levels []string) int {
	minRank := -1
	for _, name := range levels {
		r := tags.RatingRank(name)
		if r < 0 {
			continue
		}
		if minRank < 0 || r < minRank {
			minRank = r
		}
	}
	if minRank < 0 {
		return -2
	}
	return minRank - 1
}

// peelCeilingChain walks expr, appending each NotExpr{FilterExpr{
// Key:"rating", Val:X}} encountered along AndExpr branches into out
// and returning the non-chain remainder. Anything that is not an
// AndExpr or a chain leaf is returned as-is.
func peelCeilingChain(expr Expr, out *[]string) Expr {
	switch v := expr.(type) {
	case NotExpr:
		if f, ok := v.Expr.(FilterExpr); ok && f.Key == "rating" {
			*out = append(*out, strings.ToLower(f.Val))
			return nil
		}
		return expr
	case AndExpr:
		left := peelCeilingChain(v.Left, out)
		right := peelCeilingChain(v.Right, out)
		switch {
		case left == nil && right == nil:
			return nil
		case left == nil:
			return right
		case right == nil:
			return left
		default:
			return AndExpr{Left: left, Right: right}
		}
	}
	return expr
}

// fastApproxThreshold gates the fast-path approximations on the slow
// path actually being slow. The slow EXISTS-AND-EXISTS / per-row scan
// is bounded by the smallest matching tag's image_tags rows, so for
// counts under this cap the slow path finishes inside the search
// per-query budget and remains exact. Above the cap (popular tags on
// large libraries) the upper-bound short-circuit kicks in.
const fastApproxThreshold = 50000

// rankInQueryMaxRank caps how far down its result set RankInQuery will
// count. What makes a rank expensive is how deep the image sits, not
// how many rows the query matches, and prev/next only ever walks a page
// or two past wherever the operator arrived - five default pages of head
// room covers that and keeps the count off the detail handler's 500 ms
// ceiling. Deeper than the cap the helper reports -1: the page it would
// name is the one the URL already carries, since nothing but a click on
// that page could have got the operator there.
const rankInQueryMaxRank = 200

func fastCountTag(database *db.DB, t TagExpr) (int, bool) {
	if t.Tag == "" {
		return 0, false
	}
	pred, arg, ok := tagNamePredicate("name", t.Wildcard, t.Tag)
	if !ok {
		return 0, false
	}
	canonIDs, err := db.QueryIDs(database.Read,
		`SELECT DISTINCT COALESCE(canonical_tag_id, id) FROM tags WHERE `+pred,
		arg,
	)
	if err != nil {
		return 0, false
	}
	if len(canonIDs) == 0 {
		return 0, true
	}
	if len(canonIDs) == 1 {
		var n int
		if err := database.Read.QueryRow(
			`SELECT usage_count FROM tags WHERE id = ?`, canonIDs[0],
		).Scan(&n); err != nil {
			return 0, false
		}
		return n, true
	}
	// Multi-canonical exact-name (same name in two categories): summing
	// would over-count images carrying both, and the user typed an exact
	// name so they expect an exact answer. Fall back.
	if t.Wildcard == "" {
		return 0, false
	}
	placeholders, args := db.InPlaceholders(canonIDs)
	var n int
	if err := database.Read.QueryRow(
		`SELECT COALESCE(SUM(usage_count), 0) FROM tags WHERE id IN (`+placeholders+`)`,
		args...,
	).Scan(&n); err != nil {
		return 0, false
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	// The sum counts an image once per matching tag it carries, so a
	// pattern that resolves to hundreds of canonicals can total several
	// times the library. Clamp to the visible count like the OR bound:
	// still loose, but never a number the gallery can't hold.
	return capToVisible(database, n)
}

// fastCountNot only handles NOT of a single literal tag. count(!E) is
// visible_count - count(E); applied to an upper-bound count(E) it
// would under-shoot, leaving pagination unable to reach actually-
// existing pages. Restricting to the exact-count case keeps the
// upper-bound invariant of fastTagTotal.
func fastCountNot(database *db.DB, e NotExpr) (int, bool) {
	inner, ok := e.Expr.(TagExpr)
	if !ok || inner.Wildcard != "" || inner.Tag == "" {
		return 0, false
	}
	used, ok := fastCountTag(database, inner)
	if !ok {
		return 0, false
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	return max(visible-used, 0), true
}

// fastCountBinary resolves both sides of a binary node through recurse
// and combines them, bailing as soon as either side has no answer.
func fastCountBinary(database *db.DB, l, r Expr, recurse func(*db.DB, Expr) (int, bool), combine func(int, int) int) (int, bool) {
	lv, ok := recurse(database, l)
	if !ok {
		return 0, false
	}
	rv, ok := recurse(database, r)
	if !ok {
		return 0, false
	}
	return combine(lv, rv), true
}

func addInts(a, b int) int { return a + b }

func minInt(a, b int) int { return min(a, b) }

// capToVisible clamps an OR's summed bound to the visible-image count:
// the two sides may overlap, so the sum only bounds the union. A count
// read that misses yields no answer at all - the fast path reports only
// totals it can stand behind.
func capToVisible(database *db.DB, sum int) (int, bool) {
	v, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	return min(sum, v), true
}

func fastCountAnd(database *db.DB, e AndExpr) (int, bool) {
	minN, ok := fastCountBinary(database, e.Left, e.Right, fastTagTotal, minInt)
	if !ok || minN < fastApproxThreshold {
		return 0, false
	}
	return minN, true
}

func fastCountOr(database *db.DB, e OrExpr) (int, bool) {
	sum, ok := fastCountBinary(database, e.Left, e.Right, fastTagTotal, addInts)
	if !ok || sum < fastApproxThreshold {
		return 0, false
	}
	return capToVisible(database, sum)
}

// fastCountFilters routes a filter key to its shortcut. Dispatch is by
// key, so no helper re-checks the key it was picked for.
var fastCountFilters = map[string]func(*db.DB, FilterExpr) (int, bool){
	"cat":        fastCountCat,
	"generated":  fastCountGenerated,
	"rating":     fastCountRating,
	"tagged":     fastCountTagged,
	"autotagged": fastCountTagged,
	"inbox":      fastCountInbox,
	"ai":         fastCountAI,
	"folder":     fastCountFolder,
	"lookup":     fastCountLookup,
}

// fastCountLookup answers `lookup:never` - the images the hash-lookup
// scheduler has no record for - as the visible count minus the visible
// images that do carry one. The slow path evaluates a NOT EXISTS per
// visible row, which is the whole library on a gallery that has never
// run a lookup and matches every row it walks. The subtrahend costs one
// pass over image_lookups instead, bounded by how many images have ever
// been looked up. The other four values read recorded history and stay
// on the slow path, where their own EXISTS is selective.
func fastCountLookup(database *db.DB, e FilterExpr) (int, bool) {
	if strings.ToLower(e.Val) != "never" {
		return 0, false
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	var recorded int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (SELECT DISTINCT image_id FROM image_lookups) l
		 JOIN images i ON i.id = l.image_id AND i.is_missing = 0`,
	).Scan(&recorded); err != nil {
		return 0, false
	}
	return max(visible-recorded, 0), true
}

// fastCountCat sums usage_count over non-alias tags in the category.
// Aliases have usage_count=0 after merge so they don't actually
// contribute, but the explicit filter mirrors the slow path's
// canonical-only image_tags rows.
func fastCountCat(database *db.DB, e FilterExpr) (int, bool) {
	var n int
	if err := database.Read.QueryRow(
		`SELECT COALESCE(SUM(usage_count), 0) FROM tags
		 WHERE is_alias = 0
		   AND category_id = (SELECT id FROM tag_categories WHERE name = ?)`,
		e.Val,
	).Scan(&n); err != nil {
		return 0, false
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	return n, true
}

func fastCountFilter(database *db.DB, e FilterExpr) (int, bool) {
	if e.Val == "" {
		return 0, false
	}
	if fn, ok := fastCountFilters[e.Key]; ok {
		return fn(database, e)
	}
	if searchkw.IsKeyword(e.Key) {
		// Other filter keywords (fav, ai, source, folder, ...) have their
		// own selective indexes that the planner picks; no fast path
		// shortcut is needed.
		return 0, false
	}
	// Category-qualified single tag (e.g. character:miku) or a
	// literal-tag fallback (e.g. nier:automata). Match buildFilterExpr's
	// categoryExists branch by looking the category up first.
	var catID int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = ?`, e.Key,
	).Scan(&catID); err != nil {
		// Not a real category; the slow path falls back to a literal-
		// tag-name match for the whole "key:val" string. Bail.
		return 0, false
	}
	var n int
	err := database.Read.QueryRow(
		`SELECT canon.usage_count FROM tags t
		 JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		 WHERE t.name = ? AND t.category_id = ?
		 LIMIT 1`,
		strings.ToLower(e.Val), catID,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	return n, true
}

// fastCountRating returns the rating tag's usage_count as the bound
// for `rating:LEVEL`. Exact for the highest level (explicit) since
// nothing higher can hide a carrier; an upper bound for lower levels
// when the highest-rank-wins write rule is held end-to-end (the manual-
// add path and the autotagger both uphold it via PruneLowerRatingsTx).
// Rows that violate the invariant over-shoot here in the same direction
// fastCountAnd / fastCountOr already do - pagination renders an empty
// trailing page rather than dropping a real one.
//
// fastApproxThreshold gates the lower-level upper bound so small
// fixtures with multi-rated images keep the slow path's exact count.
// The highest level skips the gate because its bound is exact.
func fastCountRating(database *db.DB, e FilterExpr) (int, bool) {
	level := strings.ToLower(e.Val)
	rank := tags.RatingRank(level)
	if rank < 0 {
		// Out-of-vocabulary level matches no rows; the slow-path
		// `1=0` short-circuit returns 0 too.
		return 0, true
	}
	var n int
	err := database.Read.QueryRow(
		`SELECT t.usage_count FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.is_alias = 0 AND t.name = ?`,
		level,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	// An empty rating level (usage_count == 0) and the highest level
	// (explicit, no higher to hide it) are both exact regardless of
	// fixture size; everything else gates so test/small libraries stay
	// on the slow path's exact count.
	if n == 0 || rank == len(tags.RatingLevels)-1 {
		return n, true
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	return n, true
}

// fastCountFolder answers `folder:X` counts (the recursive form) via
// two seeks against the partial idx_images_folder_nocase_visible: one
// for the folder itself, one half-open range for paths beneath it.
// Same match set as the slow path's `(folder = ? OR folder LIKE ?
// ESCAPE '\')` shape.
//
// Folder paths in monbooru are stored as POSIX-style relative paths
// without a trailing slash (e.g. "anime/girls"). Subdirs sort
// lexicographically as `X || '/' || ...`. The half-open range
// `path >= X || '/' AND path < X || '0'` covers exactly those entries
// because '0' (0x30) is the codepoint immediately after '/' (0x2f),
// so any string `X/...` falls in the range and any string `X[?]...`
// where `?` is a non-`/` character does not. Quoted forms reach the
// builder with quotes stripped, so the same range works.
//
// COLLATE NOCASE matches the slow path so `folder:Characters` resolves
// against operator-edited folder names regardless of case.
//
// Empty value falls through; the slow path emits `1=1` for `folder:`
// alone (the recursive root, equivalent to "no filter").
func fastCountFolder(database *db.DB, e FilterExpr) (int, bool) {
	rangeLo := e.Val + "/"
	rangeHi := e.Val + "0"
	var n int
	if err := database.Read.QueryRow(
		`SELECT (
		     (SELECT COUNT(*) FROM images
		        WHERE folder_path = ? COLLATE NOCASE AND is_missing = 0)
		   + (SELECT COUNT(*) FROM images INDEXED BY idx_images_folder_nocase_visible
		        WHERE folder_path >= ? COLLATE NOCASE
		          AND folder_path < ? COLLATE NOCASE
		          AND is_missing = 0)
		 )`,
		e.Val, rangeLo, rangeHi,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// fastCountAI answers `ai:VAL` counts via two index-pinned queries:
// list distinct source_type values (a tiny set on real data;
// models.SourceType* names four of them), filter that set against the
// same four LIKE patterns buildFilterExpr uses, then COUNT(*) WHERE
// source_type IN (matching) hitting idx_images_source_type. Same
// matches as the slow path; the difference is the slow path's count
// phase walks visible images evaluating an OR-of-LIKE that no index
// pins, while this helper rides idx_images_source_type for both
// phases.
//
// The bare-equality and the bare-ai aliases (`sd`, `any`, `none`)
// keep the slow path: the bare ones are fast already (single equality
// pinning idx_images_source_type), and `any` is a fixed three-element
// OR the planner handles directly. Empty value is the no-op slow path.
func fastCountAI(database *db.DB, e FilterExpr) (int, bool) {
	val := strings.ToLower(e.Val)
	if val == "sd" {
		val = "a1111"
	}
	if val == "any" || val == "none" {
		return 0, false
	}
	if !strings.Contains(val, ",") {
		// Single-token value already pins idx_images_source via the
		// bare equality emitted by buildFilterExpr. The slow path is
		// not slow here.
		return 0, false
	}
	present, err := db.QueryStrings(database.Read, `SELECT DISTINCT source_type FROM images`)
	if err != nil {
		return 0, false
	}

	// Match buildFilterExpr's 4-LIKE shape: equality, prefix-of-list,
	// suffix-of-list, middle-of-list. Decide membership in app code
	// against the small `present` set so each can ride a comma boundary.
	prefix := val + ","
	suffix := "," + val
	middle := "," + val + ","
	var matching []string
	for _, s := range present {
		if s == val ||
			strings.HasPrefix(s, prefix) ||
			strings.HasSuffix(s, suffix) ||
			strings.Contains(s, middle) {
			matching = append(matching, s)
		}
	}
	if len(matching) == 0 {
		return 0, true
	}
	placeholders, args := db.InPlaceholders(matching)
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE source_type IN (`+placeholders+`) AND is_missing = 0`,
		args...,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// parseBoolVal converts the documented boolean filter values plus the
// common aliases (yes/no, y/n, on/off, 1/0) into a canonical bool.
// ok=false signals the value wasn't a recognised boolean: callers in
// buildFilterExpr emit 1=0 so a typo like `inbox:maybe` produces an
// explicit empty result instead of silently flipping to the false
// cohort the user did not ask for.
func parseBoolVal(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "y", "on", "1":
		return true, true
	case "false", "no", "n", "off", "0":
		return false, true
	}
	return false, false
}

// fastCountTagged returns the exact fast-path count for tagged:true and
// autotagged:true. Computes visible_total - untagged_visible: the
// untagged subtrahend is a NOT-EXISTS walk over image_tags that hits
// multi-second p95 on a million-row library, so it rides the counts
// cache that counts.Invalidate drops on every membership write. Falls
// back to (0, false) on any DB error so the slow path takes over.
func fastCountTagged(database *db.DB, e FilterExpr) (int, bool) {
	val, ok := parseBoolVal(e.Val)
	if !ok || !val {
		return 0, false
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	var untagged int
	if e.Key == "autotagged" {
		untagged, ok = counts.AutoUntaggedVisibleCount(database)
	} else {
		untagged, ok = counts.UntaggedVisibleCount(database)
	}
	if !ok {
		return 0, false
	}
	return max(visible-untagged, 0), true
}

// fastCountInbox returns the visible count for inbox:true / inbox:false
// off idx_images_inbox_visible. Exact at every fixture size: the partial
// index covers the (is_missing = 0, is_inbox = ?) seek directly with no
// row fetch, so the slow path's full visible scan is the wrong tradeoff
// even on small libraries.
func fastCountInbox(database *db.DB, e FilterExpr) (int, bool) {
	val, ok := parseBoolVal(e.Val)
	if !ok {
		// Unparseable boolean: the slow path emits 1=0 (no rows match)
		// so the count is exactly zero. Mark as known so Execute's
		// fastEmpty path short-circuits the data SELECT.
		return 0, true
	}
	target := 0
	if val {
		target = 1
	}
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_inbox = ?`, target,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// fastCountGenerated answers generated:HASH counts from the metadata
// side. The partial idx_sd_metadata_genhash / idx_comfyui_metadata_genhash
// seek directly on the hash, replacing the slow path's per-row EXISTS
// probe over every visible image. UNION dedups image_ids carrying both
// sd and comfy metadata for the same hash.
func fastCountGenerated(database *db.DB, e FilterExpr) (int, bool) {
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (
		     SELECT sm.image_id FROM sd_metadata sm
		       JOIN images i ON i.id = sm.image_id
		       WHERE sm.generation_hash = ? AND i.is_missing = 0
		     UNION
		     SELECT cm.image_id FROM comfyui_metadata cm
		       JOIN images i ON i.id = cm.image_id
		       WHERE cm.generation_hash = ? AND i.is_missing = 0
		 )`,
		e.Val, e.Val,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// fastVisibleCount reads the shared per-DB cache so the count behind
// the NOT / ceiling bounds, the driver's density gate and the
// similarity weights is computed once per invalidation rather than
// once per caller.
func fastVisibleCount(database *db.DB) (int, bool) {
	return counts.VisibleCount(database)
}
