package tags

import (
	"database/sql"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// Tag suggestion and the related-images panel.

// relatedMaxTagUsage drops seed tags whose global usage_count exceeds
// the cap. A tag carried by more than this many images doesn't add
// discriminative signal - the candidate set it brings in is mostly
// noise - and on a 1M-image library it's the difference between a 2 s
// GROUP BY and a sub-second one. Seed images whose every tag is
// over-cap render an empty related panel rather than a slow one.
const relatedMaxTagUsage = 10000

// RelatedImages returns up to limit images that share the most tags
// with imageID, ranked by shared tag count. The source image, missing
// images, and meta-only matches are excluded.
//
// Staged so the images join only runs against the top-N candidates:
// my_tags resolves once (general capped to the K rarest, popular tags
// dropped via relatedMaxTagUsage so the candidate scan stays bounded);
// candidates aggregate from image_tags alone with an inner LIMIT; the
// images row is joined last so is_missing filtering costs O(buffer).
// The SELECT carries id + file_type + page_count so the related-images
// partial can render the manga-pill ("N pages") on cbz candidates the
// same way the gallery grid does.
//
// Type partition: a manga source (file_type='cbz') only surfaces
// other manga; non-manga sources only surface non-manga (regular
// images and animated). The split keeps "Similar entries" coherent -
// the user navigating in a manga shouldn't get bounced into a regular
// image grid and vice versa.
//
// ratingCeiling, when non-empty, drops candidates carrying any rating
// tag above the ceiling level (highest-wins). Pass "" or "explicit" to
// disable the filter.
func (s *Service) RelatedImages(imageID int64, limit int, ratingCeiling string) ([]models.Image, error) {
	excluded := s.RatingTagIDsAbove(ratingCeiling)

	var sourceFileType string
	if err := s.db.Read.QueryRow(`SELECT file_type FROM images WHERE id = ?`, imageID).Scan(&sourceFileType); err != nil {
		return nil, err
	}
	typePredicate := "i.file_type = 'cbz'"
	if sourceFileType != "cbz" {
		typePredicate = "i.file_type != 'cbz'"
	}

	candidatesExtra := ""
	args := []any{imageID, relatedMaxTagUsage, relatedGeneralTagsCap, imageID}
	if len(excluded) > 0 {
		placeholders, excludedArgs := db.InPlaceholders(excluded)
		candidatesExtra = ` AND NOT EXISTS (
		         SELECT 1 FROM image_tags x
		         WHERE x.image_id = theirs.image_id
		           AND x.tag_id IN (` + placeholders + `)
		     )`
		args = append(args, excludedArgs...)
	}
	args = append(args, limit*2+5, limit)

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
		           AND t.usage_count <= ?
		     )
		     WHERE cat_name != 'general' OR rn <= ?
		 ),
		 candidates AS (
		     SELECT theirs.image_id, COUNT(*) AS shared
		     FROM image_tags theirs
		     WHERE theirs.tag_id IN (SELECT tag_id FROM my_tags)
		       AND theirs.image_id != ?`+candidatesExtra+`
		     GROUP BY theirs.image_id
		     ORDER BY shared DESC, theirs.image_id DESC
		     LIMIT ?
		 )
		 SELECT i.id, i.file_type, i.page_count
		 FROM candidates c
		 JOIN images i ON i.id = c.image_id
		 WHERE i.is_missing = 0 AND `+typePredicate+`
		 ORDER BY c.shared DESC, c.image_id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []models.Image
	for rows.Next() {
		var img models.Image
		var pageCount *int
		if err := rows.Scan(&img.ID, &img.FileType, &pageCount); err != nil {
			return nil, err
		}
		img.PageCount = pageCount
		out = append(out, img)
	}
	return out, rows.Err()
}

// SuggestTags returns tags matching prefix, sorted by usage_count DESC.
// Two-pass shape: prefix matches first, then substring matches.
func (s *Service) SuggestTags(prefix string, limit int) ([]models.Tag, error) {
	return suggestUsageRanked(s.db, prefix, "", false, limit)
}

// SuggestUsageRanked is the exported entry point for callers outside
// the tags package (the no-context fast path of search.SuggestTagsWithFilter).
// requireUsage gates the `usage_count > 0` filter so the search-bar
// autocomplete hides zero-usage tags while the detail-page tag input
// (where freshly-declared tags must surface immediately) keeps them.
func SuggestUsageRanked(database *db.DB, prefix, categoryName string, requireUsage bool, limit int) ([]models.Tag, error) {
	return suggestUsageRanked(database, prefix, categoryName, requireUsage, limit)
}

// suggestUsageRanked is the shared two-pass prefix→substring helper:
// prefix matches first (sorted by usage_count DESC), then substring
// matches that aren't already in the prefix set, until limit is hit.
// categoryName, when non-empty, scopes both passes to that category;
// requireUsage adds `usage_count > 0`.
func suggestUsageRanked(database *db.DB, prefix, categoryName string, requireUsage bool, limit int) ([]models.Tag, error) {
	prefix = db.EscapeLike(NormalizeTagName(prefix))
	// The ranked pick runs against tags alone so it can ride
	// idx_tags_active_usage and stop at the limit. With the category
	// join in the same SELECT the planner drives from tag_categories
	// instead, fetches every tag row through idx_tags_category and
	// temp-sorts the lot to hand back ten.
	baseSQL := `SELECT t.id, t.name, tc.name, tc.color, t.usage_count
	            FROM (SELECT id, name, category_id, usage_count
	                  FROM tags
	                  WHERE is_alias = 0
	                    %s
	                    AND name LIKE ? ESCAPE '\'
	                    %s
	                  ORDER BY usage_count DESC, name ASC
	                  LIMIT ?) t
	            JOIN tag_categories tc ON tc.id = t.category_id
	            ORDER BY t.usage_count DESC, t.name ASC`
	usageClause := ""
	if requireUsage {
		usageClause = "AND usage_count > 0"
	}
	catClause := ""
	var catArgs []any
	if categoryName != "" {
		catClause = "AND category_id = (SELECT id FROM tag_categories WHERE name = ?)"
		catArgs = []any{categoryName}
	}

	run := func(pat string, prior []models.Tag, remaining int, nameNotLike string) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 2+len(catArgs))
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if nameNotLike != "" {
			extra = extra + ` AND name NOT LIKE ? ESCAPE '\'`
			qargs = append(qargs, nameNotLike)
		}
		qargs = append(qargs, remaining)
		scanned, err := db.QueryAll(database.Read, ScanTag, fmt.Sprintf(baseSQL, usageClause, extra), qargs...)
		if err != nil {
			return prior, err
		}
		seen := map[int64]bool{}
		for _, t := range prior {
			seen[t.ID] = true
		}
		for _, t := range scanned {
			if seen[t.ID] {
				continue
			}
			prior = append(prior, t)
			seen[t.ID] = true
		}
		return prior, nil
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

// SuggestTagsInCategory returns tags matching prefix in the named
// category, sorted by usage_count DESC.
func (s *Service) SuggestTagsInCategory(prefix, categoryName string, limit int) ([]models.Tag, error) {
	return db.QueryAll(s.db.Read, ScanTag,
		`SELECT t.id, t.name, tc.name, tc.color, t.usage_count
		 FROM (SELECT id, name, category_id, usage_count
		       FROM tags
		       WHERE category_id = (SELECT id FROM tag_categories WHERE name = ?)
		         AND name LIKE ? ESCAPE '\' AND is_alias = 0
		       ORDER BY usage_count DESC
		       LIMIT ?) t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 ORDER BY t.usage_count DESC`,
		categoryName, db.EscapeLike(NormalizeTagName(prefix))+"%", limit)
}

// ScanTag reads one row of the five-column tag projection (id, name,
// category name, color, usage_count) that every tag listing selects.
func ScanTag(rows *sql.Rows) (models.Tag, error) {
	var t models.Tag
	err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount)
	return t, err
}
