package search

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

// buildSimilarFilter handles the two similar: forms:
//
//   - `similar:<id>` matches every image sharing at least one of the
//     seed's tags. Sharing one is already score > 0, so the bare form
//     skips scoring and rides a plain membership test on
//     idx_image_tags_tag_image.
//   - `similar:<id>~<score>` matches images scoring at least <score>.
//
// Malformed input collapses to `1=0` like id: does.
func (b *whereBuilder) buildSimilarFilter(e FilterExpr) string {
	seedID, threshold, ok := parseSimilarValue(e.Val)
	if !ok {
		return "1=0"
	}
	seed, ok := b.similaritySeed(seedID)
	if !ok || len(seed.TagIDs) == 0 {
		return "1=0"
	}
	placeholders, idArgs := db.InPlaceholders(seed.TagIDs)
	b.args = append(b.args, idArgs...)
	b.args = append(b.args, seedID)
	member := "i.id IN (SELECT it.image_id FROM image_tags it" +
		" WHERE it.tag_id IN (" + placeholders + ") AND it.image_id != ?"
	if threshold < 0 {
		return member + ")"
	}
	// Tighten the gate to the shared count the threshold implies before
	// the exact score prices anything: on a large library that is the
	// difference between scoring a third of the images and scoring the
	// few that can still reach it.
	if need := seed.MinShared(threshold); need > 1 {
		member += " GROUP BY it.image_id HAVING count(*) >= ?"
		b.args = append(b.args, need)
	}
	member += ")"
	// The membership gate runs first, so the score only prices rows that
	// already share something.
	scoreExpr, scoreArgs := seed.ScoreExpr("i.id", "its")
	b.args = append(b.args, scoreArgs...)
	b.args = append(b.args, threshold)
	return member + " AND " + scoreExpr + " >= ?"
}

// parseSimilarValue splits `<id>` and `<id>~<score>`. threshold is -1
// for the bare form; a `~` with nothing usable after it is malformed
// rather than a silent fallback to the bare form.
func parseSimilarValue(val string) (seedID int64, threshold float64, ok bool) {
	val = strings.TrimSpace(val)
	idPart, scorePart := val, ""
	tilde := strings.IndexByte(val, '~')
	if tilde >= 0 {
		idPart, scorePart = val[:tilde], strings.TrimSpace(val[tilde+1:])
	}
	id, err := strconv.ParseInt(strings.TrimSpace(idPart), 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, false
	}
	if tilde < 0 {
		return id, -1, true
	}
	s, err := strconv.ParseFloat(scorePart, 64)
	if err != nil || s < 0 || s > 1 {
		return 0, 0, false
	}
	return id, s, true
}

// similarityOrderClause ranks by overlap with the seed. The score is
// not a column, so it rides a correlated count the same way the
// collection sort reads its position.
func similarityOrderClause(seed tags.OverlapSeed, order string) (string, []any) {
	dir := sqlDir(order, "DESC")
	sub, args := seed.ScoreExpr("i.id", "it")
	return "ORDER BY " + sub + " " + dir + ", i.id " + dir, args
}

// similarityRankSeed resolves the seed the similarity sort ranks
// against: the leftmost positive similar: term in expr. Returns false
// when there is none or it has nothing to match on, and the caller
// keeps the default order.
func similarityRankSeed(database *db.DB, expr Expr) (tags.OverlapSeed, bool) {
	if database == nil {
		return tags.OverlapSeed{}, false
	}
	id, ok := SimilaritySeedID(expr)
	if !ok {
		return tags.OverlapSeed{}, false
	}
	seed, err := tags.LoadOverlapSeed(database, id)
	if err != nil || len(seed.TagIDs) == 0 {
		return tags.OverlapSeed{}, false
	}
	return seed, true
}

// SimilaritySeedID returns the seed the query ranks against: the first
// positive similar: term in reading order. Negated terms are skipped -
// ranking by a seed the operator asked to exclude is never what they
// meant. The gallery handler reads it to default the sort to
// similarity, the way a collection: term defaults it to collection
// order, and to score the page it is about to render.
func SimilaritySeedID(expr Expr) (int64, bool) {
	switch e := expr.(type) {
	case AndExpr:
		if id, ok := SimilaritySeedID(e.Left); ok {
			return id, true
		}
		return SimilaritySeedID(e.Right)
	case OrExpr:
		if id, ok := SimilaritySeedID(e.Left); ok {
			return id, true
		}
		return SimilaritySeedID(e.Right)
	case FilterExpr:
		if e.Key != "similar" {
			return 0, false
		}
		id, _, ok := parseSimilarValue(e.Val)
		return id, ok
	}
	return 0, false
}

// HasSimilarTerm reports whether expr carries a positive similar: term.
func HasSimilarTerm(expr Expr) bool {
	_, ok := SimilaritySeedID(expr)
	return ok
}

// similarityMatchIDs runs the ranked id-only SELECT that the cold
// prev/next and back-page paths read from. The similarity sort has no
// key column to seek on, so both resolve their answer by position in
// this list instead of a cursor comparison.
//
// The fan seeds the adjacency cache the way Execute's page-1 fan does,
// so a detail page reached by a direct link - rather than from a
// gallery that already populated the list - pays the scored pass once
// instead of on every render. A short read stays out of the cache: a
// list at the cap is partial against an unknown total.
//
// ctx is the render's: no fast counter recognises a similar: shape, so
// nothing upstream can bail out of an oversized candidate set ahead of
// this scan, and the deadline is the only bound on it.
func similarityMatchIDs(ctx context.Context, database *db.DB, q Query) []int64 {
	seed, ok := similarityRankSeed(database, q.Expr)
	if !ok {
		return nil
	}
	// The gallery-side fan takes this gate so concurrent misses on one
	// key don't each run the whole scored pass; this fan rides the same
	// key for the same reason. A loser renders without prev/next and
	// the winner leaves the list cached for the next hit. A keyless
	// render has nothing to share and nothing to seed.
	if q.CacheKey != "" {
		if !AdjacencyCacheTryAcquireFan(q.CacheKey) {
			return nil
		}
		defer AdjacencyCacheReleaseFan(q.CacheKey)
	}
	driverLegs, _ := pickAndDriverTag(database, q.Expr, false)
	where, args, hasMissingFilter, _ := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)
	where = andDefaultVisible(where, hasMissingFilter)
	ids := fanSimilarityIDs(ctx, database, seed, q.Order, where, args)
	if len(ids) < adjacencyCacheMaxIDs {
		AdjacencyCacheSet(q.CacheKey, ids)
	}
	return ids
}

// fanSimilarityIDs returns the match set ranked by overlap with the
// seed. The score's denominator - the candidate's own counted-tag
// total - comes from the cached tallies rather than the correlated
// subquery, which re-derives it through both tag joins for every
// candidate on every render. Falls back to the scored ORDER BY when
// the tallies are unavailable.
func fanSimilarityIDs(ctx context.Context, database *db.DB, seed tags.OverlapSeed, order, where string, args []any) []int64 {
	totals, err := counts.CountedTagTotals(ctx, database, seed.MaxUsage)
	if err != nil {
		orderClause, orderArgs := similarityOrderClause(seed, order)
		return fetchSortedMatchIDs(ctx, database, "", where, args, orderClause, orderArgs, adjacencyCacheMaxIDs)
	}
	ids, err := db.QueryIDsContext(ctx, database.Read,
		"SELECT i.id FROM images i WHERE "+where+" LIMIT ?",
		append(slices.Clone(args), adjacencyCacheMaxIDs)...)
	if err != nil || len(ids) == 0 {
		return nil
	}
	shared, err := sharedTagCounts(ctx, database, seed)
	if err != nil {
		return nil
	}

	type ranked struct {
		id    int64
		score float64
	}
	rows := make([]ranked, len(ids))
	for i, id := range ids {
		// A candidate with no counted tags scores NULL in SQL, which
		// sorts below every number: last under DESC, first under ASC,
		// which is what a negative sentinel reproduces.
		score := -1.0
		if total := totals.Total(id); total > 0 {
			score = tags.OverlapScore(int(shared[id]), len(seed.TagIDs), int(total))
		}
		rows[i] = ranked{id: id, score: score}
	}
	asc := order == "asc"
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return (rows[i].score < rows[j].score) == asc
		}
		return (rows[i].id < rows[j].id) == asc
	})
	for i, r := range rows {
		ids[i] = r.id
	}
	return ids
}

// sharedTagCounts tallies how many of the seed's counted tags each
// image carries, in one grouped read of idx_image_tags_tag_image.
func sharedTagCounts(ctx context.Context, database *db.DB, seed tags.OverlapSeed) (map[int64]int32, error) {
	placeholders, args := db.InPlaceholders(seed.TagIDs)
	rows, err := database.Read.QueryContext(ctx,
		"SELECT it.image_id, count(*) FROM image_tags it WHERE it.tag_id IN ("+
			placeholders+") GROUP BY it.image_id", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]int32)
	for rows.Next() {
		var id int64
		var n int32
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
