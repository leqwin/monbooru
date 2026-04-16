package tags

import (
	"database/sql"
	"fmt"
)

// MergeTags makes aliasID an alias of canonicalID.
// It moves all image_tags from alias to canonical and marks the alias.
func (s *Service) MergeTags(aliasID, canonicalID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// a. Move image_tags from aliasID to canonicalID (handle conflicts).
	//    Find images that have aliasID but not canonicalID — insert canonical.
	rows, err := tx.Query(
		`SELECT image_id, is_auto, confidence, tagger_name FROM image_tags WHERE tag_id = ?`, aliasID,
	)
	if err != nil {
		return err
	}
	type imageTagRow struct {
		imageID    int64
		isAuto     int
		confidence sql.NullFloat64
		taggerName sql.NullString
	}
	var aliasTags []imageTagRow
	for rows.Next() {
		var r imageTagRow
		if err := rows.Scan(&r.imageID, &r.isAuto, &r.confidence, &r.taggerName); err != nil {
			rows.Close()
			return err
		}
		aliasTags = append(aliasTags, r)
	}
	rows.Close()

	for _, at := range aliasTags {
		// Check if canonical tag already on this image
		var existingIsAuto int
		err := tx.QueryRow(
			`SELECT is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`,
			at.imageID, canonicalID,
		).Scan(&existingIsAuto)

		if err == sql.ErrNoRows {
			// Not there yet — insert canonical tag, carrying auto-tagger attribution over.
			if _, err := tx.Exec(
				`INSERT INTO image_tags (image_id, tag_id, is_auto, confidence, tagger_name)
				 VALUES (?, ?, ?, ?, ?)`,
				at.imageID, canonicalID, at.isAuto, at.confidence, at.taggerName,
			); err != nil {
				return fmt.Errorf("inserting canonical tag for image %d: %w", at.imageID, err)
			}
		} else if err != nil {
			return err
		}
		// If canonical already there, nothing to do (keep the existing canonical row)

		// Remove the alias tag from this image
		if _, err := tx.Exec(
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`,
			at.imageID, aliasID,
		); err != nil {
			return err
		}
	}

	// b. Mark aliasID as alias
	if _, err := tx.Exec(
		`UPDATE tags SET is_alias = 1, canonical_tag_id = ?, usage_count = 0 WHERE id = ?`,
		canonicalID, aliasID,
	); err != nil {
		return err
	}

	// c. Recompute usage_count for canonicalID
	if _, err := tx.Exec(
		`UPDATE tags SET usage_count = (
			SELECT COUNT(*) FROM image_tags WHERE tag_id = ?
		) WHERE id = ?`,
		canonicalID, canonicalID,
	); err != nil {
		return err
	}

	return tx.Commit()
}
