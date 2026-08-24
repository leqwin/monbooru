package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// Tag rows themselves: name validation, get-or-create, listing and
// filtering, alias lookups, per-image reads, and deletion.

// tagDecorationClass is the separator/decoration set. A name built only from
// these is rejected as content-free ("---"); an emoticon like ">_<" passes
// because it also carries runes outside the set.
const tagDecorationClass = "_()!@#$.~+:-"

// buildTagName is the shared tag-name normalizer: lowercase, drop
// control/format/private-use runes, trim surrounding whitespace, and fold each
// internal whitespace run to a single `_`. Existing underscores are left alone.
// When foldReserved is set the grammar-reserved `"` and `*` fold like
// whitespace; the strict validator leaves them in place so it can reject them.
func buildTagName(name string, foldReserved bool) string {
	name = strings.ToLower(name)
	var b strings.Builder
	b.Grow(len(name))
	pendingFold := false
	for _, r := range name {
		switch {
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co):
		case unicode.IsSpace(r) || (foldReserved && (r == '"' || r == '*')):
			pendingFold = true
		default:
			if pendingFold && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingFold = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NormalizeTagName applies the shared normalization without folding the
// reserved characters. The add path (via ValidateTagName), the search parser,
// the filter-value resolvers, and the tag autocomplete all run input through it
// so a typed query matches a stored name.
func NormalizeTagName(name string) string { return buildTagName(name, false) }

// HasTagContent reports whether name carries a rune outside the decoration
// class, so pure separators are rejected while emoticons pass.
func HasTagContent(name string) bool {
	return strings.ContainsFunc(name, func(r rune) bool {
		return !strings.ContainsRune(tagDecorationClass, r)
	})
}

// ValidateTagName normalizes name and checks the tag-name rules: 1-200 runes,
// none of them the reserved `"` or `*`, and at least one content rune. Returns
// the normalized name or an ErrInvalidTagName-wrapped error. Exposed so non-UI
// sources (the auto-tagger label loader, the JSON import path) apply the same
// rules.
func ValidateTagName(name string) (string, error) {
	name = NormalizeTagName(name)

	if n := utf8.RuneCountInString(name); n == 0 || n > 200 {
		return "", fmt.Errorf("%w: length must be 1-200 characters", ErrInvalidTagName)
	}
	if strings.ContainsAny(name, `"*`) {
		return "", fmt.Errorf(`%w: contains invalid characters (allowed: any character except whitespace, " and *)`, ErrInvalidTagName)
	}
	if !HasTagContent(name) {
		return "", fmt.Errorf("%w: name must contain at least one letter, digit, or emoticon character", ErrInvalidTagName)
	}
	return name, nil
}

// NormalizeName folds an externally-sourced tag name into the stored form:
// normalized like ValidateTagName but the reserved `"` / `*` fold to `_` rather
// than being rejected, and leftover end underscores are trimmed. Import tokens
// pass through it so a hydrus tag like `hatsune miku` is stored as
// `hatsune_miku` instead of being rejected. Returns "" when nothing usable
// remains.
func NormalizeName(name string) string {
	return strings.Trim(buildTagName(name, true), "_")
}

func (s *Service) GetOrCreateTag(name string, categoryID int64) (*models.Tag, error) {
	return s.GetOrCreateTagFrom(name, categoryID, "user")
}

// GetOrCreateTagFrom is GetOrCreateTag with an explicit creation origin
// (a booru site, "ptr", an import label). The origin is stamped only on
// the insert; an existing row keeps its creator.
func (s *Service) GetOrCreateTagFrom(name string, categoryID int64, origin string) (*models.Tag, error) {
	normalized, err := ValidateTagName(name)
	if err != nil {
		return nil, err
	}
	if s.ratingCatID != 0 && categoryID == s.ratingCatID && !IsCanonicalRating(normalized) {
		return nil, ErrNonCanonicalRating
	}

	var tag *models.Tag
	err = s.inWriteTx(func(tx *sql.Tx) error {
		var txErr error
		tag, txErr = getOrCreateTagTx(tx, normalized, categoryID, origin)
		return txErr
	})
	return tag, err
}

func getOrCreateTagTx(tx *sql.Tx, name string, categoryID int64, origin string) (*models.Tag, error) {
	var tag models.Tag
	var createdAt string
	var canonicalID sql.NullInt64
	// Look up by (name, category_id) so the same name can live in
	// multiple categories.
	err := tx.QueryRow(
		`SELECT id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at FROM tags WHERE name = ? AND category_id = ?`,
		name, categoryID,
	).Scan(&tag.ID, &tag.Name, &tag.CategoryID, &tag.UsageCount, &tag.IsAlias, &canonicalID, &createdAt)

	if err == sql.ErrNoRows {
		var id int64
		if err := tx.QueryRow(
			`INSERT INTO tags (name, category_id, origin) VALUES (?, ?, ?) RETURNING id`,
			name, categoryID, origin,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("inserting tag: %w", err)
		}
		tag = models.Tag{
			ID:         id,
			Name:       name,
			CategoryID: categoryID,
			Origin:     origin,
			CreatedAt:  time.Now().UTC(),
		}
		return &tag, nil
	}
	if err != nil {
		return nil, err
	}

	// If this row is an alias, redirect to its canonical. MergeTags
	// refuses to point an alias at another alias, so one hop is enough.
	// A failed lookup (notably a dangling canonical_tag_id) must not
	// fall through to the alias row itself - callers would key
	// image_tags onto an alias id, which the resolver treats
	// inconsistently.
	if tag.IsAlias && canonicalID.Valid {
		var canon models.Tag
		var canonCreated string
		if err := tx.QueryRow(
			`SELECT id, name, category_id, usage_count, is_alias, created_at FROM tags WHERE id = ?`,
			canonicalID.Int64,
		).Scan(&canon.ID, &canon.Name, &canon.CategoryID, &canon.UsageCount, &canon.IsAlias, &canonCreated); err != nil {
			return nil, fmt.Errorf("resolving canonical %d for alias %q: %w", canonicalID.Int64, tag.Name, err)
		}
		canon.CreatedAt, _ = time.Parse(time.RFC3339, canonCreated)
		return &canon, nil
	}

	tag.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &tag, nil
}

// tagFilterWhere builds the WHERE clause and bound args shared by
// ListTags and ListTagIDs so both views see exactly the same set.
func tagFilterWhere(filter TagFilter) (string, []any) {
	args := []any{}
	where := "1=1"

	if filter.CategoryID != nil {
		where += " AND t.category_id = ?"
		args = append(args, *filter.CategoryID)
	}
	if filter.Prefix != "" {
		pat := db.EscapeLike(filter.Prefix)
		if strings.Contains(filter.Prefix, "*") {
			pat = strings.ReplaceAll(pat, "*", "%")
		} else {
			pat += "%"
		}
		where += " AND t.name LIKE ? ESCAPE '\\'"
		args = append(args, pat)
	}
	switch filter.Origin {
	case "":
	case "alias":
		// Legacy spelling kept for pre-Type URLs and API callers;
		// alias-ness is structure, not provenance.
		where += " AND t.is_alias = 1"
	default:
		where += " AND t.origin = ?"
		args = append(args, filter.Origin)
	}
	switch filter.Type {
	case "alias":
		where += " AND t.is_alias = 1"
	case "tag":
		where += " AND t.is_alias = 0"
	}
	if filter.UsedBy != "" {
		// EXISTS stops at the first ledger row; the IN-subquery form
		// materialises every row the source ever wrote instead.
		where += ` AND EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.source = ? AND s.tag_id = t.id)`
		args = append(args, filter.UsedBy)
	}
	if filter.CreatedAfter != "" {
		where += " AND t.created_at >= ?"
		args = append(args, filter.CreatedAfter)
	}
	if filter.ConflictsOnly {
		// Same row-counting shape as ConflictsCount so badge and listing agree.
		where += ` AND t.is_alias = 0 AND t.name IN (
			SELECT name FROM tags WHERE is_alias = 0
			GROUP BY name HAVING COUNT(*) >= 2)`
	}
	// The IN over the small stale slice bounds the scan to tags that have any
	// stale row; the "full" count subquery then runs only for those. Implied
	// rows never go stale, so a tag with live implied usage never reads as
	// full - the conservative direction for a delete candidate.
	switch filter.Stale {
	case "has":
		where += ` AND t.id IN (SELECT tag_id FROM image_tags WHERE stale = 1)`
	case "full":
		// usage_count is maintained over visible images only, so the stale
		// count has to skip missing ones or the two sides never agree.
		where += ` AND t.usage_count > 0 AND t.id IN (SELECT tag_id FROM image_tags WHERE stale = 1)
			AND (SELECT COUNT(*) FROM image_tags it JOIN images mi ON mi.id = it.image_id AND mi.is_missing = 0
			     WHERE it.tag_id = t.id AND it.stale = 1) = t.usage_count`
	}
	if filter.FoldedOnly {
		where += ` AND t.id IN (SELECT old_id FROM folded_tag_pairs)`
	}
	switch {
	case filter.ZeroOnly:
		// Strictly zero-usage non-alias rows. Aliases are excluded because
		// their usage_count is 0 by construction and would otherwise drown
		// the actual triage targets.
		where += " AND t.usage_count = 0 AND t.is_alias = 0"
	case !filter.ShowZero:
		// Hide non-alias zero-usage rows. Aliases always pass because
		// their usage_count is 0 by construction.
		where += " AND (t.usage_count > 0 OR t.is_alias = 1)"
	}
	return where, args
}

// tagOrderBy builds the ORDER BY shared by ListTags and AdjacentTags.
// The trailing id tiebreak keeps full ties deterministic so the detail
// page's prev/next walk agrees with the rendered listing.
func tagOrderBy(filter TagFilter) string {
	dir := "ASC"
	if strings.EqualFold(filter.Order, "desc") {
		dir = "DESC"
	}
	orderBy := "t.name " + dir
	switch {
	case filter.ConflictsOnly:
		// Colliding pairs must sit adjacent; the category tiebreak keeps
		// each pair's order stable.
		orderBy = "t.name ASC, t.category_id ASC"
	case filter.Sort == "usage" || filter.Sort == "created" || filter.Sort == "last_used":
		// These default to DESC when no order is set (most-used / newest /
		// most recently applied first).
		dir = "DESC"
		if strings.EqualFold(filter.Order, "asc") {
			dir = "ASC"
		}
		switch filter.Sort {
		case "usage":
			orderBy = "t.usage_count " + dir + ", t.name ASC"
		case "created":
			return "t.created_at " + dir + ", t.id " + dir
		case "last_used":
			// SQLite sorts NULL smallest, so never-applied rows land last
			// on the default DESC and first on ASC.
			orderBy = "t.last_used_at " + dir + ", t.name ASC"
		}
	}
	return orderBy + ", t.id ASC"
}

func (s *Service) ListTags(filter TagFilter) ([]models.Tag, int, error) {
	where, args := tagFilterWhere(filter)
	orderBy := tagOrderBy(filter)

	var total int
	if err := s.db.Read.QueryRow(
		"SELECT COUNT(*) FROM tags t WHERE "+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 40
	}
	offset := filter.PageIndex * limit

	// The folded-into subquery only runs on the folded-duplicates view; every
	// other listing selects a constant NULL so the hot path pays nothing.
	foldedCol := "NULL"
	if filter.FoldedOnly {
		foldedCol = `(SELECT t2.name FROM folded_tag_pairs fp JOIN tags t2 ON t2.id = fp.new_id WHERE fp.old_id = t.id AND fp.ambiguous = 0 LIMIT 1)`
	}

	// The page is picked from tags alone so the sort index can serve it
	// and stop at the limit; with the joins in the same SELECT the
	// planner drives from tag_categories, reads every tag row through
	// idx_tags_category and temp-sorts the catalog to return a hundred.
	// LEFT JOIN pulls the canonical name/category when t.is_alias = 1
	// so the caller can render "alias -> canonical" without a second
	// round trip.
	query := fmt.Sprintf(
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color,
		        t.usage_count, t.is_alias, t.canonical_tag_id, t.created_at,
		        t.origin, t.last_used_at,
		        (SELECT COUNT(*) FROM image_tags it WHERE it.tag_id = t.id AND it.stale = 1),
		        %s,
		        c.name, cc.name, cc.color
		 FROM (SELECT t.* FROM tags t WHERE %s ORDER BY %s LIMIT ? OFFSET ?) t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 LEFT JOIN tags c ON c.id = t.canonical_tag_id
		 LEFT JOIN tag_categories cc ON cc.id = c.category_id
		 ORDER BY %s`,
		foldedCol, where, orderBy, orderBy,
	)
	args = append(args, limit, offset)

	tagList, err := db.QueryAll(s.db.Read, func(rows *sql.Rows) (models.Tag, error) {
		var t models.Tag
		var isAlias int
		var canonicalID sql.NullInt64
		var createdAt string
		var lastUsed sql.NullString
		var foldedInto sql.NullString
		var canonName, canonCatName, canonCatColor sql.NullString
		if err := rows.Scan(
			&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
			&t.UsageCount, &isAlias, &canonicalID, &createdAt,
			&t.Origin, &lastUsed, &t.StaleUsage, &foldedInto,
			&canonName, &canonCatName, &canonCatColor,
		); err != nil {
			return t, err
		}
		if foldedInto.Valid {
			t.FoldedInto = foldedInto.String
		}
		if lastUsed.Valid {
			t.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed.String)
		}
		t.IsAlias = isAlias == 1
		if canonicalID.Valid {
			t.CanonicalTagID = &canonicalID.Int64
		}
		if canonName.Valid {
			t.CanonicalName = canonName.String
		}
		if canonCatName.Valid {
			t.CanonicalCategoryName = canonCatName.String
		}
		if canonCatColor.Valid {
			t.CanonicalCategoryColor = canonCatColor.String
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		return t, nil
	}, query, args...)
	if err != nil {
		return nil, 0, err
	}
	return tagList, total, nil
}

// ConflictsCount reports how many tag rows carry a name that occupies
// more than one category, for the /tags Conflicts filter badge and the
// Maintenance diagnostic. Rows rather than names: the badge is the entry
// point to the listing its own link opens, and that listing renders one
// row per (name, category) - four colliding names across two categories
// each is a badge of 8 and a header of 8.
//
// UNIQUE(name, category_id) makes COUNT(*) per name equal to the count
// of distinct categories, and the name-only form is what keeps both the
// grouping scan and the membership test inside idx_tags_active_name:
// reading category_id would send every row back to the table, which is an
// order of magnitude slower on a large catalog.
func (s *Service) ConflictsCount() (int, error) {
	var n int
	// Rows, not names: the badge is the entry point to the listing its own
	// link opens, and that listing renders one row per (name, category).
	err := s.db.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE is_alias = 0 AND name IN (
		SELECT name FROM tags WHERE is_alias = 0
		GROUP BY name HAVING COUNT(*) >= 2)`).Scan(&n)
	return n, err
}

// StaleUsageCount reports how many tags carry at least one source-dropped
// (stale) usage, for the /tags Stale filter badge.
func (s *Service) StaleUsageCount() (int, error) {
	var n int
	err := s.db.Read.QueryRow(`SELECT COUNT(DISTINCT tag_id) FROM image_tags WHERE stale = 1`).Scan(&n)
	return n, err
}

// FullyStaleCount reports how many tags have nothing but stale usage left,
// for the /tags Stale filter's second badge. The predicate mirrors
// tagFilterWhere's `full` branch so badge and listing can't disagree.
func (s *Service) FullyStaleCount() (int, error) {
	var n int
	err := s.db.Read.QueryRow(`SELECT COUNT(*) FROM tags t
		WHERE t.usage_count > 0 AND t.id IN (SELECT tag_id FROM image_tags WHERE stale = 1)
		  AND (SELECT COUNT(*) FROM image_tags it JOIN images mi ON mi.id = it.image_id AND mi.is_missing = 0
		       WHERE it.tag_id = t.id AND it.stale = 1) = t.usage_count`).Scan(&n)
	return n, err
}

// OriginCount pairs a stored creation-origin label with how many tags
// carry it.
type OriginCount struct {
	Label string
	Count int
}

// OriginCounts returns the distinct non-empty creation origins in the
// catalog, most-populated first, for the /tags sidebar filter. typeFilter
// is the listing's active Type ("tag" / "alias" / ""), applied here too so
// a badge counts the rows its own link will show.
func (s *Service) OriginCounts(typeFilter string) ([]OriginCount, error) {
	where := ""
	switch typeFilter {
	case "alias":
		where = " AND is_alias = 1"
	case "tag":
		where = " AND is_alias = 0"
	}
	return db.QueryAll(s.db.Read, func(rows *sql.Rows) (OriginCount, error) {
		var oc OriginCount
		err := rows.Scan(&oc.Label, &oc.Count)
		return oc, err
	},
		`SELECT origin, COUNT(*) FROM tags WHERE origin <> ''`+where+
			` GROUP BY origin ORDER BY COUNT(*) DESC, origin ASC`,
	)
}

// AutoTaggerLabels reports which of labels appear as an auto-tagger
// attribution (an is_auto = 1 tagger_name) so origin chips can color
// machine creators apart from site creators.
func (s *Service) AutoTaggerLabels(labels []string) (map[string]struct{}, error) {
	set := make(map[string]struct{})
	seen := make(map[string]struct{}, len(labels))
	var wanted []string
	for _, l := range labels {
		if l == "" {
			continue
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		wanted = append(wanted, l)
	}
	if len(wanted) == 0 {
		return set, nil
	}
	placeholders, args := db.InPlaceholders(wanted)
	// The literal IS NOT NULL / != '' terms restate
	// idx_image_tags_auto_tagger's partial predicate; without them the
	// planner can't prove the index applies and scans image_tags.
	labels, err := db.QueryStrings(s.db.Read,
		`SELECT DISTINCT tagger_name FROM image_tags
		 WHERE is_auto = 1 AND tagger_name IS NOT NULL AND tagger_name != ''
		   AND tagger_name IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		set[l] = struct{}{}
	}
	return set, nil
}

// ListTagIDs returns every tag id matching the filter, ignoring
// PageIndex / Limit. Used by /tags' bulk delete-in-current-search so
// the dialog count and the actual delete set agree.
func (s *Service) ListTagIDs(filter TagFilter) ([]int64, error) {
	where, args := tagFilterWhere(filter)
	return db.QueryIDs(s.db.Read, `SELECT t.id FROM tags t WHERE `+where+` ORDER BY t.id`, args...)
}

// AdjacentTags returns the ids neighbouring id in the filter's listing
// order, ignoring pagination, so the tag detail page can step through
// the same sequence the /tags table shows. A nil side means no
// neighbour there (or id absent from the filtered set).
func (s *Service) AdjacentTags(filter TagFilter, id int64) (prev, next *int64, err error) {
	where, args := tagFilterWhere(filter)
	// The listing has no key to seek on, so the scan is ordered and
	// unbounded; walking it and stopping at the row after the match is
	// what keeps a tag near the front of a 100k-tag catalog from paying
	// for the whole of it.
	var last int64
	var seen, found bool
	err = db.QueryIDsFunc(s.db.Read, func(cur int64) bool {
		switch {
		case found:
			n := cur
			next = &n
			return false
		case cur == id:
			found = true
			if seen {
				p := last
				prev = &p
			}
		}
		last, seen = cur, true
		return true
	}, `SELECT t.id FROM tags t WHERE `+where+` ORDER BY `+tagOrderBy(filter), args...)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, nil
	}
	return prev, next, nil
}

func (s *Service) GetTag(id int64) (*models.Tag, error) {
	var t models.Tag
	var isAlias int
	var canonicalID sql.NullInt64
	var createdAt string

	var lastUsed sql.NullString
	err := s.db.Read.QueryRow(
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color, t.usage_count,
		        t.is_alias, t.canonical_tag_id, t.created_at, t.origin, t.last_used_at
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE t.id = ?`, id,
	).Scan(
		&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
		&t.UsageCount, &isAlias, &canonicalID, &createdAt, &t.Origin, &lastUsed,
	)
	if err == sql.ErrNoRows {
		return nil, ErrTagNotFound
	}
	if err != nil {
		return nil, err
	}

	t.IsAlias = isAlias == 1
	if canonicalID.Valid {
		t.CanonicalTagID = &canonicalID.Int64
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if lastUsed.Valid {
		t.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed.String)
	}
	return &t, nil
}

// AliasesForTagIDs returns alias rows keyed by the canonical tag id
// they point at, with display fields joined for chip rendering on the
// detail page (where viewers benefit from seeing which alternate names
// also surface this image in search). One query regardless of input
// size, chunked at the SQLite parameter cap.
func (s *Service) AliasesForTagIDs(canonicalIDs []int64) (map[int64][]models.Tag, error) {
	out := make(map[int64][]models.Tag, len(canonicalIDs))
	if len(canonicalIDs) == 0 {
		return out, nil
	}
	err := db.Chunked(canonicalIDs, 500, func(batch []int64) error {
		placeholders, args := db.InPlaceholders(batch)
		aliases, err := db.QueryAll(s.db.Read, func(rows *sql.Rows) (models.Tag, error) {
			var t models.Tag
			var canonicalID int64
			var stale int64
			err := rows.Scan(
				&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
				&canonicalID, &t.Origin, &stale,
				&t.CanonicalName, &t.CanonicalCategoryName, &t.CanonicalCategoryColor,
			)
			t.Stale = stale == 1
			t.IsAlias = true
			t.CanonicalTagID = &canonicalID
			return t, err
		},
			`SELECT a.id, a.name, a.category_id, ac.name, ac.color,
			        a.canonical_tag_id, a.origin, a.stale,
			        c.name, cc.name, cc.color
			 FROM tags a
			 JOIN tag_categories ac ON ac.id = a.category_id
			 JOIN tags c ON c.id = a.canonical_tag_id
			 JOIN tag_categories cc ON cc.id = c.category_id
			 WHERE a.is_alias = 1 AND a.canonical_tag_id IN (`+placeholders+`)
			 ORDER BY a.name`,
			args...)
		if err != nil {
			return err
		}
		for _, t := range aliases {
			out[*t.CanonicalTagID] = append(out[*t.CanonicalTagID], t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AliasKey identifies an alias row by category and name.
type AliasKey struct {
	CategoryID int64
	Name       string
}

// SyncAliasStaleness reconciles the stale flag on a canonical tag's
// origin-attributed alias rows against a refresh: rows absent from fresh are
// flagged, rows listed again are cleared. The rows stay either way - the
// operator decides what to remove. Returns how many rows were newly flagged.
func (s *Service) SyncAliasStaleness(canonicalID int64, origin string, fresh map[AliasKey]bool) (int, error) {
	flagged := 0
	err := s.inWriteTx(func(tx *sql.Tx) error {
		type aliasRow struct {
			id, catID, stale int64
			name             string
		}
		present, err := db.QueryAll(tx, func(rows *sql.Rows) (aliasRow, error) {
			var a aliasRow
			err := rows.Scan(&a.id, &a.catID, &a.name, &a.stale)
			return a, err
		}, `SELECT id, category_id, name, stale FROM tags
			 WHERE is_alias = 1 AND canonical_tag_id = ? AND origin = ?`, canonicalID, origin)
		if err != nil {
			return err
		}
		var flag, clear []int64
		for _, a := range present {
			switch current := fresh[AliasKey{CategoryID: a.catID, Name: a.name}]; {
			case !current && a.stale == 0:
				flag = append(flag, a.id)
			case current && a.stale == 1:
				clear = append(clear, a.id)
			}
		}
		if err := setTagsStaleTx(tx, flag, 1); err != nil {
			return err
		}
		if err := setTagsStaleTx(tx, clear, 0); err != nil {
			return err
		}
		flagged = len(flag)
		return nil
	})
	return flagged, err
}

func setTagsStaleTx(tx *sql.Tx, ids []int64, stale int) error {
	return setStaleTx(tx, "tags", "id", "", ids, stale)
}

// setStaleTx flags or clears the stale column on the rows keyCol
// selects, narrowed further by extraWhere when the table needs a
// second key (an implication is keyed by its parent too).
func setStaleTx(tx *sql.Tx, table, keyCol, extraWhere string, ids []int64, stale int) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders, args := db.InPlaceholders(ids)
	_, err := tx.Exec(
		`UPDATE `+table+` SET stale = `+strconv.Itoa(stale)+
			` WHERE `+extraWhere+keyCol+` IN (`+placeholders+`)`,
		args...)
	return err
}

// AppliedByCount is one attribution group over a tag's image_tags rows:
// which source applies the tag and on how many images.
type AppliedByCount struct {
	Label  string // tagger_name; "" = anonymous UI adds
	IsAuto bool
	Count  int
}

// UsageMonth is one month of a tag's still-present applications, keyed
// by the row's created_at. Removals leave the history and rows moved by
// a merge keep their original dates.
type UsageMonth struct {
	Month string // YYYY-MM
	Count int
}

// UsageBreakdown aggregates a tag's image_tags rows once and returns
// both detail-page views: applied-by attribution groups and the monthly
// histogram. One pass because the row fetch dominates on popular tags -
// a monster tag's hundreds of thousands of rows are read once, not once
// per panel.
func (s *Service) UsageBreakdown(tagID int64) ([]AppliedByCount, []UsageMonth, error) {
	rows, err := s.db.Read.Query(
		`SELECT COALESCE(tagger_name, ''), is_auto, strftime('%Y-%m', created_at), COUNT(*)
		 FROM image_tags WHERE tag_id = ?
		 GROUP BY COALESCE(tagger_name, ''), is_auto, strftime('%Y-%m', created_at)`,
		tagID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	appliedIdx := make(map[[2]any]int)
	monthIdx := make(map[string]int)
	var applied []AppliedByCount
	var months []UsageMonth
	for rows.Next() {
		var label, month string
		var isAuto, count int
		if err := rows.Scan(&label, &isAuto, &month, &count); err != nil {
			return nil, nil, err
		}
		ak := [2]any{label, isAuto}
		if i, ok := appliedIdx[ak]; ok {
			applied[i].Count += count
		} else {
			appliedIdx[ak] = len(applied)
			applied = append(applied, AppliedByCount{Label: label, IsAuto: isAuto == 1, Count: count})
		}
		if i, ok := monthIdx[month]; ok {
			months[i].Count += count
		} else {
			monthIdx[month] = len(months)
			months = append(months, UsageMonth{Month: month, Count: count})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// Tie-break by label asc so two equivalent runs produce the same
	// ordering, and the panel does not reshuffle between renders.
	sort.Slice(applied, func(i, j int) bool {
		if applied[i].Count != applied[j].Count {
			return applied[i].Count > applied[j].Count
		}
		return applied[i].Label < applied[j].Label
	})
	sort.Slice(months, func(i, j int) bool { return months[i].Month < months[j].Month })
	return applied, months, nil
}

// GetImageTags returns the per-image tag list alongside the owning
// image's folder_path. Both pieces are read in one round trip via a
// LEFT JOIN over images so a freshly-uploaded image with no tags still
// surfaces its folder. The folder is empty when the image id is unknown.
func (s *Service) GetImageTags(imageID int64) (string, []models.ImageTag, error) {
	rows, err := s.db.Read.Query(
		`SELECT i.folder_path,
		        it.image_id, it.tag_id, t.name, tc.name, tc.color, t.usage_count,
		        it.is_auto, it.is_implied, it.confidence, it.tagger_name, it.stale, it.created_at
		 FROM images i
		 LEFT JOIN image_tags it ON it.image_id = i.id
		 LEFT JOIN tags t ON t.id = it.tag_id
		 LEFT JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE i.id = ?
		 ORDER BY tc.name, t.usage_count DESC, t.name`, imageID,
	)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = rows.Close() }()

	var folder string
	var result []models.ImageTag
	for rows.Next() {
		var (
			folderPath sql.NullString
			imgID      sql.NullInt64
			tagID      sql.NullInt64
			tagName    sql.NullString
			category   sql.NullString
			color      sql.NullString
			usage      sql.NullInt64
			isAuto     sql.NullInt64
			isImplied  sql.NullInt64
			conf       sql.NullFloat64
			tagger     sql.NullString
			stale      sql.NullInt64
			createdAt  sql.NullString
		)
		if err := rows.Scan(
			&folderPath,
			&imgID, &tagID, &tagName, &category, &color, &usage,
			&isAuto, &isImplied, &conf, &tagger, &stale, &createdAt,
		); err != nil {
			return "", nil, err
		}
		if folderPath.Valid {
			folder = folderPath.String
		}
		if !imgID.Valid || !tagID.Valid {
			// Untagged image - the LEFT JOIN emitted one row with NULL
			// tag columns; skip it but keep the folder.
			continue
		}
		it := models.ImageTag{
			ImageID:    imgID.Int64,
			TagID:      tagID.Int64,
			TagName:    tagName.String,
			Category:   category.String,
			Color:      color.String,
			UsageCount: int(usage.Int64),
			IsAuto:     isAuto.Int64 == 1,
			IsImplied:  isImplied.Int64 == 1,
			Stale:      stale.Int64 == 1,
		}
		if conf.Valid {
			it.Confidence = &conf.Float64
		}
		if tagger.Valid {
			it.TaggerName = tagger.String
		}
		if createdAt.Valid {
			it.CreatedAt, _ = time.Parse(time.RFC3339, createdAt.String)
		}
		result = append(result, it)
	}
	return folder, result, rows.Err()
}

// deleteTagsTx strips ids from every image - including each id's transitive
// implied closure - and removes their aliases, inside tx. It does not delete
// the tag rows themselves: the single-tag and whole-category callers delete
// those by different keys. The returned closure is the set of implied
// descendants the caller must RecalcIDs after commit.
func deleteTagsTx(tx *sql.Tx, ids []int64) ([]int64, error) {
	closure, err := transitiveImpliedTx(tx, ids)
	if err != nil {
		return nil, fmt.Errorf("walk implied closure: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM image_tags WHERE tag_id = ?`, id); err != nil {
			return nil, fmt.Errorf("strip parent image_tags: %w", err)
		}
	}
	// Tier order: transitiveImpliedTx returns BFS, so dropping rows in
	// that order makes deeper tiers see the now-gone upstream rows when
	// they re-check whether any remaining parent on the image still
	// justifies them.
	for _, impID := range closure {
		if _, err := tx.Exec(
			`DELETE FROM image_tags
			 WHERE tag_id = ? AND is_implied = 1
			   AND NOT EXISTS (
			       SELECT 1 FROM tag_implications ti
			       JOIN image_tags it_p ON it_p.tag_id = ti.parent_tag_id
			       WHERE ti.implied_tag_id = ? AND it_p.image_id = image_tags.image_id
			   )`,
			impID, impID,
		); err != nil {
			return nil, fmt.Errorf("sweep implied %d: %w", impID, err)
		}
	}
	for _, id := range ids {
		// Imported or legacy rows can chain alias -> alias, even back through
		// the deleted tag. Null the tag's own pointer so a cycle can't
		// dangle-check the sweep, then drop the whole alias subtree in one
		// statement so intra-chain references vanish together.
		if _, err := tx.Exec(`UPDATE tags SET canonical_tag_id = NULL WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("unlink canonical: %w", err)
		}
		if _, err := tx.Exec(
			`DELETE FROM tags WHERE id IN (
			     WITH RECURSIVE sub(id) AS (
			         SELECT id FROM tags WHERE canonical_tag_id = ?
			         UNION
			         SELECT t.id FROM tags t JOIN sub ON t.canonical_tag_id = sub.id
			     )
			     SELECT id FROM sub
			 )`, id,
		); err != nil {
			return nil, fmt.Errorf("delete aliases: %w", err)
		}
	}
	return closure, nil
}

// DeleteTag removes a tag from every image and drops the tag row. Alias
// rows pointing at it are removed too (their canonical_tag_id would
// otherwise dangle). image_tags rows cascade on the tags FK, but the
// per-image removal runs first so the implied closure is swept - the
// FK cascade alone would drop the parent row and leave its
// is_implied=1 dependents on every carrier image with nothing on the
// image to justify them.
//
// Implementation: one bulk DELETE for the parent's image_tags rows,
// then a tier-by-tier sweep of the transitive implied closure where
// each tier deletes is_implied=1 rows whose only justification was the
// (now-gone) parent or an upstream implied. Same end-state as the
// per-image walk but linear in the number of distinct implied tags
// rather than the number of carrier images. Final RecalcIDs reconciles
// usage_count for every tag the cascade touched.
//
// The four canonical rating tags are immutable in the catalog (their
// names are part of the data model) so the row itself stays. Delete on
// one of them strips its image_tags rows instead - the user-visible
// "remove this rating from every image" the UI exposes. An imported
// rating-category row under any other name is not vocabulary and
// deletes outright.
func (s *Service) DeleteTag(id int64) error {
	if s.isLockedRatingTag(id) {
		return s.stripTagFromAllImages(id)
	}
	var closure []int64
	err := s.inWriteTx(func(tx *sql.Tx) error {
		var err error
		closure, err = deleteTagsTx(tx, []int64{id})
		if err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM tags WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete tag: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrTagNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Recompute usage_count over the swept set so the cached value
	// reflects the post-state without a full Recalc.
	if len(closure) > 0 {
		return s.RecalcIDs(closure)
	}
	return nil
}

// stripTagFromAllImages clears every image_tags row for tagID and zeros
// the tag's usage_count. Used by DeleteTag's rating-tag branch where
// the catalog row must stay intact.
func (s *Service) stripTagFromAllImages(tagID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM image_tags WHERE tag_id = ?`, tagID); err != nil {
			return fmt.Errorf("strip image_tags: %w", err)
		}
		if _, err := tx.Exec(`UPDATE tags SET usage_count = 0 WHERE id = ?`, tagID); err != nil {
			return fmt.Errorf("zero usage: %w", err)
		}
		return nil
	})
}

// isRatingTag reports whether the tag with this id lives in the rating
// category. Returns false on lookup error so a missing row falls through
// to the existing ErrTagNotFound path in the caller.
func (s *Service) isRatingTag(id int64) bool {
	_, ok := s.ratingRowName(id)
	return ok
}

// isLockedRatingTag reports whether the tag with this id is one of the
// four canonical rating rows. Those are the rows the vocabulary is made
// of; a rating-category row under any other name only reaches the
// catalog through a raw import (§13.5 normalizes colours and the alias
// graph, not rating names) and stays repairable.
func (s *Service) isLockedRatingTag(id int64) bool {
	name, ok := s.ratingRowName(id)
	return ok && IsCanonicalRating(name)
}

func (s *Service) ratingRowName(id int64) (string, bool) {
	if s.ratingCatID == 0 {
		return "", false
	}
	var catID int64
	var name string
	if err := s.db.Read.QueryRow(`SELECT category_id, name FROM tags WHERE id = ?`, id).Scan(&catID, &name); err != nil {
		return "", false
	}
	return name, catID == s.ratingCatID
}

// RenameTag renames a tag. The new name must pass validation and must
// not collide with another tag in the same category.
func (s *Service) RenameTag(id int64, newName string) error {
	return s.renameTag(id, newName, false)
}

// rowQuerier is the single-row read surface both *sql.DB and *sql.Tx
// satisfy.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// nameTaken returns the id of another tag holding (name, catID), or 0
// when the slot is free. exceptID is the row being renamed or moved.
func nameTaken(q rowQuerier, name string, catID, exceptID int64) (int64, error) {
	var existing int64
	switch err := q.QueryRow(
		`SELECT id FROM tags WHERE name = ? AND category_id = ? AND id != ?`, name, catID, exceptID,
	).Scan(&existing); {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("check name %q in category %d: %w", name, catID, err)
	}
	return existing, nil
}

// RenameTagKeepAlias renames the tag and installs its old name as an
// alias of the renamed row in the same transaction, so searches and
// adds of the old spelling keep resolving. Refused on alias rows - the
// leftover alias would point at an alias, a chain the resolver doesn't
// follow.
func (s *Service) RenameTagKeepAlias(id int64, newName string) error {
	return s.renameTag(id, newName, true)
}

// renameTag is the shared body: validate, load, refuse a locked rating
// row, check the destination slot, rename. keepAlias also installs the
// old spelling as an alias of the renamed row, which is why the whole
// thing runs in one transaction either way.
func (s *Service) renameTag(id int64, newName string, keepAlias bool) error {
	normalized, err := ValidateTagName(newName)
	if err != nil {
		return err
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		var catID int64
		var oldName string
		var isAlias int
		if err := tx.QueryRow(`SELECT category_id, name, is_alias FROM tags WHERE id = ?`, id).Scan(&catID, &oldName, &isAlias); err != nil {
			// Surface 404 for a missing id. The UPDATE below would
			// otherwise no-op silently and the handler would report a
			// successful rename for a tag that doesn't exist.
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTagNotFound
			}
			return fmt.Errorf("look up tag %d: %w", id, err)
		}
		if keepAlias && isAlias == 1 {
			// The leftover alias would point at an alias, a chain the
			// resolver doesn't follow.
			return fmt.Errorf("cannot keep the old name of an alias; rename it plainly")
		}
		if s.ratingCatID != 0 && catID == s.ratingCatID && IsCanonicalRating(oldName) {
			return ErrRatingTagImmutable
		}
		if oldName == normalized {
			return nil
		}
		existing, err := nameTaken(tx, normalized, catID, id)
		if err != nil {
			return err
		}
		if existing != 0 {
			return fmt.Errorf("a tag named %q already exists in this category", normalized)
		}
		if _, err := tx.Exec(`UPDATE tags SET name = ? WHERE id = ?`, normalized, id); err != nil {
			return err
		}
		if !keepAlias {
			return nil
		}
		// The old (name, category) slot vacated inside this tx, so the
		// alias insert cannot collide.
		_, err = tx.Exec(
			`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count, origin) VALUES (?, ?, 1, ?, 0, 'user')`,
			oldName, catID, id,
		)
		return err
	})
}

// ErrCategoryCollision reports the (name, target category) collision a
// category move runs into, carrying the surviving row's id so callers
// can offer to merge into it instead.
type ErrCategoryCollision struct {
	Name       string
	ExistingID int64
}

func (e *ErrCategoryCollision) Error() string {
	return fmt.Sprintf("a tag named %q already exists in the target category", e.Name)
}

// ChangeTagCategoryMerge is ChangeTagCategory that resolves a name
// collision by merging the tag into the target category's existing row
// (the moving tag becomes an alias of the survivor). The bool reports
// whether a merge happened instead of a plain move.
func (s *Service) ChangeTagCategoryMerge(tagID, newCategoryID int64) (bool, error) {
	err := s.ChangeTagCategory(tagID, newCategoryID)
	var coll *ErrCategoryCollision
	if errors.As(err, &coll) {
		if err := s.MergeTags(tagID, coll.ExistingID); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, err
}

// ChangeTagCategory moves a tag to a different category. Returns
// ErrTagNotFound, ErrCategoryNotFound, or ErrCategoryCollision when a
// tag with the same name already lives in the target category.
func (s *Service) ChangeTagCategory(tagID, newCategoryID int64) error {
	var currentCatID int64
	var name string
	if err := s.db.Read.QueryRow(
		`SELECT category_id, name FROM tags WHERE id = ?`, tagID,
	).Scan(&currentCatID, &name); err != nil {
		return ErrTagNotFound
	}
	if currentCatID == newCategoryID {
		return nil
	}
	// Moving in stays refused whatever the name: the category takes only
	// its four canonical rows. Moving out is refused only for those rows.
	if s.ratingCatID != 0 && (newCategoryID == s.ratingCatID ||
		(currentCatID == s.ratingCatID && IsCanonicalRating(name))) {
		return ErrRatingTagImmutable
	}
	var catExists int
	if err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM tag_categories WHERE id = ?`, newCategoryID,
	).Scan(&catExists); err != nil || catExists == 0 {
		return ErrCategoryNotFound
	}
	// Reject up front so the user gets a clean message rather than the
	// raw UNIQUE(name, category_id) constraint error.
	existing, err := nameTaken(s.db.Read, name, newCategoryID, tagID)
	if err != nil {
		return err
	}
	if existing != 0 {
		return &ErrCategoryCollision{Name: name, ExistingID: existing}
	}
	_, err = s.db.Write.Exec(`UPDATE tags SET category_id = ? WHERE id = ?`, newCategoryID, tagID)
	return err
}
