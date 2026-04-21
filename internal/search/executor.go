package search

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

// Query holds a parsed query and pagination parameters.
type Query struct {
	Expr       Expr
	Sort       string // "newest" | "filesize" | "random"
	Order      string // "asc" | "desc"
	RandomSeed int64  // used when Sort=="random" for stable ordering
	Page       int    // 1-based
	Limit      int
	// PresetTotal skips the COUNT(*) query when set to a non-negative
	// value. Callers that already know the match count (e.g. an unfiltered
	// gallery page using a cached visible-image count) should set this to
	// avoid scanning the whole index on large libraries.
	PresetTotal *int
	// SkipCount drops the COUNT(*) query entirely and returns Total = 0.
	// For callers that only need Results (e.g. the sidebar handlers, which
	// use the returned IDs to build the per-page tag aggregation but never
	// surface Total). On filtered searches against 100k-image libraries the
	// count pass is a full filter evaluation; skipping it halves the work.
	SkipCount bool
}

// Execute runs the query against the DB and returns paginated results.
func Execute(database *db.DB, q Query) (*models.SearchResult, error) {
	where, args, hasMissingFilter := buildWhereDB(q.Expr, database)

	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	orderClause := buildOrder(q.Sort, q.Order, q.RandomSeed)

	page := q.Page
	if page < 1 {
		page = 1
	}
	limit := q.Limit
	if limit < 1 {
		limit = 40
	}
	offset := (page - 1) * limit

	var total int
	switch {
	case q.SkipCount:
		// total stays 0; caller opted out.
	case q.PresetTotal != nil:
		total = *q.PresetTotal
	default:
		countSQL := "SELECT COUNT(*) FROM images i WHERE " + where
		if err := database.Read.QueryRow(countSQL, args...).Scan(&total); err != nil {
			return nil, fmt.Errorf("count query: %w", err)
		}
	}

	dataSQL := fmt.Sprintf(
		`SELECT i.id, i.sha256, i.canonical_path, i.folder_path, i.file_type,
		        i.width, i.height, i.file_size, i.is_missing, i.is_favorited,
		        i.auto_tagged_at, i.source_type, i.origin, i.ingested_at
		 FROM images i
		 WHERE %s
		 %s
		 LIMIT ? OFFSET ?`,
		where, orderClause,
	)

	dataArgs := make([]any, len(args), len(args)+2)
	copy(dataArgs, args)
	dataArgs = append(dataArgs, limit, offset)
	rows, err := database.Read.Query(dataSQL, dataArgs...)
	if err != nil {
		return nil, fmt.Errorf("data query: %w", err)
	}
	defer rows.Close()

	var images []models.Image
	for rows.Next() {
		var img models.Image
		var isMissing, isFav int
		var width, height *int
		var autoTaggedAt *string
		var ingestedAt string

		if err := rows.Scan(
			&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType,
			&width, &height, &img.FileSize, &isMissing, &isFav,
			&autoTaggedAt, &img.SourceType, &img.Origin, &ingestedAt,
		); err != nil {
			return nil, err
		}
		img.IsMissing = isMissing == 1
		img.IsFavorited = isFav == 1
		img.Width = width
		img.Height = height
		if autoTaggedAt != nil {
			t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
			img.AutoTaggedAt = &t
		}
		img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: images,
	}, nil
}

// ExecuteAdjacent returns the image IDs immediately preceding and following
// currentID in sort order under the same filter as Execute. Uses cursor-style
// LIMIT 1 queries so the cost is O(log n) via the ingested_at / file_size
// indexes instead of loading every matching ID. Random sort uses the same
// deterministic seed expression as Execute, so a (currentID, seed) pair
// resolves to the same neighbours the gallery shows.
func ExecuteAdjacent(database *db.DB, q Query, currentID int64) (*int64, *int64, error) {
	var ingestedAt string
	var fileSize int64
	if err := database.Read.QueryRow(
		`SELECT ingested_at, file_size FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize); err != nil {
		return nil, nil, nil
	}

	where, args, hasMissingFilter := buildWhereDB(q.Expr, database)
	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	var keyCol string
	var keyVal any
	switch q.Sort {
	case "random":
		if q.RandomSeed == 0 {
			return nil, nil, nil
		}
		// SAFETY: q.RandomSeed is generated server-side via crypto/rand
		// (see galleryHandler) and never sourced from user input. %d only
		// produces digits regardless of value, so this is safe from SQL
		// injection - same pattern as buildOrder uses for the data query.
		keyCol = fmt.Sprintf("((i.id * %d) & 2147483647)", q.RandomSeed)
		keyVal = (currentID * q.RandomSeed) & 2147483647
	case "filesize":
		keyCol = "i.file_size"
		keyVal = fileSize
	default: // "newest"
		keyCol = "i.ingested_at"
		keyVal = ingestedAt
	}

	// For desc order the list runs largest→smallest; prev is the next-larger
	// neighbour, next is the next-smaller. For asc and random (which iterate
	// ascending by keyCol), mirrored.
	var prevCmp, nextCmp, prevSort, nextSort string
	if q.Order == "asc" || q.Sort == "random" {
		prevCmp = fmt.Sprintf("(%s < ? OR (%s = ? AND i.id < ?))", keyCol, keyCol)
		nextCmp = fmt.Sprintf("(%s > ? OR (%s = ? AND i.id > ?))", keyCol, keyCol)
		prevSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", keyCol)
		nextSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", keyCol)
	} else {
		prevCmp = fmt.Sprintf("(%s > ? OR (%s = ? AND i.id > ?))", keyCol, keyCol)
		nextCmp = fmt.Sprintf("(%s < ? OR (%s = ? AND i.id < ?))", keyCol, keyCol)
		prevSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", keyCol)
		nextSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", keyCol)
	}

	lookup := func(cursorCmp, sort string) *int64 {
		qargs := make([]any, 0, len(args)+3)
		qargs = append(qargs, args...)
		qargs = append(qargs, keyVal, keyVal, currentID)
		sql := fmt.Sprintf("SELECT i.id FROM images i WHERE %s AND %s %s LIMIT 1",
			where, cursorCmp, sort)
		var id int64
		if err := database.Read.QueryRow(sql, qargs...).Scan(&id); err != nil {
			return nil
		}
		return &id
	}
	return lookup(prevCmp, prevSort), lookup(nextCmp, nextSort), nil
}

// ExecutePosition returns the 1-based rank of currentID in the same sort
// order Execute would produce under q, and the total number of matching
// rows. The detail page uses it for the "X/Y" counter next to prev/next.
// A single combined COUNT exploits the same key-col indexes ExecuteAdjacent
// does instead of materialising the full match list.
func ExecutePosition(database *db.DB, q Query, currentID int64) (int, int, error) {
	var ingestedAt string
	var fileSize int64
	if err := database.Read.QueryRow(
		`SELECT ingested_at, file_size FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize); err != nil {
		return 0, 0, nil
	}

	where, args, hasMissingFilter := buildWhereDB(q.Expr, database)
	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	var keyCol string
	var keyVal any
	switch q.Sort {
	case "random":
		if q.RandomSeed == 0 {
			return 0, 0, nil
		}
		// SAFETY: q.RandomSeed is generated server-side via crypto/rand;
		// %d only emits digits. Same inlined-literal approach ExecuteAdjacent
		// and buildOrder use.
		keyCol = fmt.Sprintf("((i.id * %d) & 2147483647)", q.RandomSeed)
		keyVal = (currentID * q.RandomSeed) & 2147483647
	case "filesize":
		keyCol = "i.file_size"
		keyVal = fileSize
	default: // "newest"
		keyCol = "i.ingested_at"
		keyVal = ingestedAt
	}

	// prevCmp mirrors ExecuteAdjacent: it matches rows that come before
	// currentID in the active sort order, so SUM(CASE WHEN prevCmp …) gives
	// the zero-based rank. Position = rank + 1.
	var prevCmp string
	if q.Order == "asc" || q.Sort == "random" {
		prevCmp = fmt.Sprintf("(%s < ? OR (%s = ? AND i.id < ?))", keyCol, keyCol)
	} else {
		prevCmp = fmt.Sprintf("(%s > ? OR (%s = ? AND i.id > ?))", keyCol, keyCol)
	}

	stmt := fmt.Sprintf(
		`SELECT SUM(CASE WHEN %s THEN 1 ELSE 0 END), COUNT(*) FROM images i WHERE %s`,
		prevCmp, where,
	)
	qargs := make([]any, 0, len(args)+3)
	qargs = append(qargs, keyVal, keyVal, currentID)
	qargs = append(qargs, args...)

	var rankBefore sql.NullInt64
	var total int
	if err := database.Read.QueryRow(stmt, qargs...).Scan(&rankBefore, &total); err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, nil
	}
	return int(rankBefore.Int64) + 1, total, nil
}

// DeleteTarget holds minimal image data needed for bulk deletion.
type DeleteTarget struct {
	ID            int64
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
}

// ExecuteForDelete returns all images matching expr with their paths, for
// bulk deletion. No pagination is applied - all matching images are
// returned in a single query.
//
// Prefer ExecuteForDeleteStream when caller-side memory is a concern on
// large libraries; this variant remains for callers that genuinely need
// the whole slice up front.
func ExecuteForDelete(database *db.DB, expr Expr) ([]DeleteTarget, error) {
	var targets []DeleteTarget
	err := ExecuteForDeleteStream(database, expr, func(t DeleteTarget) error {
		targets = append(targets, t)
		return nil
	})
	return targets, err
}

// ExecuteForDeleteStream invokes visit for each matching row, streaming
// directly off the cursor so very large result sets never materialise in
// Go. visit returning a non-nil error aborts the iteration.
func ExecuteForDeleteStream(database *db.DB, expr Expr, visit func(DeleteTarget) error) error {
	where, args, hasMissingFilter := buildWhereDB(expr, database)
	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	rows, err := database.Read.Query(
		"SELECT i.id, i.canonical_path, i.folder_path, i.is_missing FROM images i WHERE "+where+" ORDER BY i.id",
		args...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

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

// sidebarMaxPerCategory is the per-category cap applied to the sidebar tag
// list. Kept small so the tree stays legible on long-tail tag collections.
const sidebarMaxPerCategory = 25

// SidebarTagsWithGlobalCount returns the top N tags per category for the
// given image IDs. Tags are ranked by how many images on the current page
// use them (page_count); UsageCount is set to the global tags.usage_count
// so the sidebar badge reflects total occurrences across the library.
//
// A ROW_NUMBER() window caps each category to sidebarMaxPerCategory rows
// server-side so wide-tagged libraries don't ship every association back
// just to slice most of them off in Go.
func SidebarTagsWithGlobalCount(database *db.DB, imageIDs []int64) ([]models.Tag, error) {
	if len(imageIDs) == 0 {
		return nil, nil
	}

	placeholders := strings.Repeat("?,", len(imageIDs))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, len(imageIDs))
	for i, id := range imageIDs {
		args[i] = id
	}

	rows, err := database.Read.Query(
		fmt.Sprintf(
			`WITH tag_counts AS (
			     SELECT t.id AS tag_id, t.name AS tag_name, tc.name AS cat_name,
			            tc.color AS cat_color, t.usage_count,
			            COUNT(DISTINCT it.image_id) AS page_count
			     FROM image_tags it
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
		append(args, sidebarMaxPerCategory)...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Tag
	for rows.Next() {
		var t models.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SuggestTagsWithFilter returns up to limit tags whose name starts with (or
// contains, as a secondary ranking) prefix AND which co-exist on at least one
// image matching the preceding search expression. UsageCount on each returned
// tag carries the count of images matching the full combination (existing
// expression ANDed with the suggested tag), not the global usage count.
//
// When categoryName is non-empty, suggestions are restricted to that category.
// When expr is nil, the filter degrades to "at least one image with this tag"
// (which is identical to the global usage count).
func SuggestTagsWithFilter(database *db.DB, expr Expr, prefix, categoryName string, limit int) ([]models.Tag, error) {
	// No preceding context → the combination count collapses to the tag's own
	// count (tags.usage_count already excludes missing images after recalc),
	// so skip the image_tags ⋈ images join entirely and rank by usage_count.
	// Typing a single word in an empty search bar is the common case and the
	// combo join is pure overhead there.
	if expr == nil {
		return suggestByUsage(database, prefix, categoryName, limit)
	}

	where, args, hasMissingFilter := buildWhereDB(expr, database)
	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	// Two-pass: prefix matches first (ranked by combo count), then substring
	// matches until we hit limit. Each pass passes its own copy of args.
	prefixPat := prefix + "%"
	substrPat := "%" + prefix + "%"

	baseSQL := `SELECT t.id, t.name, tc.name, tc.color, COUNT(DISTINCT i.id) AS combo
	            FROM tags t
	            JOIN tag_categories tc ON tc.id = t.category_id
	            JOIN image_tags it ON it.tag_id = t.id
	            JOIN images i ON i.id = it.image_id
	            WHERE t.is_alias = 0
	              AND t.name LIKE ?
	              %s
	              AND %s
	            GROUP BY t.id
	            HAVING combo > 0
	            ORDER BY combo DESC, t.usage_count DESC
	            LIMIT ?`

	catClause := ""
	catArgs := []any{}
	if categoryName != "" {
		catClause = "AND tc.name = ?"
		catArgs = []any{categoryName}
	}

	run := func(pat string, prior []models.Tag, remaining int, nameNotLike string) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 3+len(args)+len(catArgs))
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if nameNotLike != "" {
			extra = extra + " AND t.name NOT LIKE ?"
			qargs = append(qargs, nameNotLike)
		}
		qargs = append(qargs, args...)
		qargs = append(qargs, remaining)
		rows, err := database.Read.Query(fmt.Sprintf(baseSQL, extra, where), qargs...)
		if err != nil {
			return prior, err
		}
		defer rows.Close()
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
		// Reuse original arg slice; run copies it internally.
		out, err = run(substrPat, out, limit-len(out), prefixPat)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// suggestByUsage is the fast path of SuggestTagsWithFilter when no preceding
// search expression is present. Same prefix-then-substring two-pass shape,
// but the per-tag image count is read from tags.usage_count instead of being
// recomputed by joining image_tags and images - the dominant cost on large
// libraries where every candidate tag otherwise drags an image_tags scan.
func suggestByUsage(database *db.DB, prefix, categoryName string, limit int) ([]models.Tag, error) {
	baseSQL := `SELECT t.id, t.name, tc.name, tc.color, t.usage_count
	            FROM tags t
	            JOIN tag_categories tc ON tc.id = t.category_id
	            WHERE t.is_alias = 0
	              AND t.usage_count > 0
	              AND t.name LIKE ?
	              %s
	            ORDER BY t.usage_count DESC, t.name ASC
	            LIMIT ?`

	catClause := ""
	var catArgs []any
	if categoryName != "" {
		catClause = "AND tc.name = ?"
		catArgs = []any{categoryName}
	}

	run := func(pat string, prior []models.Tag, remaining int, nameNotLike string) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 2+len(catArgs))
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if nameNotLike != "" {
			extra = extra + " AND t.name NOT LIKE ?"
			qargs = append(qargs, nameNotLike)
		}
		qargs = append(qargs, remaining)
		rows, err := database.Read.Query(fmt.Sprintf(baseSQL, extra), qargs...)
		if err != nil {
			return prior, err
		}
		defer rows.Close()
		seen := map[int64]bool{}
		for _, t := range prior {
			seen[t.ID] = true
		}
		for rows.Next() {
			var t models.Tag
			if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
				return prior, err
			}
			if seen[t.ID] {
				continue
			}
			prior = append(prior, t)
			seen[t.ID] = true
		}
		return prior, rows.Err()
	}

	out, err := run(prefix+"%", nil, limit, "")
	if err != nil {
		return nil, err
	}
	if len(out) < limit {
		out, err = run("%"+prefix+"%", out, limit-len(out), prefix+"%")
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func buildOrder(sort, order string, randomSeed int64) string {
	switch sort {
	case "filesize":
		dir := "DESC"
		if order == "asc" {
			dir = "ASC"
		}
		return "ORDER BY i.file_size " + dir + ", i.id " + dir
	case "random":
		if randomSeed != 0 {
			// Deterministic pseudo-random order using seed: stable across page loads.
			// The masked product is not injective (two rows can share the same 31-bit
			// key), so we append i.id as a tiebreaker to guarantee a total order -
			// otherwise pagination can repeat or skip images.
			// SAFETY: randomSeed is an int64 generated server-side in galleryHandler
			// (via crypto/rand) and never sourced from user input. Interpolating it as
			// an integer literal is safe from SQL injection - fmt.Sprintf with %d only
			// produces digit characters regardless of the value.
			return fmt.Sprintf("ORDER BY ((i.id * %d) & 2147483647), i.id", randomSeed)
		}
		return "ORDER BY RANDOM(), i.id"
	default: // "newest"
		dir := "DESC"
		if order == "asc" {
			dir = "ASC"
		}
		return "ORDER BY i.ingested_at " + dir + ", i.id " + dir
	}
}

type whereBuilder struct {
	parts            []string
	args             []any
	hasMissingFilter bool
	// db is optional: when set, FilterExpr's default branch checks whether
	// an unknown `prefix:value` key matches a real tag category. On miss
	// the token falls back to a literal tag match so names like
	// `nier:automata` remain searchable by their raw form. A nil db keeps
	// the historic "always category-qualified" behaviour used in tests.
	db *db.DB
}

func buildWhere(expr Expr) (string, []any, bool) {
	return buildWhereDB(expr, nil)
}

// categoryExists reports whether the given name matches a tag_categories
// row. Returns false when the builder has no DB handle (tests), which
// preserves the old "always treat unknown key as category-qualified"
// default for the pure-parser test path.
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
	b := &whereBuilder{db: database}
	if expr != nil {
		part := b.buildExpr(expr)
		if part != "" {
			b.parts = append(b.parts, part)
		}
	}
	where := strings.Join(b.parts, " AND ")
	if where == "" {
		where = "1=1"
	}
	return where, b.args, b.hasMissingFilter
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
	switch e.Wildcard {
	case "prefix":
		b.args = append(b.args, e.Tag+"%")
		return `EXISTS (SELECT 1 FROM image_tags it JOIN tags t ON it.tag_id = t.id WHERE it.image_id = i.id AND t.name LIKE ?)`
	case "substring":
		b.args = append(b.args, "%"+e.Tag+"%")
		return `EXISTS (SELECT 1 FROM image_tags it JOIN tags t ON it.tag_id = t.id WHERE it.image_id = i.id AND t.name LIKE ?)`
	default:
		b.args = append(b.args, e.Tag)
		return `EXISTS (SELECT 1 FROM image_tags it JOIN tags t ON it.tag_id = t.id WHERE it.image_id = i.id AND t.name = ?)`
	}
}

func (b *whereBuilder) buildFilterExpr(e FilterExpr) string {
	switch e.Key {
	case "fav":
		if e.Val == "true" {
			return "i.is_favorited = 1"
		}
		return "i.is_favorited = 0"

	case "source":
		// Support comma-separated source_type (e.g. "a1111,comfyui") and legacy "sd" alias.
		val := e.Val
		if val == "sd" {
			val = "a1111"
		}
		// "ai" is a parent that matches any image with a1111 and/or comfyui metadata.
		if val == "ai" {
			return "(i.source_type = 'a1111' OR i.source_type = 'comfyui' OR i.source_type = 'a1111,comfyui')"
		}
		b.args = append(b.args, val, "%,"+val, val+",%", "%,"+val+",%")
		return "(i.source_type = ? OR i.source_type LIKE ? OR i.source_type LIKE ? OR i.source_type LIKE ?)"

	case "cat":
		b.args = append(b.args, e.Val)
		return `EXISTS (SELECT 1 FROM image_tags it JOIN tags t ON it.tag_id = t.id JOIN tag_categories tc ON tc.id = t.category_id WHERE it.image_id = i.id AND tc.name = ?)`

	case "width":
		op, n, ok := parseIntComp(e.Val)
		if !ok {
			return "1=0"
		}
		b.args = append(b.args, n)
		return fmt.Sprintf("i.width %s ?", op)

	case "height":
		op, n, ok := parseIntComp(e.Val)
		if !ok {
			return "1=0"
		}
		b.args = append(b.args, n)
		return fmt.Sprintf("i.height %s ?", op)

	case "date":
		return b.buildDateFilter(e.Val)

	case "missing":
		// Any explicit `missing:` filter - true or false - opts out of
		// the auto-injected `AND is_missing = 0`. Without this flag the
		// negated form `-missing:false` collapses to
		// `NOT (is_missing = 0) AND is_missing = 0`, which matches
		// nothing; the user typed it expecting "show me missing" and
		// got an empty grid instead.
		b.hasMissingFilter = true
		if e.Val == "true" {
			return "i.is_missing = 1"
		}
		return "i.is_missing = 0"

	case "animated":
		if e.Val == "true" {
			return "i.file_type IN ('gif', 'mp4', 'webm')"
		}
		return "i.file_type NOT IN ('gif', 'mp4', 'webm')"

	case "tagged":
		if e.Val == "true" {
			return "EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id)"
		}
		return "NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id)"

	case "autotagged":
		if e.Val == "true" {
			return "EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.is_auto = 1)"
		}
		return "NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.is_auto = 1)"

	case "folder":
		if e.Val == "" {
			// `folder:` alone is the recursive root: every non-missing image
			// lives at or below the gallery root, so the filter is a no-op.
			// Use `folderonly:` with an empty value for "root directly".
			return "1=1"
		}
		// Recursive: images in this folder or anywhere beneath it. Matches
		// how clicking a folder in the sidebar is expected to surface the
		// full subtree rather than just the directly-contained images.
		b.args = append(b.args, e.Val, e.Val+"/%")
		return "(i.folder_path = ? OR i.folder_path LIKE ?)"

	case "folderonly":
		if e.Val == "" {
			// Gallery root directly, excluding every subfolder.
			return "i.folder_path = ''"
		}
		b.args = append(b.args, e.Val)
		return "i.folder_path = ?"

	case "generated":
		// Match images whose A1111 or ComfyUI recipe hashes to e.Val.
		b.args = append(b.args, e.Val, e.Val)
		return `(EXISTS (SELECT 1 FROM sd_metadata sm WHERE sm.image_id = i.id AND sm.generation_hash = ?)
		         OR EXISTS (SELECT 1 FROM comfyui_metadata cm WHERE cm.image_id = i.id AND cm.generation_hash = ?))`

	default:
		// Unknown key is either a category-qualified tag search
		// (e.g. "character:cat") or a literal tag name that happens to
		// contain a colon (e.g. "nier:automata", ":3"). Resolution is by
		// existence: if the key matches a real tag category the token
		// is split; otherwise the whole `key:val` is matched as a tag
		// name so colon-bearing tags stay searchable by their raw form.
		if e.Val == "" {
			return "1=1"
		}
		if b.categoryExists(e.Key) {
			b.args = append(b.args, e.Val, e.Key)
			return `EXISTS (SELECT 1 FROM image_tags it
			         JOIN tags t ON t.id = it.tag_id
			         JOIN tag_categories tc ON tc.id = t.category_id
			         WHERE it.image_id = i.id AND t.name = ? AND tc.name = ?)`
		}
		b.args = append(b.args, e.Key+":"+e.Val)
		return `EXISTS (SELECT 1 FROM image_tags it
		         JOIN tags t ON t.id = it.tag_id
		         WHERE it.image_id = i.id AND t.name = ?)`
	}
}

func (b *whereBuilder) buildDateFilter(val string) string {
	if strings.HasPrefix(val, ">") {
		date := val[1:]
		b.args = append(b.args, date)
		return "i.ingested_at > ?"
	}
	if strings.HasPrefix(val, "<") {
		date := val[1:]
		b.args = append(b.args, date)
		return "i.ingested_at < ?"
	}
	if idx := strings.Index(val, ".."); idx >= 0 {
		from := val[:idx]
		to := val[idx+2:]
		b.args = append(b.args, from, to)
		return "i.ingested_at BETWEEN ? AND ?"
	}
	b.args = append(b.args, val, val+"T23:59:59Z")
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
// values like `width:>=abc` no longer collapse to `width >= 0` (SQLite
// silently coerces a string operand to 0). Returns ok=false when the
// value isn't a base-10 integer; callers should emit `1=0` so the user
// sees an explicit empty result instead of a silently overwide match.
func parseIntComp(val string) (string, int64, bool) {
	op, raw := parseCompOp(val)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return op, 0, false
	}
	return op, n, true
}
