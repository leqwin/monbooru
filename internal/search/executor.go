package search

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// Query holds a parsed query and pagination parameters.
type Query struct {
	Expr       Expr
	Sort       string // "newest" | "filesize" | "random"
	Order      string // "asc" | "desc"
	RandomSeed int64  // used when Sort=="random" for stable ordering
	Page       int    // 1-based
	Limit      int
	// PresetTotal lets a caller that already knows the match count
	// (e.g. cached visible-image count for an unfiltered render) skip
	// the COUNT(*) pass.
	PresetTotal *int
	// SkipCount drops COUNT(*) entirely; result.Total is 0. For callers
	// like the sidebar that consume Results.IDs but never surface Total.
	SkipCount bool
	// CacheKey, when set, ties Execute's match-id list to ExecuteAdjacent's
	// prev/next lookup: the gallery populates the cache when its page-1
	// result holds the full match set, and the detail page reads it
	// instead of refetching. Empty disables both sides.
	CacheKey string
	// OrderCollection names the single pinned collection whose per-image
	// position drives the order-sort. Empty falls back to the home-mirror
	// columns (i.series, i.series_order).
	OrderCollection string
}

// Execute runs the query against the DB and returns paginated results.
func Execute(database *db.DB, q Query) (*models.SearchResult, error) {
	started := time.Now()
	page := max(q.Page, 1)
	limit := q.Limit
	if limit < 1 {
		limit = 40
	}

	// Cache fast path: when the gallery's match-id list is already in the
	// adjacency cache, slice it for the requested page and reread row data
	// by primary key. Skips driver-leg picking, COUNT, and the sorted data
	// SELECT entirely. PresetTotal/SkipCount callers (unfiltered visible
	// path, sidebar) keep their tighter shapes - they don't carry a key in
	// practice but the guard keeps the contract explicit. Stale membership
	// is bounded by the cache TTL; row fields are always fresh.
	if q.CacheKey != "" && !q.SkipCount && q.PresetTotal == nil {
		if ids, ok := AdjacencyCacheGet(q.CacheKey); ok {
			return executeFromCachedIDs(database, ids, page, limit)
		}
	}

	driverLegs, _ := pickAndDriverTag(database, q.Expr, q.Sort == "random")

	// Push a recent-id bound into each multi-leg INTERSECT subquery for
	// newest-DESC pages: id is monotonic with ingested_at on the default
	// ingest path, so the top-(page*limit) rows ordered by ingested_at
	// DESC live within the most recent (page*limit)*driverIDBoundMargin
	// visible images. The bound caps each leg's materialisation by
	// orders of magnitude on a populous tag.
	//
	// Gated on intersection density >= 1/driverIDBoundDensityCutoff so a
	// sparse-AND case (where the top of the result set may not lie in the
	// recent slice) keeps the unbounded INTERSECT - the slow path was
	// already fast there. containsMissingFilter excludes shapes whose
	// match set isn't bounded by the visible (is_missing=0) carrier.
	// Order=asc is excluded because the bound is the recent end of the
	// id range; under ASC the user wants the oldest matches, whose ids
	// sit below the bound and would be filtered out entirely.
	idBounded := false
	if len(driverLegs) >= 2 &&
		(q.Sort == "" || q.Sort == "newest") &&
		q.Order != "asc" &&
		!containsMissingFilter(q.Expr) {
		if total, ok := fastTagTotal(database, q.Expr); ok {
			if visible, vOk := fastVisibleCount(database); vOk &&
				total*driverIDBoundDensityCutoff >= visible {
				targetOffset := (page * limit) * driverIDBoundMargin
				var bound int64
				err := database.Read.QueryRow(
					`SELECT id FROM images INDEXED BY idx_images_ingested_visible
					 WHERE is_missing = 0
					 ORDER BY ingested_at DESC, id DESC
					 LIMIT 1 OFFSET ?`, targetOffset,
				).Scan(&bound)
				if err == nil {
					for i := range driverLegs {
						driverLegs[i].idBound = bound
					}
					idBounded = true
				}
				// ErrNoRows: library smaller than offset; full INTERSECT
				// is already cheap, no bound needed.
			}
		}
	}

	where, args, hasMissingFilter, ceilingRewrote := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	where = andDefaultVisible(where, hasMissingFilter)

	orderClause := buildOrder(q.Sort, q.Order, q.RandomSeed)
	var orderArgs []any
	var rankSeed tags.OverlapSeed
	hasRankSeed := false
	switch {
	case q.Sort == "order" && q.OrderCollection != "":
		orderClause, orderArgs = collectionOrderClause(q.OrderCollection, q.Order)
	case q.Sort == "similarity":
		if seed, ok := similarityRankSeed(database, q.Expr); ok {
			orderClause, orderArgs = similarityOrderClause(seed, q.Order)
			rankSeed, hasRankSeed = seed, true
		}
	}

	// A similarity sort has no key column, so the COUNT and the page's
	// ORDER BY would each walk the whole match set - and the ORDER BY
	// scores it - for one page. Fan first instead and serve every page
	// off that list: page 1 is where the operator normally arrives, but
	// a deep page reached by a link or after the entry's TTL lapsed has
	// the same cost and would otherwise seed nothing for the next hit. A
	// short read (empty, an error, or a set past the cache cap) falls
	// through to the regular path rather than reporting a total it
	// cannot stand behind.
	if q.Sort == "similarity" && hasRankSeed && q.CacheKey != "" &&
		!q.SkipCount && q.PresetTotal == nil && AdjacencyCacheTryAcquireFan(q.CacheKey) {
		ctx, cancel := context.WithTimeout(context.Background(), fanBudget(time.Since(started)))
		ids := fanSimilarityIDs(ctx, database, rankSeed, q.Order, where, args)
		cancel()
		AdjacencyCacheReleaseFan(q.CacheKey)
		if len(ids) > 0 && len(ids) < adjacencyCacheMaxIDs {
			AdjacencyCacheSet(q.CacheKey, ids)
			return executeFromCachedIDs(database, ids, page, limit)
		}
		AdjacencyCacheMarkFanOverBudget(q.CacheKey)
	}

	offset := (page - 1) * limit

	var total int
	fastEmpty := false
	switch {
	case q.SkipCount:
	case q.PresetTotal != nil:
		total = *q.PresetTotal
	default:
		// usage_count is maintained as the visible-image count for the
		// canonical, so a single positive literal tag matches COUNT(*)
		// exactly without scanning images.
		if !hasMissingFilter {
			if n, ok := fastTagTotal(database, q.Expr); ok {
				total = n
				fastEmpty = n == 0
				break
			}
		}
		countSQL := "SELECT COUNT(*) FROM images i WHERE " + where
		if err := database.Read.QueryRow(countSQL, args...).Scan(&total); err != nil {
			return nil, fmt.Errorf("count query: %w", err)
		}
	}

	if fastEmpty {
		return &models.SearchResult{Page: page, Limit: limit, Total: 0}, nil
	}

	// Pin the partial sort index when nothing in the query has its own
	// more-selective index. Without the hint SQLite picks
	// idx_images_missing and materialises a temp B-tree for ORDER BY.
	indexHint := sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)

	// A multi-leg INTERSECT is the dominant filter and the planner
	// drives from it even with the sort index pinned - the pin only
	// downgrades each candidate probe to a skip-scan on the two-column
	// partial. Unpinned, the same probes ride the rowid (7x on the
	// popular-3-AND shape). The id-only fan below keeps the hint: with
	// no LIMIT to stop early, its ordered covering scan is the win.
	dataHint := indexHint
	if len(driverLegs) >= 2 {
		dataHint = ""
	}

	dataSQL := fmt.Sprintf(
		"SELECT "+models.ImageRowColumns+`
		 FROM images i%s
		 WHERE %s
		 %s
		 LIMIT ? OFFSET ?`,
		dataHint, where, orderClause,
	)

	dataArgs := append(slices.Concat(args, orderArgs), limit, offset)
	rows, err := database.Read.Query(dataSQL, dataArgs...)
	if err != nil {
		return nil, fmt.Errorf("data query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var images []models.Image
	for rows.Next() {
		img, scanErr := models.ScanImageRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Seed the cache so subsequent gallery pages and detail prev/next
	// ride the cached id slice instead of re-running the sorted SELECT.
	// Above adjacencyCacheMaxIDs the entry would be partial against an
	// unknown total; skip and let the slow path serve.
	//
	// Single-page case (len(images) == total): ids are already in hand,
	// seed synchronously - free.
	//
	// Multi-page case: page 1 fans synchronously so the immediate next
	// request hits a populated cache. A previous async fan made page 1
	// look faster on a one-shot bench, but the very next request (the
	// real-user page-flip or detail prev/next) raced the fan and missed
	// the cache. The synchronous fan adds one id-only cursor walk to
	// the page-1 wall, capped at adjacencyCacheMaxIDs and bounded by
	// adjacencyFanBudget so an expensive per-row predicate can't hold
	// the request. Pages > 1 still skip the fan because the cache
	// either settled on page 1's request or the operator jumped past it.
	//
	// The recent-id bound holds only for the rows this page needs, so
	// the fan rebuilds the WHERE without it - caching the bounded slice
	// would serve a truncated list with a shrunken Total on every later
	// page. A complete fan is the true match set: its length replaces
	// the loose upper-bound total the fast counter reported.
	if q.CacheKey != "" && total > 0 && total <= adjacencyCacheMaxIDs {
		if len(images) == total {
			ids := make([]int64, len(images))
			for i, img := range images {
				ids[i] = img.ID
			}
			AdjacencyCacheSet(q.CacheKey, ids)
		} else if page == 1 && AdjacencyCacheTryAcquireFan(q.CacheKey) {
			defer AdjacencyCacheReleaseFan(q.CacheKey)
			fanWhere, fanArgs := where, args
			if idBounded {
				for i := range driverLegs {
					driverLegs[i].idBound = 0
				}
				fanWhere, fanArgs, _, _ = buildWhereDBDriverFull(q.Expr, database, driverLegs)
				fanWhere, fanArgs = applyAndDriver(fanWhere, fanArgs, driverLegs)
				fanWhere = andDefaultVisible(fanWhere, hasMissingFilter)
			}
			ctx, cancel := context.WithTimeout(context.Background(), fanBudget(time.Since(started)))
			ids := fetchSortedMatchIDs(ctx, database, indexHint, fanWhere, fanArgs, orderClause, orderArgs, total)
			cancel()
			if len(ids) > 0 {
				AdjacencyCacheSet(q.CacheKey, ids)
				if len(ids) < min(total, adjacencyCacheMaxIDs) {
					total = len(ids)
				}
			} else {
				AdjacencyCacheMarkFanOverBudget(q.CacheKey)
			}
		}
	}

	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: images,
	}, nil
}

// executeFromCachedIDs builds a SearchResult from a cached, sorted
// match-id list: slice for the requested page, fan a single primary-key
// IN-fetch, and re-emit in the cached order. Image rows are always read
// fresh so favorite, tag, and missing-flag mutations surface
// immediately on the next render. Rows returned out of order by the
// planner are reordered in Go to match the cache's sort.
func executeFromCachedIDs(database *db.DB, ids []int64, page, limit int) (*models.SearchResult, error) {
	total := len(ids)
	offset := (page - 1) * limit
	if offset >= total {
		return &models.SearchResult{Page: page, Limit: limit, Total: total}, nil
	}
	end := min(offset+limit, total)
	pageIDs := ids[offset:end]

	placeholders, args := db.InPlaceholders(pageIDs)
	sql := fmt.Sprintf(
		"SELECT "+models.ImageRowColumns+" FROM images i WHERE i.id IN (%s)", placeholders,
	)
	rows, err := database.Read.Query(sql, args...)
	if err != nil {
		return nil, fmt.Errorf("cached id fetch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[int64]models.Image, len(pageIDs))
	for rows.Next() {
		img, scanErr := models.ScanImageRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		byID[img.ID] = img
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]models.Image, 0, len(pageIDs))
	for _, id := range pageIDs {
		if img, ok := byID[id]; ok {
			out = append(out, img)
		}
	}
	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: out,
	}, nil
}

// fetchSortedMatchIDs runs the same WHERE/ORDER BY shape as Execute's
// data SELECT but selects only ids and stops at adjacencyCacheMaxIDs.
// Used by Execute to seed the cache when total exceeds a single page,
// so subsequent page-flips and detail prev/next ride the cache. Errors
// degrade to a nil slice; the caller skips populate and the next render
// retries. A cancelled ctx lands there too, so an over-budget fan never
// caches the prefix it managed to read - a truncated list would report a
// shrunken total and cut prev/next off mid-set.
func fetchSortedMatchIDs(ctx context.Context, database *db.DB, indexHint, where string, args []any, orderClause string, orderArgs []any, total int) []int64 {
	n := min(total, adjacencyCacheMaxIDs)
	sql := fmt.Sprintf(
		`SELECT i.id FROM images i%s WHERE %s %s LIMIT ?`,
		indexHint, where, orderClause,
	)
	qargs := append(slices.Concat(args, orderArgs), n)
	ids, err := db.QueryIDsContext(ctx, database.Read, sql, qargs...)
	if err != nil {
		return nil
	}
	return ids
}

// randomAdjacencyBucketSize caps the id range ExecuteAdjacent scans
// when Sort=="random" carries a tag predicate dense enough to make the
// unbounded temp-sort blow the detail-page budget. The random key has
// no index, so the cursor's ORDER BY temp-sorts every matching row;
// bounding the outer scan to a fixed id-range bucket keeps that sort
// proportional to the bucket. The chain ends at bucket boundaries when
// the gate fires - skipped for candidate sets below fastApproxThreshold
// where the bucket would only ever hold currentID itself.
const randomAdjacencyBucketSize = 2000

// andAdjacencyBucketSize caps the id range ExecuteAdjacent scans for
// newest/filesize sorts when the back_q expression carries 3+ ANDed
// tag predicates. The cursor on `(ingested_at, id)` (or file_size, id)
// otherwise walks past arbitrarily many non-matching rows before
// finding the next match - 7-8 s p95 for a sparse-intersection 3-AND
// late in the result set at large-fixture scale. Bucketing by id caps
// the worst case to a fixed window even when the intersection is
// sparse. Sized larger than randomAdjacencyBucketSize because newest/
// filesize are the common navigation sorts; users expect prev/next to
// reach further than they do under random. Same fastApproxThreshold
// skip applies as random sort: candidate sets below it ride the
// AND-driver's single-leg path unbounded.
const andAdjacencyBucketSize = 10000

// orderCursor builds the cursor predicates and sort clauses for sort=order,
// matching buildOrder's (series, series_order NULLS-last, id) total order.
// before/after match the rows positioned before/after the current row; fwd
// and rev are the forward and reversed ORDER BY for the LIMIT-1 seeks.
func orderCursor(series string, order sql.NullInt64, id int64, desc bool) (before, after, fwd, rev string, beforeArgs, afterArgs []any) {
	if desc {
		fwd = "ORDER BY i.series DESC, i.series_order IS NULL, i.series_order DESC, i.id DESC"
		rev = "ORDER BY i.series ASC, i.series_order IS NULL DESC, i.series_order ASC, i.id ASC"
		if order.Valid {
			before = "(i.series > ? OR (i.series = ? AND i.series_order IS NOT NULL AND (i.series_order, i.id) > (?, ?)))"
			after = "(i.series < ? OR (i.series = ? AND (i.series_order IS NULL OR (i.series_order, i.id) < (?, ?))))"
			beforeArgs = []any{series, series, order.Int64, id}
			afterArgs = []any{series, series, order.Int64, id}
		} else {
			before = "(i.series > ? OR (i.series = ? AND (i.series_order IS NOT NULL OR (i.series_order IS NULL AND i.id > ?))))"
			after = "(i.series < ? OR (i.series = ? AND i.series_order IS NULL AND i.id < ?))"
			beforeArgs = []any{series, series, id}
			afterArgs = []any{series, series, id}
		}
		return
	}
	fwd = "ORDER BY i.series ASC, i.series_order IS NULL, i.series_order ASC, i.id ASC"
	rev = "ORDER BY i.series DESC, i.series_order IS NULL DESC, i.series_order DESC, i.id DESC"
	if order.Valid {
		before = "(i.series < ? OR (i.series = ? AND i.series_order IS NOT NULL AND (i.series_order, i.id) < (?, ?)))"
		after = "(i.series > ? OR (i.series = ? AND (i.series_order IS NULL OR (i.series_order, i.id) > (?, ?))))"
		beforeArgs = []any{series, series, order.Int64, id}
		afterArgs = []any{series, series, order.Int64, id}
	} else {
		before = "(i.series < ? OR (i.series = ? AND (i.series_order IS NOT NULL OR (i.series_order IS NULL AND i.id < ?))))"
		after = "(i.series > ? OR (i.series = ? AND i.series_order IS NULL AND i.id > ?))"
		beforeArgs = []any{series, series, id}
		afterArgs = []any{series, series, id}
	}
	return
}

// collectionCursor is orderCursor for a single pinned collection: the
// total order is (position NULLS-last, id), read off a JOINed pc.position
// column rather than the home-mirror series columns, matching
// collectionOrderClause. The query must JOIN image_collections AS pc.
func collectionCursor(pos sql.NullInt64, id int64, desc bool) (before, after, fwd, rev string, beforeArgs, afterArgs []any) {
	if desc {
		fwd = "ORDER BY pc.position IS NULL, pc.position DESC, i.id DESC"
		rev = "ORDER BY pc.position IS NULL DESC, pc.position ASC, i.id ASC"
		if pos.Valid {
			before = "(pc.position IS NOT NULL AND (pc.position, i.id) > (?, ?))"
			after = "(pc.position IS NULL OR (pc.position, i.id) < (?, ?))"
		} else {
			before = "(pc.position IS NOT NULL OR (pc.position IS NULL AND i.id > ?))"
			after = "(pc.position IS NULL AND i.id < ?)"
		}
	} else {
		fwd = "ORDER BY pc.position IS NULL, pc.position ASC, i.id ASC"
		rev = "ORDER BY pc.position IS NULL DESC, pc.position DESC, i.id DESC"
		if pos.Valid {
			before = "(pc.position IS NOT NULL AND (pc.position, i.id) < (?, ?))"
			after = "(pc.position IS NULL OR (pc.position, i.id) > (?, ?))"
		} else {
			before = "(pc.position IS NOT NULL OR (pc.position IS NULL AND i.id < ?))"
			after = "(pc.position IS NULL AND i.id > ?)"
		}
	}
	if pos.Valid {
		beforeArgs = []any{pos.Int64, id}
		afterArgs = []any{pos.Int64, id}
	} else {
		beforeArgs = []any{id}
		afterArgs = []any{id}
	}
	return
}

// adjacencyPlan is the cursor scaffolding ExecuteAdjacent and RankInQuery
// share before their direction-specific SQL: the resolved WHERE (driver
// legs applied, default-visible folded in, bucket bound appended), the
// pure-folder legs, the sort key, and the before/after comparisons. The
// callers' deltas stay explicit at the call sites: RankInQuery never
// buckets and walks only the before direction.
type adjacencyPlan struct {
	where        string
	args         []any
	indexHint    string
	keyCol       string
	folderActive bool
	folderEq     string
	folderLo     string
	folderHi     string
	collOrder    bool
	prevCmp      string
	nextCmp      string
	prevSort     string
	nextSort     string
	prevArgs     []any
	nextArgs     []any
	ok           bool // false: random sort with no seed; callers return their degraded value
}

// buildAdjacencyPlan resolves the shared plan for currentID under q. A
// scan error means the row is gone; callers map it to their own degraded
// return.
func buildAdjacencyPlan(ctx context.Context, database *db.DB, q Query, currentID int64, driverLegs []andDriverLeg, bucketed bool, bucketLo, bucketHi int64) (adjacencyPlan, error) {
	var p adjacencyPlan
	var ingestedAt string
	var fileSize int64
	var series string
	var seriesOrder sql.NullInt64
	if err := database.Read.QueryRowContext(ctx,
		`SELECT ingested_at, file_size, series, series_order FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize, &series, &seriesOrder); err != nil {
		return p, err
	}

	where, args, hasMissingFilter, ceilingRewrote := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	// On a pure folder predicate the lookup builds two seekable legs as
	// a UNION ALL on idx_images_folder_nocase_visible; the per-image
	// where is dropped so the legs don't double-count the placeholders.
	p.folderActive, p.folderEq, p.folderLo, p.folderHi = detectPureFolder(q.Expr)
	// sort=order has no single key column for the folder UNION-ALL legs.
	if q.Sort == "order" {
		p.folderActive = false
	}
	if p.folderActive {
		where = ""
		args = args[:0]
	}

	where = andDefaultVisible(where, hasMissingFilter)

	if bucketed {
		where = where + " AND i.id BETWEEN ? AND ?"
		args = append(args, bucketLo, bucketHi)
	}

	p.collOrder = q.Sort == "order" && q.OrderCollection != ""
	var pcPos sql.NullInt64
	if p.collOrder {
		// The pinned collection's own position drives the cursor; a missing
		// membership or NULL position sorts in the trailing null group.
		_ = database.Read.QueryRowContext(ctx,
			`SELECT position FROM image_collections WHERE image_id = ? AND name = ?`,
			currentID, q.OrderCollection).Scan(&pcPos)
	}

	if p.collOrder {
		before, after, fwd, rev, bArgs, aArgs := collectionCursor(pcPos, currentID, q.Order == "desc")
		p.prevCmp, p.prevSort, p.prevArgs = before, rev, bArgs
		p.nextCmp, p.nextSort, p.nextArgs = after, fwd, aArgs
	} else if q.Sort == "order" {
		before, after, fwd, rev, bArgs, aArgs := orderCursor(series, seriesOrder, currentID, q.Order == "desc")
		p.prevCmp, p.prevSort, p.prevArgs = before, rev, bArgs
		p.nextCmp, p.nextSort, p.nextArgs = after, fwd, aArgs
	} else {
		var keyVal any
		switch q.Sort {
		case "random":
			if q.RandomSeed == 0 {
				return p, nil
			}
			// SAFETY: %d only produces digits; literal seed interpolation
			// is injection-safe. db.RandomSortKey mirrors random_key()'s
			// hash so the cursor compares Go-computed and SQLite-computed
			// keys against the same scrambled space.
			p.keyCol = fmt.Sprintf("random_key(i.id, %d)", q.RandomSeed)
			keyVal = int64(db.RandomSortKey(currentID, q.RandomSeed))
		case "filesize":
			p.keyCol = "i.file_size"
			keyVal = fileSize
		default: // "newest"
			p.keyCol = "i.ingested_at"
			keyVal = ingestedAt
		}

		// In desc order prev is the next-larger neighbour; in asc/random it's
		// the next-smaller one. Row-value comparison `(A, id) < (?, ?)`
		// seek-prunes against the (A, id) index; the equivalent OR shape
		// does not.
		if q.Order == "asc" || q.Sort == "random" {
			p.prevCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", p.keyCol)
			p.nextCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", p.keyCol)
			p.prevSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", p.keyCol)
			p.nextSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", p.keyCol)
		} else {
			p.prevCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", p.keyCol)
			p.nextCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", p.keyCol)
			p.prevSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", p.keyCol)
			p.nextSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", p.keyCol)
		}
		p.prevArgs = []any{keyVal, currentID}
		p.nextArgs = []any{keyVal, currentID}
	}

	// Pin the partial sort index when nothing in the query has its own
	// more-selective column index, otherwise the planner can pick
	// idx_images_missing and emit a TEMP B-TREE FOR ORDER BY on libraries
	// where is_missing=0 has near-zero selectivity. Mirrors the hint in
	// Execute.
	p.indexHint = sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)
	p.where, p.args = where, args
	p.ok = true
	return p, nil
}

// ExecuteAdjacent returns the image IDs immediately before and after
// currentID under q's sort and filter. Uses cursor-style LIMIT 1
// queries so cost is O(log n) via the ingested_at / file_size indexes,
// not O(matches). Random sort has no key index; for popular tag-
// predicate queries the scan is bounded to a fixed id-range bucket
// containing currentID (see randomAdjacencyBucketSize). Sparse
// candidate sets skip the gate so prev/next reaches every match
// instead of dying at a bucket edge holding only currentID.
// ctx bounds the cold paths; the cache hit above it needs none.
func ExecuteAdjacent(ctx context.Context, database *db.DB, q Query, currentID int64) (*int64, *int64, error) {
	// Cache fast path: when the gallery handed us the sorted match list,
	// prev/next is a slice scan and no SQL fires. Empty key or cache miss
	// falls through to the cursor logic below.
	if ids, ok := AdjacencyCacheGet(q.CacheKey); ok {
		prev, next := findInAdjacencyList(ids, currentID)
		return prev, next, nil
	}

	// Similarity has no key column to seek on, so the neighbours come
	// from a position scan of the ranked list instead of a cursor.
	if q.Sort == "similarity" {
		ids := similarityMatchIDs(ctx, database, q)
		if len(ids) == 0 {
			return nil, nil, nil
		}
		prev, next := findInAdjacencyList(ids, currentID)
		return prev, next, nil
	}

	// Decide the bucket gate ahead of the AND-driver pick so the driver
	// doesn't materialise legs the bucket would render redundant. With
	// the bucket bounding the candidate range to a fixed window
	// (2k rows under random, 10k under newest/filesize), a per-row
	// correlated EXISTS finishes in tens of ms; an INTERSECT of two
	// popular leaves materialises hundreds of thousands of image_tags
	// rows ahead of the BETWEEN bound and dwarfs the bucket's cap.
	//
	// Skip the gate when the candidate set is provably small: a sparse
	// multi-tag intersection scatters its matches across id-space at
	// densities far below one per bucket, so prev/next would terminate
	// on every click. Below fastApproxThreshold the AND-driver's
	// single-leg path keeps the outer cursor scan in budget without
	// bucketing - same threshold the rest of the count helpers gate on.
	smallCandidate := false
	if total, ok := adjacencyTotalEstimate(database, q.Expr); ok && total < fastApproxThreshold {
		smallCandidate = true
	}

	bucketLo, bucketHi := int64(0), int64(0)
	bucketed := false
	switch {
	case smallCandidate:
	case q.Sort == "random" && containsTagPredicate(q.Expr):
		bucketLo = (currentID / randomAdjacencyBucketSize) * randomAdjacencyBucketSize
		bucketHi = bucketLo + randomAdjacencyBucketSize - 1
		bucketed = true
	case (q.Sort == "" || q.Sort == "newest" || q.Sort == "filesize") &&
		expensiveAdjacencyTags(q.Expr):
		// Wildcard or multi-AND adjacency: bound the cursor's outer walk
		// to a fixed id window so a broad or sparse match set can't force
		// a multi-second temp-sort. prev/next stops at the bucket
		// boundary when the bound has neighbours to give; the
		// smallCandidate gate above lifts the cap when matches are
		// scattered too thin to bucket usefully.
		bucketLo = (currentID / andAdjacencyBucketSize) * andAdjacencyBucketSize
		bucketHi = bucketLo + andAdjacencyBucketSize - 1
		bucketed = true
	}

	var driverLegs []andDriverLeg
	if !bucketed {
		driverLegs, _ = pickAndDriverTag(database, q.Expr, q.Sort == "random")
	} else {
		// Bucket gate fired. Pre-resolve any wildcard tag predicate
		// to its canonical id list and bound the materialisation to
		// the bucket window so the per-row EXISTS the slow path would
		// pay (one image_tags seek per matching tag_id, per bucket
		// row) collapses to a small IN check on the cursor's outer
		// walk. A popular substring or prefix wildcard at root would
		// otherwise pay ~30 tag_id seeks per each of 2 000 bucket
		// rows; with the bucket bound the materialised set drops to
		// whatever lives inside that 2 000-id window.
		//
		// allowSingleLiteral=true so a lone popular wildcard still
		// yields its leg: the usage-threshold bail assumes an
		// unbounded materialisation, but the bucket bound below caps
		// it, and without the leg the cursor re-evaluates the wildcard
		// EXISTS along the whole sort-index walk (~0.9 s at 1M).
		driverLegs, _ = pickAndDriverTag(database, q.Expr, true)
		for i := range driverLegs {
			driverLegs[i].idBound = bucketLo
			driverLegs[i].idBoundHi = bucketHi
		}
	}

	p, err := buildAdjacencyPlan(ctx, database, q, currentID, driverLegs, bucketed, bucketLo, bucketHi)
	if err != nil || !p.ok {
		return nil, nil, nil
	}

	lookup := func(cursorCmp, sort string, cursorArgs []any) *int64 {
		var sql string
		var qargs []any
		if p.collOrder {
			qargs = slices.Concat([]any{q.OrderCollection}, p.args, cursorArgs)
			sql = fmt.Sprintf(
				"SELECT i.id FROM images i JOIN image_collections pc ON pc.image_id = i.id AND pc.name = ? WHERE %s AND %s %s LIMIT 1",
				p.where, cursorCmp, sort)
		} else if p.folderActive {
			// UNION ALL of the equality and range legs, each pinned to
			// idx_images_folder_nocase_visible so the planner runs two
			// tight seeks instead of one OR-of-(equality, range) that
			// SQLite resolves with a full index scan + TEMP B-TREE
			// sort. The outer SELECT picks the closer of the two leg
			// winners under the shared cursor sort.
			outer := strings.ReplaceAll(strings.ReplaceAll(sort, p.keyCol, "k"), "i.id", "id")
			legSQL := "SELECT i.id AS id, " + p.keyCol + " AS k FROM images i INDEXED BY idx_images_folder_nocase_visible WHERE %s AND " + p.where + " AND " + cursorCmp + " " + sort + " LIMIT 1"
			sql = "SELECT id FROM (SELECT * FROM (" + fmt.Sprintf(legSQL, "i.folder_path = ? COLLATE NOCASE") +
				") UNION ALL SELECT * FROM (" + fmt.Sprintf(legSQL, "i.folder_path >= ? COLLATE NOCASE AND i.folder_path < ? COLLATE NOCASE") +
				")) " + outer + " LIMIT 1"
			// Leg 1 (equality) takes the folder value, leg 2 (range) the lo
			// and hi bounds; each is followed by the shared where/cursor args.
			qargs = slices.Concat(
				[]any{p.folderEq}, p.args, cursorArgs,
				[]any{p.folderLo, p.folderHi}, p.args, cursorArgs,
			)
		} else {
			qargs = slices.Concat(p.args, cursorArgs)
			sql = fmt.Sprintf("SELECT i.id FROM images i%s WHERE %s AND %s %s LIMIT 1",
				p.indexHint, p.where, cursorCmp, sort)
		}
		var id int64
		if err := database.Read.QueryRow(sql, qargs...).Scan(&id); err != nil {
			return nil
		}
		return &id
	}
	return lookup(p.prevCmp, p.prevSort, p.prevArgs), lookup(p.nextCmp, p.nextSort, p.nextArgs), nil
}

// RankInQuery returns the 0-indexed position currentID would occupy in
// q's sorted result set, computed as a single COUNT against the same
// WHERE shape Execute uses. Callers turn the rank into a 1-indexed
// page via floor(rank / pageSize) + 1. Use it as a cold-path fallback
// for the detail handler's back-link page when AdjacencyCacheGet
// misses; warm calls should hit the cache and skip this helper. The
// count stops at rankInQueryMaxRank, but a sparse predicate can walk a
// long way to reach that many rows, so spawn it in parallel with other
// detail reads and pass a deadline-bound context.
//
// Returns (-1, nil) when the helper can't usefully answer (random
// sort with seed=0, ctx cancelled, a rank past rankInQueryMaxRank).
// The caller degrades to whatever back_page came in on the URL.
func RankInQuery(ctx context.Context, database *db.DB, q Query, currentID int64) (int, error) {
	// Similarity ranks off the same position scan prev/next uses; the
	// cursor COUNT below has no score column to compare against.
	if q.Sort == "similarity" {
		for i, id := range similarityMatchIDs(ctx, database, q) {
			if id == currentID {
				return i, nil
			}
		}
		return -1, nil
	}

	// A single leaf keeps its driver until it is popular enough that
	// materialising every row it carries costs more than the cursor's
	// walk over the rows above currentID - the walk the LIMIT below
	// bounds, and what the materialisation used to stand in for. Random
	// sort keeps it either way: its key has no index for the cursor to
	// ride.
	allowSingle := q.Sort == "random"
	if total, ok := fastTagTotal(database, q.Expr); !ok || total <= fastApproxThreshold {
		allowSingle = true
	}
	driverLegs, _ := pickAndDriverTag(database, q.Expr, allowSingle)

	// The rank counts the rows before currentID, so only the plan's prev
	// comparison is consumed; no bucket gate, the COUNT must see the whole
	// candidate set.
	p, err := buildAdjacencyPlan(ctx, database, q, currentID, driverLegs, false, 0, 0)
	if err != nil {
		return -1, err
	}
	if !p.ok {
		return -1, nil
	}

	var rows string
	var qargs []any
	if p.collOrder {
		rows = fmt.Sprintf(
			"SELECT 1 FROM images i JOIN image_collections pc ON pc.image_id = i.id AND pc.name = ? WHERE %s AND %s",
			p.where, p.prevCmp)
		qargs = slices.Concat([]any{q.OrderCollection}, p.args, p.prevArgs)
	} else if p.folderActive {
		legSQL := "SELECT 1 FROM images i INDEXED BY idx_images_folder_nocase_visible WHERE %s AND " + p.where + " AND " + p.prevCmp
		rows = fmt.Sprintf(legSQL, "i.folder_path = ? COLLATE NOCASE") +
			" UNION ALL " +
			fmt.Sprintf(legSQL, "i.folder_path >= ? COLLATE NOCASE AND i.folder_path < ? COLLATE NOCASE")
		qargs = slices.Concat(
			[]any{p.folderEq}, p.args, p.prevArgs,
			[]any{p.folderLo, p.folderHi}, p.args, p.prevArgs,
		)
	} else {
		rows = fmt.Sprintf(
			"SELECT 1 FROM images i%s WHERE %s AND %s",
			p.indexHint, p.where, p.prevCmp,
		)
		qargs = slices.Concat(p.args, p.prevArgs)
	}

	var rank int
	if err := database.Read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ("+rows+" LIMIT ?)",
		append(qargs, rankInQueryMaxRank+1)...,
	).Scan(&rank); err != nil {
		return -1, err
	}
	if rank > rankInQueryMaxRank {
		return -1, nil
	}
	return rank, nil
}

// DeleteTarget is the minimum bulk-delete needs from a row.
type DeleteTarget struct {
	ID            int64
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
}

// ExecuteForDeleteStream invokes visit for each matching row, streaming
// directly off the cursor so very large result sets never materialise.
// visit returning a non-nil error aborts iteration.
func ExecuteForDeleteStream(database *db.DB, expr Expr, visit func(DeleteTarget) error) error {
	driverLegs, _ := pickAndDriverTag(database, expr, false)
	where, args, hasMissingFilter, _ := buildWhereDBDriverFull(expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)
	where = andDefaultVisible(where, hasMissingFilter)

	rows, err := database.Read.Query(
		"SELECT i.id, i.canonical_path, i.folder_path, i.is_missing FROM images i WHERE "+where+" ORDER BY i.id",
		args...,
	)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var t DeleteTarget
		var isMissing int
		if err := rows.Scan(&t.ID, &t.CanonicalPath, &t.FolderPath, &isMissing); err != nil {
			return err
		}
		t.IsMissing = isMissing == 1
		if err := visit(t); err != nil {
			return err
		}
	}
	return rows.Err()
}

// sidebarMaxPerCategory caps the sidebar tag list per category so the
// tree stays legible on long-tail libraries.
const sidebarMaxPerCategory = 25

// SidebarTagsWithGlobalCount returns the top N tags per category for the
// given image IDs. Tags are ranked by per-page count; UsageCount carries
// the global tags.usage_count so the sidebar badge reflects total
// occurrences across the library. A ROW_NUMBER() window caps each
// category server-side.
func SidebarTagsWithGlobalCount(database *db.DB, imageIDs []int64) ([]models.Tag, error) {
	if len(imageIDs) == 0 {
		return nil, nil
	}

	placeholders, args := db.InPlaceholders(imageIDs)

	return db.QueryAll(database.Read, tags.ScanTag,
		fmt.Sprintf(
			`WITH tag_counts AS (
			     SELECT t.id AS tag_id, t.name AS tag_name, tc.name AS cat_name,
			            tc.color AS cat_color, t.usage_count,
			            COUNT(DISTINCT it.image_id) AS page_count
			     FROM image_tags it INDEXED BY idx_image_tags_image
			     JOIN tags t ON t.id = it.tag_id
			     JOIN tag_categories tc ON tc.id = t.category_id
			     WHERE it.image_id IN (%s) AND t.is_alias = 0
			     GROUP BY t.id
			 )
			 SELECT tag_id, tag_name, cat_name, cat_color, usage_count
			 FROM (
			     SELECT tag_id, tag_name, cat_name, cat_color, usage_count, page_count,
			            ROW_NUMBER() OVER (PARTITION BY cat_name
			                               ORDER BY page_count DESC, tag_name ASC) AS rn
			     FROM tag_counts
			 )
			 WHERE rn <= ?
			 ORDER BY page_count DESC, tag_name ASC`,
			placeholders,
		),
		append(args, sidebarMaxPerCategory)...)
}

// suggestCandidateCap bounds how many prefix/substring-matching tags are
// considered before computing the per-tag combination count. On 100k+
// galleries a popular prefix like "re" can match thousands of tags;
// without a cap the executor joined image_tags ⋈ images for every match
// just to discard all but the top 10. Bounding by global usage_count is
// safe because COUNT(DISTINCT i.id) ≤ tags.usage_count, so a candidate
// outside the cap cannot outrank one inside it on combo count. The
// dropdown surfaces 10 results, so 2.5x headroom absorbs the prefix-then-
// substring de-dup pass without dragging tail candidates into the join.
// The per-candidate image_tags probe scales the with-context shape under
// concurrency; halving the cap halves the join work each request pays.
const suggestCandidateCap = 25

// suggestContextCap bounds the materialised context-image set. The combo
// count for hot context tags becomes a lower bound past the cap, but
// the relative ordering of suggestions is preserved because tied
// candidates fall through to global usage_count for tie-breaking. 1000
// is the working point: under c=5 the per-worker join through
// image_tags x context fits the page cache; bumping to 5000 amplifies
// the autocomplete latency 3.5x without changing the visible top 10.
const suggestContextCap = 1000

// SuggestTagsWithFilter returns up to limit tags matching prefix that
// also co-occur with at least one image matching expr. UsageCount on
// each returned tag carries the combination count (expr AND the
// suggested tag), not the global one. categoryName, when set, restricts
// suggestions to that category.
func SuggestTagsWithFilter(database *db.DB, expr Expr, prefix, categoryName string, limit int) ([]models.Tag, error) {
	prefix = tags.NormalizeTagName(prefix)
	// No preceding context: the combination count collapses to the tag's
	// global usage count, so skip the image_tags ⋈ images join entirely.
	if expr == nil {
		return tags.SuggestUsageRanked(database, prefix, categoryName, true, limit)
	}

	where, args, hasMissingFilter := buildWhereDB(expr, database)
	where = andDefaultVisible(where, hasMissingFilter)

	// Two-pass: prefix matches first (ranked by combo count), then
	// substring matches until limit is hit. Each pass first picks up to
	// suggestCandidateCap tags by global usage_count, then computes the
	// combination count only for that bounded set.
	prefixPat := db.EscapeLike(prefix) + "%"
	substrPat := "%" + db.EscapeLike(prefix) + "%"

	// ctx materialises the context-image set once via the same WHERE
	// clause Execute uses; each candidate then probes image_tags filtered
	// by `image_id IN ctx` instead of joining images and running an
	// EXISTS subquery per row. The image_tags PK makes (image_id, tag_id)
	// unique, so COUNT(it.image_id) within a group fixed at one tag_id
	// equals the original COUNT(DISTINCT i.id).
	//
	// suggestContextCap keeps the per-candidate join bounded; combo
	// counts become a lower bound past the cap.
	baseSQL := `WITH ctx AS (
	                SELECT i.id AS image_id FROM images i WHERE %s LIMIT ?
	            ),
	            cand AS (
	                SELECT id, category_id, usage_count
	                FROM tags
	                WHERE is_alias = 0
	                  AND name LIKE ? ESCAPE '\'
	                  %s
	                ORDER BY usage_count DESC
	                LIMIT ?
	            )
	            SELECT c.id, t.name, tc.name, tc.color, COUNT(it.image_id) AS combo
	            FROM cand c
	            JOIN tags t ON t.id = c.id
	            JOIN tag_categories tc ON tc.id = c.category_id
	            JOIN image_tags it ON it.tag_id = c.id
	                              AND it.image_id IN (SELECT image_id FROM ctx)
	            GROUP BY c.id
	            HAVING combo > 0
	            ORDER BY combo DESC, c.usage_count DESC
	            LIMIT ?`

	catClause := ""
	catArgs := []any{}
	if categoryName != "" {
		catClause = "AND category_id = (SELECT id FROM tag_categories WHERE name = ?)"
		catArgs = []any{categoryName}
	}

	run := func(pat string, prior []models.Tag, remaining int, nameNotLike string) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 5+len(args)+len(catArgs))
		qargs = append(qargs, args...)
		qargs = append(qargs, suggestContextCap)
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if nameNotLike != "" {
			extra = extra + ` AND name NOT LIKE ? ESCAPE '\'`
			qargs = append(qargs, nameNotLike)
		}
		qargs = append(qargs, suggestCandidateCap)
		qargs = append(qargs, remaining)
		rows, err := database.Read.Query(fmt.Sprintf(baseSQL, where, extra), qargs...)
		if err != nil {
			return prior, err
		}
		defer func() { _ = rows.Close() }()
		seen := map[int64]bool{}
		for _, t := range prior {
			seen[t.ID] = true
		}
		for rows.Next() {
			var t models.Tag
			var combo int
			if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &combo); err != nil {
				return prior, err
			}
			if seen[t.ID] {
				continue
			}
			t.UsageCount = combo
			prior = append(prior, t)
			seen[t.ID] = true
		}
		return prior, rows.Err()
	}

	out, err := run(prefixPat, nil, limit, "")
	if err != nil {
		return nil, err
	}
	if len(out) < limit {
		out, err = run(substrPat, out, limit-len(out), prefixPat)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// sqlDir renders the ORDER BY direction, falling back to def when the
// order parameter names neither.
func sqlDir(order, def string) string {
	switch order {
	case "asc":
		return "ASC"
	case "desc":
		return "DESC"
	}
	return def
}

// collectionOrderClause sorts a result set pinned to one collection by
// that collection's per-image position (NULLs last in both directions),
// then id. The position is read from the join table so an image filed
// under several collections sorts by the pinned one, not its home order.
func collectionOrderClause(name, order string) (string, []any) {
	dir := sqlDir(order, "ASC")
	sub := "(SELECT position FROM image_collections WHERE image_id = i.id AND name = ?)"
	clause := "ORDER BY " + sub + " IS NULL, " + sub + " " + dir + ", i.id " + dir
	return clause, []any{name, name}
}

// PinnedCollectionName returns the single collection: value asserted at the
// top level of expr, or "" when none or several are present. When exactly
// one is pinned the order sort reads that collection's own position.
func PinnedCollectionName(expr Expr) string {
	seen := map[string]struct{}{}
	last := ""
	var walk func(Expr)
	walk = func(e Expr) {
		switch v := e.(type) {
		case AndExpr:
			walk(v.Left)
			walk(v.Right)
		case FilterExpr:
			if v.Key == "collection" && v.Val != "" {
				seen[strings.ToLower(v.Val)] = struct{}{}
				last = v.Val
			}
		}
	}
	walk(expr)
	if len(seen) == 1 {
		return last
	}
	return ""
}

func buildOrder(sort, order string, randomSeed int64) string {
	switch sort {
	case "filesize":
		dir := sqlDir(order, "DESC")
		return "ORDER BY i.file_size " + dir + ", i.id " + dir
	case "order":
		// Group by series alphabetically, then by within-series position
		// with NULLs last in both directions (a series with most rows
		// unordered should still sit next to its ordered ones), then
		// fall back to ingest order so untagged rows have a stable seat
		// and pagination has a total order. ASC and DESC flip every
		// axis except the NULLs-last bias.
		dir := "ASC"
		if order == "desc" {
			dir = "DESC"
		}
		return "ORDER BY i.series " + dir + ", i.series_order IS NULL, i.series_order " + dir + ", i.id " + dir
	case "random":
		if randomSeed != 0 {
			// Deterministic pseudo-random order, stable across page
			// loads. random_key() applies a SplitMix64-style hash to
			// (id, seed) so consecutive ids end up at unrelated
			// positions even for small seeds. id remains the
			// tiebreaker for a total order so pagination doesn't
			// repeat or skip.
			// SAFETY: %d only produces digits; literal interpolation
			// of the seed is injection-safe.
			return fmt.Sprintf("ORDER BY random_key(i.id, %d), i.id", randomSeed)
		}
		return "ORDER BY RANDOM(), i.id"
	default: // "newest"
		dir := sqlDir(order, "DESC")
		return "ORDER BY i.ingested_at " + dir + ", i.id " + dir
	}
}
