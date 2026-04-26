package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

var (
	ErrInvalidTagName       = errors.New("invalid tag name")
	ErrTagNotFound          = errors.New("tag not found")
	ErrCategoryNotFound     = errors.New("category not found")
	ErrBuiltinCategory      = errors.New("cannot delete built-in category")
	ErrReservedCategoryName = errors.New("this name is used by a search filter (e.g. fav:, source:, cat:, width:, height:, date:, missing:, folder:, folderonly:, generated:, animated:, tagged:, autotagged:)")

	// Allowed tag name characters. The colon is kept despite doubling
	// as the category:tag separator; the parser falls back to a literal
	// tag when the prefix is neither a filter keyword nor a known
	// category.
	tagNameRe = regexp.MustCompile(`^[a-z0-9_()!@#$.~+:\-]+$`)

	// #rgb or #rrggbb. Anything else gets ZgotmplZ'd in the template's
	// CSS context, so reject it up front with a useful error.
	categoryColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

	ErrInvalidCategoryColor = errors.New("invalid category color (must be #rgb or #rrggbb)")
)

// IsValidCategoryColor reports whether s matches the #rgb / #rrggbb
// shape the UI form enforces.
func IsValidCategoryColor(s string) bool { return categoryColorRe.MatchString(s) }

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
	// reservedCategoryNames are search-filter keywords that would
	// collide with a category-qualified tag search. Category names
	// matching any of these are refused at create/rename time.
	reservedCategoryNames = map[string]struct{}{
		"fav":        {},
		"source":     {},
		"cat":        {},
		"width":      {},
		"height":     {},
		"date":       {},
		"missing":    {},
		"folder":     {},
		"folderonly": {},
		"generated":  {},
		"animated":   {},
		"tagged":     {},
		"autotagged": {},
	}
)

func isReservedCategoryName(name string) bool {
	_, ok := reservedCategoryNames[name]
	return ok
}

// TagFilter controls listing behavior.
type TagFilter struct {
	CategoryID *int64
	Prefix     string
	Sort       string // "name" | "usage"
	// PageIndex is 0-based - callers supply the requested page number minus
	// one. ListTags multiplies by Limit to derive the SQL OFFSET.
	PageIndex int
	Limit     int
	Origin    string // "" | "user" | "auto"
}

// Service provides tag and category CRUD with usage_count and co-occurrence maintenance.
type Service struct {
	db *db.DB
}

// New creates a new Service.
func New(database *db.DB) *Service {
	return &Service{db: database}
}

// RecalcAndPruneDB recomputes usage_count from image_tags (non-missing
// images only) and deletes any tags that drop to zero usage. Call after
// bulk deletes or sync.
func RecalcAndPruneDB(database *db.DB) {
	_, _ = RecalcAndPruneDBCount(database)
}

// RecalcAndPruneDBCount is RecalcAndPruneDB with row counts.
//
// A naïve correlated-subquery UPDATE recomputes the count twice per
// tag and dominates sync time on tag-heavy libraries. This impl zeros
// out tags whose remaining usages all point at missing images, then
// fills in the rest with one GROUP BY pass over image_tags.
func RecalcAndPruneDBCount(database *db.DB) (updated int64, pruned int64) {
	if res, err := database.Write.Exec(`
		UPDATE tags SET usage_count = 0
		WHERE usage_count != 0
		  AND NOT EXISTS (
		      SELECT 1 FROM image_tags it
		      JOIN images i ON i.id = it.image_id
		      WHERE it.tag_id = tags.id AND i.is_missing = 0
		  )
	`); err == nil {
		n, _ := res.RowsAffected()
		updated += n
	}
	if res, err := database.Write.Exec(`
		UPDATE tags SET usage_count = c.cnt
		FROM (
		    SELECT it.tag_id, COUNT(*) AS cnt FROM image_tags it
		    JOIN images i ON i.id = it.image_id
		    WHERE i.is_missing = 0
		    GROUP BY it.tag_id
		) c
		WHERE c.tag_id = tags.id AND tags.usage_count != c.cnt
	`); err == nil {
		n, _ := res.RowsAffected()
		updated += n
	}
	if res, err := database.Write.Exec(`DELETE FROM tags WHERE usage_count <= 0`); err == nil {
		pruned, _ = res.RowsAffected()
	}
	return
}

func (s *Service) RecalcAndPrune() {
	RecalcAndPruneDB(s.db)
}

func (s *Service) RecalcAndPruneCount() (updated int64, pruned int64) {
	return RecalcAndPruneDBCount(s.db)
}

// RecalcAndPruneIDs recomputes usage_count for the given tag IDs and
// prunes any that drop to zero usage. Lets bulk callers scope the work
// to tags they actually touched instead of walking the whole table.
// IDs are processed in chunks to stay under the SQLite parameter limit.
func (s *Service) RecalcAndPruneIDs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if _, err := s.db.Write.Exec(`UPDATE tags SET usage_count = (
			SELECT COUNT(*) FROM image_tags it
			JOIN images i ON i.id = it.image_id
			WHERE it.tag_id = tags.id AND i.is_missing = 0
		) WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("recalc usage_count chunk: %w", err)
		}
		if _, err := s.db.Write.Exec(`DELETE FROM tags WHERE usage_count <= 0 AND id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("prune zero-usage chunk: %w", err)
		}
	}
	return nil
}

// --- Category methods ---

func (s *Service) ListCategories() ([]models.TagCategory, error) {
	rows, err := s.db.Read.Query(
		`SELECT id, name, color, is_builtin FROM tag_categories ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cats []models.TagCategory
	for rows.Next() {
		var c models.TagCategory
		var isBuiltin int
		if err := rows.Scan(&c.ID, &c.Name, &c.Color, &isBuiltin); err != nil {
			return nil, err
		}
		c.IsBuiltin = isBuiltin == 1
		cats = append(cats, c)
	}
	return cats, rows.Err()
}

func (s *Service) CreateCategory(name, color string) (*models.TagCategory, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return nil, fmt.Errorf("category name must not be empty")
	}
	if isReservedCategoryName(name) {
		return nil, ErrReservedCategoryName
	}
	color = strings.TrimSpace(color)
	if !categoryColorRe.MatchString(color) {
		return nil, ErrInvalidCategoryColor
	}
	var id int64
	err := s.db.Write.QueryRow(
		`INSERT INTO tag_categories (name, color) VALUES (?, ?) RETURNING id`,
		name, color,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("creating category: %w", err)
	}
	return &models.TagCategory{ID: id, Name: name, Color: color}, nil
}

func (s *Service) UpdateCategoryColor(id int64, color string) error {
	color = strings.TrimSpace(color)
	if !categoryColorRe.MatchString(color) {
		return ErrInvalidCategoryColor
	}
	_, err := s.db.Write.Exec(
		`UPDATE tag_categories SET color = ? WHERE id = ?`, color, id,
	)
	return err
}

func (s *Service) RenameCategory(id int64, newName string) error {
	newName = strings.TrimSpace(strings.ToLower(newName))
	if newName == "" {
		return fmt.Errorf("category name must not be empty")
	}
	if isReservedCategoryName(newName) {
		return ErrReservedCategoryName
	}
	var isBuiltin int
	if err := s.db.Read.QueryRow(
		`SELECT is_builtin FROM tag_categories WHERE id = ?`, id,
	).Scan(&isBuiltin); err != nil {
		return ErrCategoryNotFound
	}
	if isBuiltin == 1 {
		return ErrBuiltinCategory
	}
	_, err := s.db.Write.Exec(
		`UPDATE tag_categories SET name = ? WHERE id = ?`, newName, id,
	)
	return err
}

// GetCategoryTagCount returns the number of tags in a category.
func (s *Service) GetCategoryTagCount(id int64) (int, error) {
	var count int
	err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE category_id = ? AND is_alias = 0`, id,
	).Scan(&count)
	return count, err
}

// DeleteCategoryMoveOrDelete deletes a category. action="delete_all"
// deletes all tags in the category; "move" reparents them to targetID.
func (s *Service) DeleteCategoryMoveOrDelete(id int64, action string, targetID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var isBuiltin int
	if err := tx.QueryRow(
		`SELECT is_builtin FROM tag_categories WHERE id = ?`, id,
	).Scan(&isBuiltin); err == sql.ErrNoRows {
		return ErrCategoryNotFound
	} else if err != nil {
		return err
	}
	if isBuiltin == 1 {
		return ErrBuiltinCategory
	}

	switch action {
	case "delete_all":
		rows, err := tx.Query(`SELECT id FROM tags WHERE category_id = ?`, id)
		if err != nil {
			return err
		}
		var tagIDs []int64
		for rows.Next() {
			var tid int64
			if scanErr := rows.Scan(&tid); scanErr != nil {
				rows.Close()
				return scanErr
			}
			tagIDs = append(tagIDs, tid)
		}
		if iterErr := rows.Err(); iterErr != nil {
			rows.Close()
			return iterErr
		}
		rows.Close()
		for _, tid := range tagIDs {
			if _, err := tx.Exec(`DELETE FROM image_tags WHERE tag_id = ?`, tid); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM tags WHERE category_id = ?`, id); err != nil {
			return err
		}
	default: // "move"
		if targetID == 0 {
			if err := tx.QueryRow(
				`SELECT id FROM tag_categories WHERE name = 'general'`,
			).Scan(&targetID); err != nil {
				return fmt.Errorf("finding general category: %w", err)
			}
		}
		if _, err := tx.Exec(
			`UPDATE tags SET category_id = ? WHERE category_id = ?`, targetID, id,
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`DELETE FROM tag_categories WHERE id = ?`, id); err != nil {
		return err
	}

	return tx.Commit()
}

// --- Tag methods ---

// ValidateTagName lowercases + trims name and checks it against the
// documented allowlist. Returns the normalised name or an
// ErrInvalidTagName-wrapped error. Exposed so non-UI sources (the
// auto-tagger label loader, the JSON import path) apply the same rules.
func ValidateTagName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.ToLower(name)

	if len(name) == 0 || len(name) > 200 {
		return "", fmt.Errorf("%w: length must be 1–200 characters", ErrInvalidTagName)
	}

	if !tagNameRe.MatchString(name) {
		return "", fmt.Errorf("%w: contains invalid characters (allowed: a-z 0-9 _ ( ) ! @ # $ . ~ + - :)", ErrInvalidTagName)
	}

	allPunct := true
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			allPunct = false
			break
		}
	}
	if allPunct {
		return "", fmt.Errorf("%w: name must contain at least one letter or digit", ErrInvalidTagName)
	}

	return name, nil
}

func (s *Service) GetOrCreateTag(name string, categoryID int64) (*models.Tag, error) {
	normalized, err := ValidateTagName(name)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Write.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	tag, err := getOrCreateTagTx(tx, normalized, categoryID)
	if err != nil {
		return nil, err
	}
	return tag, tx.Commit()
}

func getOrCreateTagTx(tx *sql.Tx, name string, categoryID int64) (*models.Tag, error) {
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
			`INSERT INTO tags (name, category_id) VALUES (?, ?) RETURNING id`,
			name, categoryID,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("inserting tag: %w", err)
		}
		tag = models.Tag{
			ID:         id,
			Name:       name,
			CategoryID: categoryID,
			CreatedAt:  time.Now().UTC(),
		}
		return &tag, nil
	}
	if err != nil {
		return nil, err
	}

	// If this row is an alias, redirect to its canonical. MergeTags
	// refuses to point an alias at another alias, so one hop is enough.
	if tag.IsAlias && canonicalID.Valid {
		var canon models.Tag
		var canonCreated string
		if err := tx.QueryRow(
			`SELECT id, name, category_id, usage_count, is_alias, created_at FROM tags WHERE id = ?`,
			canonicalID.Int64,
		).Scan(&canon.ID, &canon.Name, &canon.CategoryID, &canon.UsageCount, &canon.IsAlias, &canonCreated); err == nil {
			canon.CreatedAt, _ = time.Parse(time.RFC3339, canonCreated)
			return &canon, nil
		}
	}

	tag.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &tag, nil
}

func (s *Service) ListTags(filter TagFilter) ([]models.Tag, int, error) {
	args := []any{}
	where := "1=1"

	if filter.CategoryID != nil {
		where += " AND t.category_id = ?"
		args = append(args, *filter.CategoryID)
	}
	if filter.Prefix != "" {
		where += " AND t.name LIKE ?"
		args = append(args, filter.Prefix+"%")
	}
	switch filter.Origin {
	case "auto":
		where += " AND t.is_alias = 0 AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.tag_id = t.id AND it.is_auto = 0)"
	case "user":
		where += " AND t.is_alias = 0 AND EXISTS (SELECT 1 FROM image_tags it WHERE it.tag_id = t.id AND it.is_auto = 0)"
	case "alias":
		where += " AND t.is_alias = 1"
	}

	orderBy := "t.name ASC"
	if filter.Sort == "usage" {
		orderBy = "t.usage_count DESC, t.name ASC"
	}

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

	// LEFT JOIN pulls the canonical name/category when t.is_alias = 1
	// so the caller can render "alias → canonical" without a second
	// round trip.
	query := fmt.Sprintf(
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color,
		        t.usage_count, t.is_alias, t.canonical_tag_id, t.created_at,
		        c.name, cc.name, cc.color
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 LEFT JOIN tags c ON c.id = t.canonical_tag_id
		 LEFT JOIN tag_categories cc ON cc.id = c.category_id
		 WHERE %s ORDER BY %s LIMIT ? OFFSET ?`,
		where, orderBy,
	)
	args = append(args, limit, offset)

	rows, err := s.db.Read.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var tagList []models.Tag
	for rows.Next() {
		var t models.Tag
		var isAlias int
		var canonicalID sql.NullInt64
		var createdAt string
		var canonName, canonCatName, canonCatColor sql.NullString
		if err := rows.Scan(
			&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
			&t.UsageCount, &isAlias, &canonicalID, &createdAt,
			&canonName, &canonCatName, &canonCatColor,
		); err != nil {
			return nil, 0, err
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
		tagList = append(tagList, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Resolve IsAutoOnly in one batch keyed on canonical rows only.
	// Aliases have no image_tags of their own; the origin badge for
	// them is "alias", not "auto-only".
	var ids []any
	for _, t := range tagList {
		if !t.IsAlias {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) > 0 {
		placeholders := strings.Repeat("?,", len(ids))
		placeholders = placeholders[:len(placeholders)-1]
		userRows, err := s.db.Read.Query(
			`SELECT DISTINCT tag_id FROM image_tags WHERE is_auto = 0 AND tag_id IN (`+placeholders+`)`,
			ids...,
		)
		if err != nil {
			return nil, 0, err
		}
		hasUser := map[int64]struct{}{}
		for userRows.Next() {
			var id int64
			if err := userRows.Scan(&id); err != nil {
				userRows.Close()
				return nil, 0, err
			}
			hasUser[id] = struct{}{}
		}
		userRows.Close()
		for i := range tagList {
			if tagList[i].IsAlias {
				continue
			}
			if _, ok := hasUser[tagList[i].ID]; !ok {
				tagList[i].IsAutoOnly = true
			}
		}
	}

	return tagList, total, nil
}

func (s *Service) GetTag(id int64) (*models.Tag, error) {
	var t models.Tag
	var isAlias int
	var canonicalID sql.NullInt64
	var createdAt string

	err := s.db.Read.QueryRow(
		`SELECT t.id, t.name, t.category_id, tc.name, tc.color, t.usage_count,
		        t.is_alias, t.canonical_tag_id, t.created_at
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE t.id = ?`, id,
	).Scan(
		&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
		&t.UsageCount, &isAlias, &canonicalID, &createdAt,
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
	return &t, nil
}

// --- Image tag methods ---

func (s *Service) GetImageTags(imageID int64) ([]models.ImageTag, error) {
	rows, err := s.db.Read.Query(
		`SELECT it.image_id, it.tag_id, t.name, tc.name, tc.color, t.usage_count,
		        it.is_auto, it.confidence, it.tagger_name, it.created_at
		 FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ?
		 ORDER BY tc.name, t.usage_count DESC, t.name`, imageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.ImageTag
	for rows.Next() {
		var it models.ImageTag
		var isAuto int
		var conf sql.NullFloat64
		var taggerName sql.NullString
		var createdAt string
		if err := rows.Scan(
			&it.ImageID, &it.TagID, &it.TagName, &it.Category, &it.Color, &it.UsageCount,
			&isAuto, &conf, &taggerName, &createdAt,
		); err != nil {
			return nil, err
		}
		it.IsAuto = isAuto == 1
		if conf.Valid {
			it.Confidence = &conf.Float64
		}
		if taggerName.Valid {
			it.TaggerName = taggerName.String
		}
		it.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		result = append(result, it)
	}
	return result, rows.Err()
}

func (s *Service) AddTagToImage(imageID, tagID int64, isAuto bool, confidence *float64) error {
	_, err := s.AddTagToImageReportingDup(imageID, tagID, isAuto, confidence, "")
	return err
}

// AddTagToImageFromTagger is AddTagToImage with a source identifier
// stored alongside the row (auto-tagger subfolder name when isAuto, or
// any caller-supplied string for manual/API adds).
func (s *Service) AddTagToImageFromTagger(imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) error {
	_, err := s.AddTagToImageReportingDup(imageID, tagID, isAuto, confidence, taggerName)
	return err
}

// AddTagToImageReportingDup runs INSERT OR IGNORE inside a write-pool
// transaction and returns whether a new row was inserted. Lets a
// caller atomically distinguish "added" from "already on image"
// without the read-then-write race of doing it across pools.
func (s *Service) AddTagToImageReportingDup(imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) (bool, error) {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	added, err := addTagToImageTxReportingDup(tx, imageID, tagID, isAuto, confidence, taggerName)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return added, nil
}

func addTagToImageTxReportingDup(tx *sql.Tx, imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) (bool, error) {
	isAutoInt := 0
	if isAuto {
		isAutoInt = 1
	}
	// tagger_name doubles as a generic source identifier: the tagger
	// subfolder name on auto rows, any caller-supplied string on manual
	// rows, NULL for UI-driven user adds.
	var tname any
	if taggerName != "" {
		tname = taggerName
	}

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, confidence, tagger_name) VALUES (?, ?, ?, ?, ?)`,
		imageID, tagID, isAutoInt, confidence, tname,
	)
	if err != nil {
		return false, fmt.Errorf("inserting image_tag: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return false, nil
	}

	if _, err := tx.Exec(
		`UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tagID,
	); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) RemoveTagFromImage(imageID, tagID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := removeTagFromImageTx(tx, imageID, tagID); err != nil {
		return err
	}
	return tx.Commit()
}

func removeTagFromImageTx(tx *sql.Tx, imageID, tagID int64) error {
	res, err := tx.Exec(
		`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, tagID,
	)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Tag wasn't on the image; nothing to decrement.
		return nil
	}

	if _, err := tx.Exec(
		`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, tagID,
	); err != nil {
		return err
	}

	// Surface the prune error so the surrounding tx can roll back rather
	// than commit a zero-usage tag that would leak into the sidebar.
	if _, err := tx.Exec(`DELETE FROM tags WHERE id = ? AND usage_count <= 0`, tagID); err != nil {
		return fmt.Errorf("prune zero-usage tag %d: %w", tagID, err)
	}
	return nil
}

// RemoveAllAutoTags deletes every auto-tagged image_tags row across the
// library. A non-empty taggerNames restricts the deletion to rows whose
// tagger_name matches. Affected tag usage counts are recomputed (scoped
// to the rows we actually touched) and zero-usage tags pruned afterwards.
func (s *Service) RemoveAllAutoTags(taggerNames []string) (int, error) {
	where := `is_auto = 1`
	args := []any{}
	if len(taggerNames) > 0 {
		placeholders := strings.Repeat("?,", len(taggerNames))
		placeholders = placeholders[:len(placeholders)-1]
		where += ` AND tagger_name IN (` + placeholders + `)`
		for _, n := range taggerNames {
			args = append(args, n)
		}
	}
	touched, err := s.collectTagIDs(`SELECT DISTINCT tag_id FROM image_tags WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Write.Exec(`DELETE FROM image_tags WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	if err := s.RecalcAndPruneIDs(touched); err != nil {
		return int(removed), err
	}
	return int(removed), nil
}

// RemoveAllUserTags deletes every manual image_tags row across the
// library, then recomputes usage counts for the touched tags.
func (s *Service) RemoveAllUserTags() (int, error) {
	touched, err := s.collectTagIDs(`SELECT DISTINCT tag_id FROM image_tags WHERE is_auto = 0`)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Write.Exec(`DELETE FROM image_tags WHERE is_auto = 0`)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	if err := s.RecalcAndPruneIDs(touched); err != nil {
		return int(removed), err
	}
	return int(removed), nil
}

// RemoveAllTags deletes every image_tags row across the library, both
// manual and auto-tagged, then recomputes usage counts for the touched
// tags.
func (s *Service) RemoveAllTags() (int, error) {
	touched, err := s.collectTagIDs(`SELECT DISTINCT tag_id FROM image_tags`)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Write.Exec(`DELETE FROM image_tags`)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	if err := s.RecalcAndPruneIDs(touched); err != nil {
		return int(removed), err
	}
	return int(removed), nil
}

// collectTagIDs runs the given SELECT and returns the resulting tag_id
// list. Used by the bulk-removal helpers to scope their post-delete
// RecalcAndPruneIDs call to the tags actually touched.
func (s *Service) collectTagIDs(query string, args ...any) ([]int64, error) {
	rows, err := s.db.Read.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RemoveUserTagsFromImage drops every manual image_tags row for one
// image and adjusts usage counts.
func (s *Service) RemoveUserTagsFromImage(imageID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT tag_id FROM image_tags WHERE image_id = ? AND is_auto = 0`, imageID)
	if err != nil {
		return err
	}
	var tagIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tagIDs = append(tagIDs, id)
	}
	rows.Close()

	for _, tagID := range tagIDs {
		if err := removeTagFromImageTx(tx, imageID, tagID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveAutoTagsFromImage drops auto-tagged image_tags rows for one
// image. A non-empty taggerNames restricts the deletion to rows whose
// tagger_name matches.
func (s *Service) RemoveAutoTagsFromImage(imageID int64, taggerNames []string) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		rows *sql.Rows
	)
	if len(taggerNames) == 0 {
		rows, err = tx.Query(`SELECT tag_id FROM image_tags WHERE image_id = ? AND is_auto = 1`, imageID)
	} else {
		placeholders := strings.Repeat("?,", len(taggerNames))
		placeholders = placeholders[:len(placeholders)-1]
		args := []any{imageID}
		for _, n := range taggerNames {
			args = append(args, n)
		}
		rows, err = tx.Query(
			`SELECT tag_id FROM image_tags WHERE image_id = ? AND is_auto = 1 AND tagger_name IN (`+placeholders+`)`,
			args...,
		)
	}
	if err != nil {
		return err
	}
	var tagIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tagIDs = append(tagIDs, id)
	}
	rows.Close()

	for _, tagID := range tagIDs {
		if err := removeTagFromImageTx(tx, imageID, tagID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) RemoveAllTagsFromImage(imageID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT tag_id FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}
	var tagIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tagIDs = append(tagIDs, id)
	}
	rows.Close()

	if len(tagIDs) > 0 {
		placeholders := strings.Repeat("?,", len(tagIDs))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(tagIDs))
		for i, id := range tagIDs {
			args[i] = id
		}
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id IN (`+placeholders+`)`,
			args...,
		); err != nil {
			return err
		}
		tx.Exec(`DELETE FROM tags WHERE usage_count <= 0 AND id IN (`+placeholders+`)`, args...)
	}

	if _, err := tx.Exec(`DELETE FROM image_tags WHERE image_id = ?`, imageID); err != nil {
		return err
	}

	return tx.Commit()
}

// relatedGeneralTagsCap bounds the general-category portion of the
// probe set so a source carrying `1girl` doesn't drag every image_tags
// row for that tag into the candidate GROUP BY. Non-general non-meta
// categories pass through uncapped because they carry distinguishing
// signal worth the scan even when common.
const relatedGeneralTagsCap = 15

// RelatedImages returns up to limit images that share the most tags
// with imageID, ranked by shared tag count. The source image, missing
// images, and meta-only matches are excluded.
//
// Staged so the images join only runs against the top-N candidates:
// my_tags resolves once (general capped to the K rarest, every other
// category whole); candidates aggregate from image_tags alone with an
// inner LIMIT; the images row is joined last so is_missing filtering
// costs O(buffer). The SELECT only fetches what
// partials/related_images.html actually consumes (currently `.ID`),
// since the rest of the row would be unused payload across the SQL
// boundary.
func (s *Service) RelatedImages(imageID int64, limit int) ([]models.Image, error) {
	rows, err := s.db.Read.Query(
		`WITH my_tags AS (
		     SELECT tag_id FROM (
		         SELECT it.tag_id, tc.name AS cat_name,
		                ROW_NUMBER() OVER (PARTITION BY tc.name
		                                   ORDER BY t.usage_count ASC, t.id ASC) AS rn
		         FROM image_tags it
		         JOIN tags t ON t.id = it.tag_id
		         JOIN tag_categories tc ON tc.id = t.category_id
		         WHERE it.image_id = ? AND tc.name != 'meta'
		     )
		     WHERE cat_name != 'general' OR rn <= ?
		 ),
		 candidates AS (
		     SELECT theirs.image_id, COUNT(*) AS shared
		     FROM image_tags theirs
		     WHERE theirs.tag_id IN (SELECT tag_id FROM my_tags)
		       AND theirs.image_id != ?
		     GROUP BY theirs.image_id
		     ORDER BY shared DESC, theirs.image_id DESC
		     LIMIT ?
		 )
		 SELECT i.id
		 FROM candidates c
		 JOIN images i ON i.id = c.image_id
		 WHERE i.is_missing = 0
		 ORDER BY c.shared DESC, c.image_id DESC
		 LIMIT ?`,
		imageID, relatedGeneralTagsCap, imageID, limit*3+10, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Image
	for rows.Next() {
		var img models.Image
		if err := rows.Scan(&img.ID); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// SuggestTags returns tags matching prefix, sorted by usage_count DESC.
// Two-pass shape: prefix matches first, then substring matches.
func (s *Service) SuggestTags(prefix string, limit int) ([]models.Tag, error) {
	prefixLimit := limit
	rows, err := s.db.Read.Query(
		`SELECT t.id, t.name, tc.name, tc.color, t.usage_count
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE t.name LIKE ? AND t.is_alias = 0
		 ORDER BY t.usage_count DESC
		 LIMIT ?`,
		prefix+"%", prefixLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[int64]struct{}{}
	var result []models.Tag
	for rows.Next() {
		var t models.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
			return nil, err
		}
		seen[t.ID] = struct{}{}
		result = append(result, t)
	}
	rows.Close()

	if len(result) < limit {
		remaining := limit - len(result)
		rows2, err := s.db.Read.Query(
			`SELECT t.id, t.name, tc.name, tc.color, t.usage_count
			 FROM tags t
			 JOIN tag_categories tc ON tc.id = t.category_id
			 WHERE t.name LIKE ? AND t.name NOT LIKE ? AND t.is_alias = 0
			 ORDER BY t.usage_count DESC
			 LIMIT ?`,
			"%"+prefix+"%", prefix+"%", remaining,
		)
		if err != nil {
			return result, nil
		}
		defer rows2.Close()

		for rows2.Next() {
			var t models.Tag
			if err := rows2.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
				return nil, err
			}
			if _, ok := seen[t.ID]; !ok {
				result = append(result, t)
			}
		}
	}

	return result, nil
}

// SuggestTagsInCategory returns tags matching prefix in the named
// category, sorted by usage_count DESC.
func (s *Service) SuggestTagsInCategory(prefix, categoryName string, limit int) ([]models.Tag, error) {
	rows, err := s.db.Read.Query(
		`SELECT t.id, t.name, tc.name, tc.color, t.usage_count
		 FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = ? AND t.name LIKE ? AND t.is_alias = 0
		 ORDER BY t.usage_count DESC
		 LIMIT ?`,
		categoryName, prefix+"%", limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.Tag
	for rows.Next() {
		var t models.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

// DeleteTag removes a tag from every image and drops the tag row. Alias
// rows pointing at it are removed too (their canonical_tag_id would
// otherwise dangle). image_tags rows cascade on the tags FK.
func (s *Service) DeleteTag(id int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM tags WHERE canonical_tag_id = ?`, id); err != nil {
		return fmt.Errorf("delete aliases: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM tags WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete tag: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTagNotFound
	}
	return tx.Commit()
}

// RenameTag renames a tag. The new name must pass validation and must
// not collide with another tag in the same category.
func (s *Service) RenameTag(id int64, newName string) error {
	normalized, err := ValidateTagName(newName)
	if err != nil {
		return err
	}
	var catID int64
	s.db.Read.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&catID)
	var existing int64
	if err := s.db.Read.QueryRow(
		`SELECT id FROM tags WHERE name = ? AND category_id = ? AND id != ?`, normalized, catID, id,
	).Scan(&existing); err == nil {
		return fmt.Errorf("a tag named %q already exists in this category", normalized)
	}
	_, err = s.db.Write.Exec(`UPDATE tags SET name = ? WHERE id = ?`, normalized, id)
	return err
}

// MergeGeneralIntoCategorized scans general-category tags whose name has
// exactly one non-general non-meta counterpart and merges the general
// tag into that counterpart via MergeTags. General tags carrying any
// manual image_tags row are skipped (explicit user intent). Returns the
// number of tags merged.
func (s *Service) MergeGeneralIntoCategorized() (int, error) {
	rows, err := s.db.Read.Query(`
		SELECT g.id, c.id
		FROM tags g
		JOIN tag_categories gc ON gc.id = g.category_id
		JOIN tags c ON c.name = g.name AND c.is_alias = 0 AND c.id != g.id
		JOIN tag_categories cc ON cc.id = c.category_id
		WHERE g.is_alias = 0
		  AND gc.name = 'general'
		  AND cc.name NOT IN ('general', 'meta')
		  AND NOT EXISTS (
		      SELECT 1 FROM image_tags it
		      WHERE it.tag_id = g.id AND it.is_auto = 0
		  )
		  AND (
		      SELECT COUNT(*) FROM tags t2
		      JOIN tag_categories tc2 ON tc2.id = t2.category_id
		      WHERE t2.name = g.name AND t2.is_alias = 0
		        AND tc2.name NOT IN ('general', 'meta')
		  ) = 1`)
	if err != nil {
		return 0, err
	}
	type pair struct{ aliasID, canonID int64 }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.aliasID, &p.canonID); err != nil {
			rows.Close()
			return 0, err
		}
		pairs = append(pairs, p)
	}
	rows.Close()

	merged := 0
	for _, p := range pairs {
		if err := s.MergeTags(p.aliasID, p.canonID); err != nil {
			return merged, err
		}
		merged++
	}
	return merged, nil
}

// ChangeTagCategory moves a tag to a different category. Returns
// ErrTagNotFound, ErrCategoryNotFound, or a clean error when a tag with
// the same name already lives in the target category.
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
	var catExists int
	if err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM tag_categories WHERE id = ?`, newCategoryID,
	).Scan(&catExists); err != nil || catExists == 0 {
		return ErrCategoryNotFound
	}
	// Reject up front so the user gets a clean message rather than the
	// raw UNIQUE(name, category_id) constraint error.
	var existing int64
	if err := s.db.Read.QueryRow(
		`SELECT id FROM tags WHERE name = ? AND category_id = ? AND id != ?`,
		name, newCategoryID, tagID,
	).Scan(&existing); err == nil {
		return fmt.Errorf("a tag named %q already exists in the target category", name)
	}
	_, err := s.db.Write.Exec(`UPDATE tags SET category_id = ? WHERE id = ?`, newCategoryID, tagID)
	return err
}
