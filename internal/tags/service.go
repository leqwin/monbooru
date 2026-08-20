package tags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/searchkw"
)

var (
	ErrInvalidTagName       = errors.New("invalid tag name")
	ErrTagNotFound          = errors.New("tag not found")
	ErrCategoryNotFound     = errors.New("category not found")
	ErrBuiltinCategory      = errors.New("cannot delete built-in category")
	ErrBuiltinCategoryName  = errors.New("cannot rename a built-in category")
	ErrReservedCategoryName = errors.New("this name is used by a search filter (e.g. " + reservedCategoryHint() + ")")
	ErrNonCanonicalRating   = errors.New("rating category accepts only general, sensitive, questionable, explicit")
	ErrRatingTagImmutable   = errors.New("rating category tags cannot be renamed, moved, or turned into aliases")
	ErrInvalidMoveTarget    = errors.New("tags must move to another existing category")

	// #rgb or #rrggbb. Anything else gets ZgotmplZ'd in the template's
	// CSS context, so reject it up front with a useful error.
	categoryColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

	ErrInvalidCategoryColor = errors.New("invalid category color (must be #rgb or #rrggbb)")

	// Category names round-trip through HTML form-field name attributes
	// (per-tagger threshold inputs), URL query values (search syntax
	// `cat:`), and template context. The allowlist mirrors the gallery-
	// name shape so a user-typed category can't smuggle quotes, slashes,
	// shell control characters, or whitespace through any of those
	// surfaces. The colon is excluded because it doubles as the
	// `category:tag` separator the search parser keys on.
	categoryNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

	ErrInvalidCategoryName = errors.New("invalid category name (use lowercase letters, digits, underscore, or hyphen)")

	// ErrCategoryExists is surfaced when a create/rename collides with an
	// existing row. Names are stored case-folded so "MOOD" and "mood"
	// register as the same duplicate.
	ErrCategoryExists = errors.New("a category with this name already exists")
)

// IsValidCategoryColor reports whether s matches the #rgb / #rrggbb
// shape the UI form enforces.
func IsValidCategoryColor(s string) bool { return categoryColorRe.MatchString(s) }

// normalizeCategoryColor folds a valid colour to the lowercase six-digit
// form a theme's --cat-<rrggbb> variable is keyed on, so a colour typed
// by hand still matches the mapping. Anything else comes back untouched
// for the caller's validation to refuse.
func normalizeCategoryColor(s string) string {
	if !IsValidCategoryColor(s) {
		return s
	}
	s = strings.ToLower(s)
	if len(s) == 4 {
		return string([]byte{'#', s[1], s[1], s[2], s[2], s[3], s[3]})
	}
	return s
}

// SafeCategoryColor returns s when it's a valid hex colour, otherwise
// the neutral fallback ("#888888"). Used on rows arriving from outside
// the UI form layer (JSON / DB imports) so foreign payloads never
// reach the inline-style template context unchecked.
func SafeCategoryColor(s string) string {
	if IsValidCategoryColor(s) {
		return s
	}
	return "#888888"
}

var (
	// reservedCategoryList is the source of truth for category names
	// refused at create/rename time: every search-filter keyword (which
	// would collide with `category:tag` parsing) plus "system" (the
	// search-bar cheat-sheet trigger; a real category by that name would
	// hijack `system:foo` into a category-qualified search). The slice
	// drives both reservedCategoryNames and ErrReservedCategoryName so
	// adding a future filter is a single edit in internal/searchkw.
	reservedCategoryList = append(append([]string{}, searchkw.Keywords...), "system")

	// RatingLevels is the canonical rating vocabulary, ordered low to high.
	// Highest-wins resolution and the cookie ceiling both rely on this order.
	RatingLevels = []string{"general", "sensitive", "questionable", "explicit"}
)

// IsCanonicalRating reports whether name is one of the four allowed
// rating tag names. The rating category refuses any other name.
func IsCanonicalRating(name string) bool {
	return RatingRank(name) >= 0
}

func isReservedCategoryName(name string) bool {
	return slices.Contains(reservedCategoryList, name)
}

// reservedCategoryHint formats reservedCategoryList as a human-readable
// "fav:, source:, cat:, ..." list for the inline error message. Computed
// at package init so the error string stays a single sentinel value.
func reservedCategoryHint() string {
	parts := make([]string, len(reservedCategoryList))
	for i, n := range reservedCategoryList {
		parts[i] = n + ":"
	}
	return strings.Join(parts, ", ")
}

// TagFilter controls listing behavior.
type TagFilter struct {
	CategoryID *int64
	Prefix     string
	Sort       string // "name" | "usage" | "created" | "last_used"
	Order      string // "asc" | "desc" - flips the primary sort direction
	// PageIndex is 0-based - callers supply the requested page number minus
	// one. ListTags multiplies by Limit to derive the SQL OFFSET.
	PageIndex int
	Limit     int
	// Origin matches the stored creation label (tags.origin). The legacy
	// "alias" spelling still resolves as a structural narrowing so pre-Type
	// URLs and API callers keep working; alias-ness itself lives in Type.
	Origin string
	// Type narrows structurally: "alias" = alias rows only, "tag" =
	// non-alias rows only, "" = both.
	Type string
	// UsedBy keeps tags this source applied to at least one image, per the
	// image_tag_sources ledger. Independent of Origin: a source that only
	// re-confirmed a tag someone else created still matches.
	UsedBy string
	// CreatedAfter keeps rows whose created_at is >= it (ISO 8601).
	CreatedAfter string
	// ConflictsOnly narrows to non-alias tags whose name occupies more
	// than one category. ListTags forces name order so the colliding
	// rows sit adjacent.
	ConflictsOnly bool
	// ShowZero opts in to surfacing non-alias tags whose usage_count is 0.
	// Default behaviour hides them so the listing reflects what is actually
	// applied to images; alias rows always render regardless because their
	// usage_count is 0 by construction.
	ShowZero bool
	// ZeroOnly narrows the listing to non-alias zero-usage tags only.
	// Implies ShowZero. Used by the /tags Zero-usage Only filter to find
	// declared-but-unused tags for triage.
	ZeroOnly bool
	// Stale narrows to tags with source-dropped usage: "has" = at least one
	// stale image_tags row, "full" = every usage stale (the safe-to-remove
	// set). Empty means no stale filter.
	Stale string
	// FoldedOnly narrows to the folded originals recorded by the last
	// folded-duplicate scan (folded_tag_pairs.old_id).
	FoldedOnly bool
}

// Service provides tag and category CRUD with usage_count and co-occurrence maintenance.
type Service struct {
	db *db.DB
	// ratingCatID is the resolved id of the built-in `rating` category,
	// cached at New time so the GetOrCreateTag guard and the
	// rename/merge/delete/move-category refusals don't pay a SELECT per
	// call. The category row is built-in and never deleted, so this
	// never becomes stale; the four canonical rating tag IDs are
	// resolved per-call (they can be pruned on zero-usage and re-created
	// via GetOrCreateTag, so a long-lived cache would drift).
	ratingCatID int64
}

// New creates a new Service.
func New(database *db.DB) *Service {
	s := &Service{db: database}
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'rating'`,
	).Scan(&s.ratingCatID); err != nil {
		// db.Bootstrap seeds the rating category before this runs, so a
		// miss here means the DB is in a partially-migrated state. Log
		// it instead of silently dropping the error - the rating guards
		// that read s.ratingCatID will short-circuit with the bare 0
		// and the operator gets a hint as to why.
		logx.Warnf("tags.New: rating category lookup failed: %v", err)
	}
	return s
}

// RatingCategoryID returns the cached id of the rating category, or 0
// when the category is missing (only possible on a pre-bootstrap DB).
func (s *Service) RatingCategoryID() int64 { return s.ratingCatID }

func (s *Service) inWriteTx(work func(*sql.Tx) error) error {
	return db.InWriteTx(s.db.Write, work)
}

// RecalcDB recomputes usage_count from image_tags (non-missing images
// only). Call after bulk deletes, imports, or sync. Tag rows are kept
// even at zero usage so user-declared aliases and implications survive
// against an empty library.
func RecalcDB(database *db.DB) {
	if _, err := RecalcDBCount(database); err != nil {
		logx.Warnf("RecalcDB: %v", err)
	}
}

// RecalcDBCount is RecalcDB with the count of rows whose usage_count
// changed.
//
// A naive correlated-subquery UPDATE recomputes the count twice per
// tag and dominates sync time on tag-heavy libraries. This impl zeros
// out tags whose remaining usages all point at missing images, then
// fills in the rest with one GROUP BY pass over image_tags, chunked
// by tag_id range so the single writer is released between chunks.
// Returns the first per-chunk SQL error encountered alongside the
// (partial) updated count so callers can surface the failure; per-
// chunk errors are also logged at WARN before the early return.
func RecalcDBCount(database *db.DB) (int64, error) {
	const chunkSize = 2000

	var updated int64
	var maxID int64
	if err := database.Read.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM tags`).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("max tag id: %w", err)
	}

	for start := int64(0); start <= maxID; start += chunkSize {
		end := start + chunkSize
		res, err := database.Write.Exec(`
			UPDATE tags SET usage_count = 0
			WHERE usage_count != 0
			  AND id >= ? AND id < ?
			  AND NOT EXISTS (
			      SELECT 1 FROM image_tags it
			      JOIN images i ON i.id = it.image_id
			      WHERE it.tag_id = tags.id AND i.is_missing = 0
			  )
		`, start, end)
		if err != nil {
			logx.Warnf("RecalcDBCount zero-out chunk [%d, %d): %v", start, end, err)
			return updated, fmt.Errorf("zero-out chunk [%d, %d): %w", start, end, err)
		}
		n, _ := res.RowsAffected()
		updated += n

		res, err = database.Write.Exec(`
			UPDATE tags SET usage_count = c.cnt
			FROM (
			    SELECT it.tag_id, COUNT(*) AS cnt FROM image_tags it
			    JOIN images i ON i.id = it.image_id
			    WHERE i.is_missing = 0 AND it.tag_id >= ? AND it.tag_id < ?
			    GROUP BY it.tag_id
			) c
			WHERE c.tag_id = tags.id AND tags.usage_count != c.cnt
		`, start, end)
		if err != nil {
			logx.Warnf("RecalcDBCount fill chunk [%d, %d): %v", start, end, err)
			return updated, fmt.Errorf("fill chunk [%d, %d): %w", start, end, err)
		}
		n, _ = res.RowsAffected()
		updated += n
	}
	return updated, nil
}

func (s *Service) RecalcCount() (int64, error) {
	return RecalcDBCount(s.db)
}

// ChunkedDeleteWithTagRecalc walks ids in 500-row write transactions.
// Per chunk it (1) collects the distinct tag_ids the about-to-delete
// rows would touch (`SELECT DISTINCT tag_id FROM image_tags WHERE
// image_id IN (?…)` + extraSQL), (2) calls deleteFn(tx, chunk,
// placeholders, args) for the caller's actual DELETE, (3) commits, (4)
// runs afterCommit(chunk) outside the tx for filesystem cleanup or
// progress reporting.
//
// ctx aborts at a chunk boundary; cancelled is true and processed
// reflects partial progress so the caller's summary stays accurate.
// extraSQL is appended after the IN-list for both the tag-id SELECT
// and the caller's deleteFn args (caller decides whether to embed it
// in their DELETE), with extraArgs spliced in after the chunk ids.
//
// affected is the union of touched tag_ids; the caller passes it to
// RecalcIDs once after the loop so RecalcIDs runs on the touched set
// instead of walking the whole tags table.
func (s *Service) ChunkedDeleteWithTagRecalc(
	ctx context.Context,
	ids []int64,
	extraSQL string,
	extraArgs []any,
	deleteFn func(tx *sql.Tx, chunk []int64, placeholders string, args []any) error,
	afterCommit func(chunk []int64),
) (affected []int64, processed int, cancelled bool, err error) {
	const chunkSize = 500
	seen := map[int64]struct{}{}
	for start := 0; start < len(ids); start += chunkSize {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		chunk := ids[start:min(start+chunkSize, len(ids))]
		placeholders, chunkArgs := db.InPlaceholders(chunk)
		args := append(chunkArgs, extraArgs...)

		tx, err := s.db.Write.Begin()
		if err != nil {
			return tagIDsFromSet(seen), processed, false, err
		}
		// A short set would leave the tags it missed carrying a usage_count
		// the delete is about to invalidate, with nothing to say so, which
		// is why a read failure aborts the chunk rather than shortening it.
		touched, err := db.QueryIDs(tx,
			`SELECT DISTINCT tag_id FROM image_tags WHERE image_id IN (`+placeholders+`)`+extraSQL,
			args...)
		if err != nil {
			_ = tx.Rollback()
			return tagIDsFromSet(seen), processed, false, err
		}
		for _, tid := range touched {
			seen[tid] = struct{}{}
		}
		if err := deleteFn(tx, chunk, placeholders, args); err != nil {
			_ = tx.Rollback()
			return tagIDsFromSet(seen), processed, false, err
		}
		if err := tx.Commit(); err != nil {
			return tagIDsFromSet(seen), processed, false, err
		}
		if afterCommit != nil {
			afterCommit(chunk)
		}
		processed += len(chunk)
	}
	return tagIDsFromSet(seen), processed, cancelled, nil
}

func tagIDsFromSet(seen map[int64]struct{}) []int64 {
	if len(seen) == 0 {
		return nil
	}
	return slices.Collect(maps.Keys(seen))
}

// RecalcIDs recomputes usage_count for the given tag IDs. Lets bulk
// callers scope the work to tags they actually touched instead of
// walking the whole table. IDs are processed in chunks to stay under
// the SQLite parameter limit.
func (s *Service) RecalcIDs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return db.Chunked(ids, 500, func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		if _, err := s.db.Write.Exec(`UPDATE tags SET usage_count = (
			SELECT COUNT(*) FROM image_tags it
			JOIN images i ON i.id = it.image_id
			WHERE it.tag_id = tags.id AND i.is_missing = 0
		) WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("recalc usage_count chunk: %w", err)
		}
		return nil
	})
}
