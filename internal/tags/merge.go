package tags

import (
	"database/sql"
	"fmt"
)

// MergeTags makes aliasID an alias of canonicalID, moving all
// image_tags rows from alias to canonical and marking the alias row.
func (s *Service) MergeTags(aliasID, canonicalID int64) error {
	if aliasID == canonicalID {
		return fmt.Errorf("cannot merge a tag into itself")
	}

	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Refuse merge-into-alias: the alias resolver only follows one hop
	// (COALESCE(canonical_tag_id, id) and GetOrCreateTag's single lookup),
	// so a two-hop chain would silently drop rows.
	var targetIsAlias int
	if err := tx.QueryRow(`SELECT is_alias FROM tags WHERE id = ?`, canonicalID).Scan(&targetIsAlias); err != nil {
		return fmt.Errorf("target tag not found")
	}
	if targetIsAlias == 1 {
		return fmt.Errorf("cannot merge into a tag that is itself an alias")
	}

	// (a) Move image_tags from alias to canonical, skipping images that
	// already carry the canonical.
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
		var existingIsAuto int
		err := tx.QueryRow(
			`SELECT is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`,
			at.imageID, canonicalID,
		).Scan(&existingIsAuto)

		if err == sql.ErrNoRows {
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

		if _, err := tx.Exec(
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`,
			at.imageID, aliasID,
		); err != nil {
			return err
		}
	}

	// (b) Mark aliasID as an alias of canonicalID.
	if _, err := tx.Exec(
		`UPDATE tags SET is_alias = 1, canonical_tag_id = ?, usage_count = 0 WHERE id = ?`,
		canonicalID, aliasID,
	); err != nil {
		return err
	}

	// (c) Recount canonical's usage_count from non-missing images, the
	// same convention RecalcAndPruneDB enforces, so a merge doesn't
	// inflate the count past what the next recalc would emit.
	if _, err := tx.Exec(
		`UPDATE tags SET usage_count = (
			SELECT COUNT(*) FROM image_tags it
			JOIN images i ON i.id = it.image_id
			WHERE it.tag_id = ? AND i.is_missing = 0
		) WHERE id = ?`,
		canonicalID, canonicalID,
	); err != nil {
		return err
	}

	return tx.Commit()
}
