package tags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/monbooru/monbooru/internal/db"
)

// image_tags mutation: the add/remove/prune family, the source-sync
// merge, implications fan-in, and the rating prunes.

func (s *Service) AddTagToImage(imageID, tagID int64, isAuto bool, confidence *float64) error {
	_, err := s.AddTagToImageReportingDup(imageID, tagID, isAuto, confidence, "")
	return err
}

// AddTagsToImageFromTagger applies a batch of tag IDs to a single image
// in one write transaction. Per-tag promotion / dup-detection and the
// rating-prune split (manual overwrites, auto keeps highest) are
// preserved so the result matches a serial chain of
// AddTagToImageFromTagger calls, minus the per-tag transaction
// overhead. Used by gallery_merge.go's import path so a record
// carrying dozens of tags doesn't pay one tx per row.
func (s *Service) AddTagsToImageFromTagger(imageID int64, tagIDs []int64, isAuto bool, taggerName string) error {
	return s.AddTagsToImageFromTaggerConf(imageID, tagIDs, nil, isAuto, taggerName)
}

// AddTagsToImageFromTaggerConf is AddTagsToImageFromTagger with a per-tag
// confidence: confs[i] pairs with tagIDs[i], and a nil or missing entry stores
// NULL. The transfer path uses it so an auto-tag keeps the score it was
// assigned.
func (s *Service) AddTagsToImageFromTaggerConf(imageID int64, tagIDs []int64, confs []*float64, isAuto bool, taggerName string) error {
	if len(tagIDs) == 0 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		_, err := addTagsTx(tx, imageID, tagIDs, confs, isAuto, taggerName, s.ratingCatID)
		return err
	})
}

// addTagsTx applies each tag to the image inside tx and reports what
// each add changed. confs pairs positionally with tagIDs; a nil or
// short slice stores NULL.
func addTagsTx(tx *sql.Tx, imageID int64, tagIDs []int64, confs []*float64, isAuto bool, via string, ratingCatID int64) ([]AddResult, error) {
	results := make([]AddResult, 0, len(tagIDs))
	for i, tagID := range tagIDs {
		var c *float64
		if i < len(confs) {
			c = confs[i]
		}
		added, promoted, displaced, err := addOneTagTx(tx, imageID, tagID, isAuto, c, via, ratingCatID)
		if err != nil {
			return results, err
		}
		results = append(results, AddResult{Added: added, Promoted: promoted, DisplacedRatings: displaced})
	}
	return results, nil
}

// addOneTagTx runs one insert-or-promote plus the rating prune the pair
// always travels with, reporting the flags and any displaced rating names.
func addOneTagTx(tx *sql.Tx, imageID, tagID int64, isAuto bool, confidence *float64, taggerName string, ratingCatID int64) (added, promoted bool, displaced []string, err error) {
	added, promoted, err = addTagToImageTxReportingDup(tx, imageID, tagID, isAuto, confidence, taggerName, ratingCatID)
	if err != nil {
		return false, false, nil, err
	}
	if added || promoted {
		if displaced, err = pruneRatingsAfterAddTx(tx, ratingCatID, imageID, tagID, isAuto); err != nil {
			return false, false, nil, err
		}
	}
	return added, promoted, displaced, nil
}

// AddResult bundles the dup-tracking and rating-overwrite signals so
// callers can surface inline diagnostics without a second query. Added
// reports a brand-new image_tags row; Promoted reports an existing
// implied row flipped to user-owned. DisplacedRatings carries the names
// of rating rows the manual add swept off the image (empty for non-
// rating adds and for the auto-tagger path).
type AddResult struct {
	Added            bool
	Promoted         bool
	DisplacedRatings []string
}

// AddTagToImageReportingDup runs INSERT OR IGNORE inside a write-pool
// transaction. Returns an AddResult describing what changed.
func (s *Service) AddTagToImageReportingDup(imageID, tagID int64, isAuto bool, confidence *float64, taggerName string) (AddResult, error) {
	var res AddResult
	err := s.inWriteTx(func(tx *sql.Tx) error {
		out, err := addTagsTx(tx, imageID, []int64{tagID}, []*float64{confidence}, isAuto, taggerName, s.ratingCatID)
		if err != nil {
			return err
		}
		res = out[0]
		return nil
	})
	if err != nil {
		return AddResult{}, err
	}
	return res, nil
}

func addTagToImageTxReportingDup(tx *sql.Tx, imageID, tagID int64, isAuto bool, confidence *float64, taggerName string, ratingCatID int64) (bool, bool, error) {
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
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, ?, 0, ?, ?)`,
		imageID, tagID, isAutoInt, confidence, tname,
	)
	if err != nil {
		return false, false, fmt.Errorf("inserting image_tag: %w", err)
	}
	added, _ := res.RowsAffected()
	var promoted int64
	if added == 0 {
		// Row already present. Promote when the new add carries more
		// authority than the existing row: an implication-side row
		// becomes user-owned so removing a parent later won't sweep it
		// out, and an auto-tagger row gets re-stamped as user-owned
		// when the operator manually re-adds the same tag (manual >
		// auto). Auto adds never demote a user-owned row.
		upd, err := tx.Exec(
			`UPDATE image_tags SET is_implied = 0, is_auto = ?, confidence = ?, tagger_name = ?
			 WHERE image_id = ? AND tag_id = ?
			   AND (is_implied = 1 OR (is_auto = 1 AND ? = 0))`,
			isAutoInt, confidence, tname, imageID, tagID, isAutoInt,
		)
		if err != nil {
			return false, false, err
		}
		promoted, _ = upd.RowsAffected()
	} else {
		if err := BumpTagUsageTx(tx, tagID, imageID); err != nil {
			return false, false, err
		}
	}

	// A named source re-confirming an existing tag is what the ledger
	// exists to capture; a bare UI re-add of a tag already present is a
	// no-op and must not stamp a phantom 'user' source.
	if added > 0 || promoted > 0 || taggerName != "" {
		if err := RecordTagSourceTx(tx, imageID, tagID, taggerName); err != nil {
			return false, false, err
		}
	}

	if err := fanOutImpliedTxImpl(tx, imageID, tagID, ratingCatID, isAutoInt); err != nil {
		return false, false, err
	}

	if added == 0 {
		return false, promoted > 0, nil
	}
	return true, false, nil
}

// TransitiveImpliedTx is the exported entrypoint for callers outside
// the tags package (the implication propagation job in internal/web)
// that need to walk the implication graph inside the same transaction
// they already hold open, so a freshly-added edge is visible to the
// walk.
func TransitiveImpliedTx(tx *sql.Tx, parents []int64) ([]int64, error) {
	return transitiveImpliedTx(tx, parents)
}

// transitiveImpliedTx walks the transitive implied-tag closure of
// parents inside the transaction the caller already holds open, so a
// freshly-added edge is visible.
func transitiveImpliedTx(tx *sql.Tx, parents []int64) ([]int64, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	var out []int64
	err := bfsImpliedTx(tx, parents, func(id int64) bool {
		out = append(out, id)
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddTagsToOneImage adds every tag in tagIDs to imageID inside a single
// write-pool transaction. Mirrors the per-token behaviour of
// AddTagToImageReportingDup (insert-or-promote, fan-out implied closure,
// prune ratings on a manual rating add) and returns one AddResult per
// input id so callers preserve the existing "added / promoted / dupes /
// replaced rating" flash. Used by the detail-page paste path so a
// 50-token paste pays one writer round-trip instead of N. The optional
// via string is recorded as the tagger_name (origin label) on each new
// image_tags row; the UI passes "" so manual adds stay anonymous, the
// REST API passes the caller-supplied source so a scraper can label
// its writes.
func (s *Service) AddTagsToOneImage(imageID int64, tagIDs []int64, via string) ([]AddResult, error) {
	if len(tagIDs) == 0 {
		return nil, nil
	}
	var results []AddResult
	err := s.inWriteTx(func(tx *sql.Tx) error {
		var err error
		results, err = addTagsTx(tx, imageID, tagIDs, nil, false, via, s.ratingCatID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// SyncResult reports what SyncSourceTags changed.
type SyncResult struct {
	Added        int
	Retired      int
	RatingFilled bool
}

// SyncSourceTags reconciles the tags one source contributed to an image (the
// image_tags rows with tagger_name = site, is_auto = 0) against the incoming
// set: incoming tags are added attributed to the site (an existing manual /
// auto / other-source row keeps its own attribution), a re-confirmed row
// sheds its stale flag, and, when reconcile is set, tags the source no
// longer carries are flagged stale rather than removed - the row stays until
// the operator acts. Reconcile is off when the site holds more than one post
// on the image - the slice is shared per site, so one post's fetch must not
// flag its sibling's tags. An incoming rating tag is skipped when the image
// is already rated, so a merge never displaces an existing rating.
func (s *Service) SyncSourceTags(imageID int64, tagIDs []int64, site string, reconcile bool) (SyncResult, error) {
	if site == "" {
		return SyncResult{}, errors.New("source label required")
	}
	incoming := make(map[int64]bool, len(tagIDs))
	for _, id := range tagIDs {
		incoming[id] = true
	}
	var out SyncResult
	err := s.inWriteTx(func(tx *sql.Tx) error {
		alreadyRated := false
		if s.ratingCatID != 0 {
			if err := tx.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM image_tags it JOIN tags t ON t.id = it.tag_id
				 WHERE it.image_id = ? AND t.category_id = ?)`, imageID, s.ratingCatID).Scan(&alreadyRated); err != nil {
				return err
			}
		}

		var res SyncResult
		for _, tagID := range tagIDs {
			isRating := false
			if s.ratingCatID != 0 {
				var cat int64
				switch err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, tagID).Scan(&cat); {
				case errors.Is(err, sql.ErrNoRows):
					continue
				case err != nil:
					return err
				}
				isRating = cat == s.ratingCatID
			}
			if isRating && alreadyRated {
				continue
			}
			added, err := addSourceTagTx(tx, imageID, tagID, site, s.ratingCatID)
			if err != nil {
				return err
			}
			if added {
				res.Added++
				if isRating {
					res.RatingFilled = true
					// A payload carrying more than one rating (a PTR hash with
					// conflicting ratings) must not stack them: the first wins.
					alreadyRated = true
				}
			}
		}

		// A tag the source lists again is current, whatever flagged it before.
		for _, tagID := range tagIDs {
			if _, err := tx.Exec(
				`UPDATE image_tags SET stale = 0
				 WHERE image_id = ? AND tag_id = ? AND tagger_name = ? AND stale = 1`,
				imageID, tagID, site); err != nil {
				return err
			}
		}
		if !reconcile {
			out = res
			return nil
		}
		// Flag this source's own tags no longer in the incoming set, but never a
		// rating: an existing rating is protected, so it is neither overwritten
		// (skipped above) nor flagged here.
		ratingCat := s.ratingCatID
		if ratingCat == 0 {
			ratingCat = -1
		}
		current, err := db.QueryIDs(tx,
			`SELECT it.tag_id FROM image_tags it JOIN tags t ON t.id = it.tag_id
			 WHERE it.image_id = ? AND it.tagger_name = ? AND it.is_auto = 0
			   AND it.stale = 0 AND t.category_id != ?`,
			imageID, site, ratingCat)
		if err != nil {
			return err
		}
		var stale []int64
		for _, tid := range current {
			if !incoming[tid] {
				stale = append(stale, tid)
			}
		}
		for _, tid := range stale {
			if _, err := tx.Exec(
				`UPDATE image_tags SET stale = 1 WHERE image_id = ? AND tag_id = ?`,
				imageID, tid); err != nil {
				return err
			}
			res.Retired++
		}
		out = res
		return nil
	})
	return out, err
}

// addSourceTagTx records one source-contributed tag insert-only: a tag
// already on the image - manual, auto, implied, or from another source -
// keeps its attribution, unlike the promote path a manual re-add takes.
// That is what keeps the sync's slice-bounded prune from ever deleting a
// row the source didn't contribute.
func addSourceTagTx(tx *sql.Tx, imageID, tagID int64, site string, ratingCatID int64) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, 0, 0, NULL, ?)`,
		imageID, tagID, site,
	)
	if err != nil {
		return false, fmt.Errorf("inserting image_tag: %w", err)
	}
	added, _ := res.RowsAffected()
	if added > 0 {
		if err := BumpTagUsageTx(tx, tagID, imageID); err != nil {
			return false, err
		}
	}
	if err := RecordTagSourceTx(tx, imageID, tagID, site); err != nil {
		return false, err
	}
	if err := fanOutImpliedTxImpl(tx, imageID, tagID, ratingCatID, 0); err != nil {
		return false, err
	}
	return added > 0, nil
}

// BatchAddTagsTx applies an add for every (imageID, tagID) pair inside
// the supplied transaction. Mirrors AddTagToImageReportingDup's per-row
// logic (insert-or-promote, fan-out implied closure, prune lower
// ratings on a manual rating add) but without opening N inner
// transactions. Returns the number of (image, tag) pairs that resulted
// in a fresh image_tags row, and the number of images whose existing
// rating the add swept off, so the caller can sum both across chunks -
// a scope-wide re-rating is otherwise invisible next to the row count.
func (s *Service) BatchAddTagsTx(tx *sql.Tx, imageIDs []int64, tagIDs []int64) (added, ratingsReplaced int, err error) {
	for _, imageID := range imageIDs {
		replaced := false
		for _, tagID := range tagIDs {
			a, _, displaced, err := addOneTagTx(tx, imageID, tagID, false, nil, "", s.ratingCatID)
			if err != nil {
				return added, ratingsReplaced, err
			}
			if a {
				added++
			}
			if len(displaced) > 0 {
				replaced = true
			}
		}
		if replaced {
			ratingsReplaced++
		}
	}
	return added, ratingsReplaced, nil
}

// BatchRemoveTagsTx is the remove twin of BatchAddTagsTx: removes each
// (imageID, tagID) pair via removeTagFromImageTx so usage_count and the
// implied closure stay consistent. removed counts the pairs that touched
// an existing row; implied counts the ones left standing because a parent
// on the image still implies them. A batch names tags rather than rows,
// so it reports those instead of failing the whole scope on one.
func (s *Service) BatchRemoveTagsTx(tx *sql.Tx, imageIDs []int64, tagIDs []int64) (removed, implied int, err error) {
	for _, imageID := range imageIDs {
		for _, tagID := range tagIDs {
			before := 0
			if err := tx.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, tagID).Scan(&before); err != nil {
				return removed, implied, err
			}
			if before == 0 {
				continue
			}
			switch isImplied, err := impliedByParentOnImage(tx, imageID, tagID); {
			case err != nil:
				return removed, implied, err
			case isImplied:
				implied++
				continue
			}
			if _, err := removeTagFromImageTx(tx, imageID, tagID); err != nil {
				return removed, implied, err
			}
			removed++
		}
	}
	return removed, implied, nil
}

// RemoveTagFromImage drops one operator-named tag from one image. An
// implied row a parent on the image still justifies is refused with
// ErrTagImplied: the detail sidebar renders no remove button for those,
// and the routes behind it owe the same answer.
func (s *Service) RemoveTagFromImage(imageID, tagID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		switch implied, err := impliedByParentOnImage(tx, imageID, tagID); {
		case err != nil:
			return err
		case implied:
			return ErrTagImplied
		}
		_, err := removeTagFromImageTx(tx, imageID, tagID)
		return err
	})
}

// removeTagIDsFromImageTx removes each tag from imageID inside tx and
// returns how many rows went in total.
func removeTagIDsFromImageTx(tx *sql.Tx, imageID int64, tagIDs []int64) (int, error) {
	removed := 0
	for _, tagID := range tagIDs {
		n, err := removeTagFromImageTx(tx, imageID, tagID)
		removed += n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// RemoveTagsFromOneImage drops every tag in tagIDs from imageID inside
// a single write-pool transaction, mirroring AddTagsToOneImage's batch
// shape. Per-id implied-closure cleanup is preserved; the txn rollback
// covers partial failures so the row's tag state stays consistent.
func (s *Service) RemoveTagsFromOneImage(imageID int64, tagIDs []int64) error {
	if len(tagIDs) == 0 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		for _, tagID := range tagIDs {
			switch implied, err := impliedByParentOnImage(tx, imageID, tagID); {
			case err != nil:
				return err
			case implied:
				return ErrTagImplied
			}
		}
		_, err := removeTagIDsFromImageTx(tx, imageID, tagIDs)
		return err
	})
}

// bumpTagUsageTx increments a tag's usage_count for one image, skipping
// the bump when the image is missing. usage_count tracks visible images
// only (RecalcDB rebuilds it that way), so counting a missing image would
// be silently corrected back down by the next RecalcIDs.
func BumpTagUsageTx(tx *sql.Tx, tagID, imageID int64) error {
	_, err := tx.Exec(
		`UPDATE tags SET usage_count = usage_count + 1,
		        last_used_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
		tagID, imageID,
	)
	return err
}

// DropTagUsageTx is the symmetric decrement: a missing image was never
// counted, so removing its row must not decrement either.
func DropTagUsageTx(tx *sql.Tx, tagID, imageID int64) error {
	_, err := tx.Exec(
		`UPDATE tags SET usage_count = MAX(0, usage_count - 1)
		 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
		tagID, imageID,
	)
	return err
}

// removeTagFromImageTx drops one tag from one image and returns how many
// rows went, the swept implied children included.
func removeTagFromImageTx(tx *sql.Tx, imageID, tagID int64) (int, error) {
	// Walk the parent's implication closure before deleting so we know
	// which implied rows might lose their last justifying parent. The
	// closure only matters when the row being removed is itself a
	// parent in the graph; for ordinary tags the SELECT comes back empty.
	implied, err := transitiveImpliedTx(tx, []int64{tagID})
	if err != nil {
		return 0, err
	}

	res, err := tx.Exec(
		`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, tagID,
	)
	if err != nil {
		return 0, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return 0, nil
	}

	if err := DropTagUsageTx(tx, tagID, imageID); err != nil {
		return 0, err
	}
	removed := 1

	// For every transitively implied tag still sitting on the image as
	// is_implied=1, drop it unless another parent currently on the image
	// still implies it. is_implied=0 rows are user-owned and untouched.
	// A row in the closure can be the only justification for another one,
	// and the closure is walked in an arbitrary order within a level, so
	// sweep until a pass drops nothing.
	for {
		dropped := false
		for _, impID := range implied {
			var rowImplied int
			err := tx.QueryRow(
				`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, impID,
			).Scan(&rowImplied)
			if err == sql.ErrNoRows {
				continue
			} else if err != nil {
				return removed, err
			}
			if rowImplied != 1 {
				continue
			}
			stillImplied, err := implicationParentsOnImageExcluding(tx, imageID, impID, tagID)
			if err != nil {
				return removed, err
			}
			if len(stillImplied) > 0 {
				continue
			}
			if _, err := tx.Exec(
				`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, impID,
			); err != nil {
				return removed, err
			}
			if err := DropTagUsageTx(tx, impID, imageID); err != nil {
				return removed, err
			}
			removed++
			dropped = true
		}
		if !dropped {
			return removed, nil
		}
	}
}

// removeMatchingTx removes the image's tags selected by andWhere - a
// predicate on image_tags, ANDed after the image id - through the
// per-tag closure walk removeTagIDsFromImageTx does.
func (s *Service) removeMatchingTx(imageID int64, andWhere string, args ...any) (int, error) {
	removed := 0
	err := s.inWriteTx(func(tx *sql.Tx) error {
		tagIDs, err := db.QueryIDs(tx,
			`SELECT tag_id FROM image_tags WHERE image_id = ? `+andWhere,
			append([]any{imageID}, args...)...)
		if err != nil {
			return err
		}
		removed, err = removeTagIDsFromImageTx(tx, imageID, tagIDs)
		return err
	})
	return removed, err
}

// RemoveUserTagsFromImage drops the operator's manual tags for one image
// and adjusts usage counts. Manual tags are is_auto = 0, is_implied = 0
// rows with no tagger_name; a source's tags (is_auto = 0 carrying the
// site as tagger_name) are left in place - RemoveSourceTagsFromImage
// handles those. Implied rows carry a NULL tagger_name too but were
// never added by the operator; the closure cleanup sweeps them when
// their parent goes.
func (s *Service) RemoveUserTagsFromImage(imageID int64) (int, error) {
	return s.removeMatchingTx(imageID,
		`AND is_auto = 0 AND is_implied = 0 AND (tagger_name IS NULL OR tagger_name = '')`)
}

// RemoveStaleTagsFromImage drops the image's stale rows - tags a source
// dropped on its last refresh (stale = 1) - adjusting usage counts and the
// implied closure like the other removers.
func (s *Service) RemoveStaleTagsFromImage(imageID int64) (int, error) {
	return s.removeMatchingTx(imageID, `AND stale = 1`)
}

// RemoveSourceTagsFromImage drops the tags one or more external sources
// contributed - is_auto = 0 rows whose tagger_name is a listed site - leaving
// the operator's manual tags and the auto-tagger rows untouched. stale narrows
// to the source's stale ("1") or current ("0") half, matching how the detail
// page splits a source into two groups; "" takes both.
func (s *Service) RemoveSourceTagsFromImage(imageID int64, sources []string, stale string) (int, error) {
	if len(sources) == 0 {
		return 0, nil
	}
	placeholders, args := db.InPlaceholders(sources)
	where := `AND is_auto = 0 AND tagger_name IN (` + placeholders + `)`
	switch stale {
	case "1":
		where += ` AND stale = 1`
	case "0":
		where += ` AND stale = 0`
	}
	return s.removeMatchingTx(imageID, where, args...)
}

// RemoveCategoryTagsFromImage drops the image's own tags in one category.
// Implied rows are left to the closure walk: they belong to whatever
// parent justifies them, which may sit in another category.
func (s *Service) RemoveCategoryTagsFromImage(imageID int64, category string) (int, error) {
	return s.removeMatchingTx(imageID,
		`AND is_implied = 0 AND tag_id IN (
		   SELECT t.id FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		   WHERE tc.name = ?)`, category)
}

// DropSourceFromImageTags withdraws one source's claim on an image's
// tags. Its ledger rows go; a tag no other source still vouches for
// leaves the image with them, while one another source also applied
// stays and merely stops being listed under this source. A non-empty
// tagIDs narrows the withdrawal to those tags.
//
// This is deliberately not RemoveSourceTagsFromImage, which deletes by
// first-writer attribution: a tag two sources agree on belongs to both,
// so one of them backing out cannot take it off the image.
//
// Returns how many tags the source was recorded against and how many of
// them left the image.
func (s *Service) DropSourceFromImageTags(imageID int64, source string, tagIDs []int64) (covered, removed int, err error) {
	err = s.inWriteTx(func(tx *sql.Tx) error {
		// The ledger rows, plus the rows no ledger ever recorded whose own
		// attribution names this source - the fallback the by-source view
		// lists them under, so the withdrawal has to reach them too.
		claim := `SELECT tag_id FROM (
		            SELECT tag_id FROM image_tag_sources
		             WHERE image_id = ? AND source = ?
		            UNION
		            SELECT it.tag_id FROM image_tags it
		             WHERE it.image_id = ? AND it.is_implied = 0
		               AND COALESCE(NULLIF(it.tagger_name, ''), 'user') = ?
		               AND NOT EXISTS (SELECT 1 FROM image_tag_sources s
		                                WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id))`
		args := []any{imageID, source, imageID, source}
		if len(tagIDs) > 0 {
			placeholders, ids := db.InPlaceholders(tagIDs)
			claim += ` WHERE tag_id IN (` + placeholders + `)`
			args = append(args, ids...)
		}
		claimed, err := db.QueryIDs(tx, claim, args...)
		if err != nil {
			return err
		}
		covered = len(claimed)
		if covered == 0 {
			return nil
		}
		placeholders, ids := db.InPlaceholders(claimed)
		if _, err := tx.Exec(
			`DELETE FROM image_tag_sources
			  WHERE image_id = ? AND source = ? AND tag_id IN (`+placeholders+`)`,
			append([]any{imageID, source}, ids...)...); err != nil {
			return err
		}
		orphaned, err := db.QueryIDs(tx,
			`SELECT it.tag_id FROM image_tags it
			  WHERE it.image_id = ? AND it.is_implied = 0 AND it.tag_id IN (`+placeholders+`)
			    AND NOT EXISTS (SELECT 1 FROM image_tag_sources s
			                     WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id)`,
			append([]any{imageID}, ids...)...)
		if err != nil {
			return err
		}
		removed, err = removeTagIDsFromImageTx(tx, imageID, orphaned)
		return err
	})
	return covered, removed, err
}

// RemoveAutoTagsFromImage drops auto-tagged image_tags rows for one
// image. A non-empty taggerNames restricts the deletion to rows whose
// tagger_name matches.
func (s *Service) RemoveAutoTagsFromImage(imageID int64, taggerNames []string) (int, error) {
	if len(taggerNames) == 0 {
		return s.removeMatchingTx(imageID, `AND is_auto = 1`)
	}
	placeholders, args := db.InPlaceholders(taggerNames)
	return s.removeMatchingTx(imageID, `AND is_auto = 1 AND tagger_name IN (`+placeholders+`)`, args...)
}

// PruneOrphanedImplied drops the implied rows on the given images whose last
// justifying parent is gone, for the bulk paths that delete image_tags rows
// with one predicate instead of walking each tag's closure. Repeats per chunk
// because a dropped row can be the only justification for another. Returns
// the tags whose row count changed and how many rows went; usage_count is
// left to the caller's RecalcIDs.
func (s *Service) PruneOrphanedImplied(ctx context.Context, imageIDs []int64) ([]int64, int, error) {
	seen := map[int64]struct{}{}
	removed := 0
	err := db.Chunked(imageIDs, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.inWriteTx(func(tx *sql.Tx) error {
			placeholders, args := db.InPlaceholders(chunk)
			type orphan struct{ imageID, tagID int64 }
			for {
				orphans, err := db.QueryAll(tx, func(rows *sql.Rows) (orphan, error) {
					var o orphan
					err := rows.Scan(&o.imageID, &o.tagID)
					return o, err
				}, `SELECT it.image_id, it.tag_id FROM image_tags it
					 WHERE it.is_implied = 1 AND it.image_id IN (`+placeholders+`)
					   AND NOT EXISTS (SELECT 1 FROM tag_implications ti
					                   JOIN image_tags p ON p.image_id = it.image_id AND p.tag_id = ti.parent_tag_id
					                   WHERE ti.implied_tag_id = it.tag_id)`, args...)
				if err != nil {
					return err
				}
				if len(orphans) == 0 {
					return nil
				}
				for _, o := range orphans {
					if _, err := tx.Exec(
						`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, o.imageID, o.tagID,
					); err != nil {
						return err
					}
					seen[o.tagID] = struct{}{}
					removed++
				}
			}
		})
	})
	if err != nil {
		return tagIDsFromSet(seen), removed, err
	}
	return tagIDsFromSet(seen), removed, nil
}

func (s *Service) RemoveAllTagsFromImage(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return RemoveAllTagsFromImageTx(tx, imageID) })
}

// RemoveAllTagsFromImageTx is RemoveAllTagsFromImage on a caller-held
// transaction, so the image-delete path can commit the tag drop, the
// relations cleanup and the row delete together.
func RemoveAllTagsFromImageTx(tx *sql.Tx, imageID int64) error {
	tagIDs, err := db.QueryIDs(tx, `SELECT tag_id FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}

	if len(tagIDs) > 0 {
		// Skip the bulk decrement when the image was missing: its rows
		// were never counted in usage_count to begin with.
		var isMissing int
		if err := tx.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, imageID).Scan(&isMissing); err != nil && err != sql.ErrNoRows {
			return err
		}
		if isMissing == 0 {
			placeholders, args := db.InPlaceholders(tagIDs)
			if _, err := tx.Exec(
				`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id IN (`+placeholders+`)`,
				args...,
			); err != nil {
				return err
			}
		}
	}

	_, err = tx.Exec(`DELETE FROM image_tags WHERE image_id = ?`, imageID)
	return err
}

// relatedGeneralTagsCap bounds the general-category portion of the
// probe set so a source carrying `1girl` doesn't drag every image_tags
// row for that tag into the candidate GROUP BY. Non-general non-meta
// categories pass through uncapped because they carry distinguishing
// signal worth the scan even when common.
const relatedGeneralTagsCap = 15

// RatingRank returns the position of name in RatingLevels (0-indexed,
// general < sensitive < questionable < explicit). Returns -1 for any
// non-canonical name.
func RatingRank(name string) int {
	return slices.Index(RatingLevels, name)
}

// PruneLowerRatingsTx keeps only the highest-rank rating tag on imageID.
// When the image carries multiple rating-category rows (general <
// sensitive < questionable < explicit) the lower-rank rows are removed
// via removeTagFromImageTx so usage_count adjustment and the implied
// closure cleanup match the rest of the tag-removal path. Idempotent:
// after the call the image carries at most one rating tag.
//
// Both the manual add path (AddTagToImageReportingDup) and the auto-
// tagger's storeResults call this so highest-rank-wins is the durable
// invariant a fresh write upholds. fastCountCeiling and fastCountRating
// rely on the invariant for their constant-time bounds.
//
// ratingCatID is the rating category id; pass 0 to skip (only possible
// against a pre-bootstrap DB, where the four canonical rating rows
// don't yet exist).
func PruneLowerRatingsTx(tx *sql.Tx, ratingCatID, imageID int64) error {
	return pruneLowerRatingsTx(tx, ratingCatID, imageID)
}

// pruneRatingsAfterAddTx enforces the one-rating-per-image rule after a
// rating tag is added. The rule splits on origin: a manual add overwrites
// whatever rating was there so the user's chosen level always wins (even
// when it ranks below a pre-existing auto-tagger value), returning the
// names it swept off; an auto-tagger add keeps the highest rank so a
// single inference emitting `sensitive` and `questionable` resolves the
// way search does. No-ops when the rating category is unset or tagID is
// not a rating tag; the PK lookup is cheap and the prune is a no-op on an
// image carrying 0 or 1 rating tags.
func pruneRatingsAfterAddTx(tx *sql.Tx, ratingCatID, imageID, tagID int64, isAuto bool) ([]string, error) {
	if ratingCatID == 0 {
		return nil, nil
	}
	var catID int64
	if err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, tagID).Scan(&catID); err != nil || catID != ratingCatID {
		return nil, nil
	}
	if isAuto {
		return nil, pruneLowerRatingsTx(tx, ratingCatID, imageID)
	}
	return pruneOtherRatingsTx(tx, ratingCatID, imageID, tagID)
}

type ratingRow struct {
	tagID int64
	name  string
}

// ratingRowsOnImageTx returns the rating-category rows on imageID.
func ratingRowsOnImageTx(tx *sql.Tx, ratingCatID, imageID int64) ([]ratingRow, error) {
	return db.QueryAll(tx, func(rows *sql.Rows) (ratingRow, error) {
		var r ratingRow
		err := rows.Scan(&r.tagID, &r.name)
		return r, err
	}, `SELECT it.tag_id, t.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.category_id = ? AND t.is_alias = 0`,
		imageID, ratingCatID)
}

func pruneLowerRatingsTx(tx *sql.Tx, ratingCatID, imageID int64) error {
	if ratingCatID == 0 {
		return nil
	}
	present, err := ratingRowsOnImageTx(tx, ratingCatID, imageID)
	if err != nil {
		return fmt.Errorf("scan rating rows for prune: %w", err)
	}
	if len(present) <= 1 {
		return nil
	}
	bestRank := -1
	for _, r := range present {
		bestRank = max(bestRank, RatingRank(r.name))
	}
	_, err = pruneRatingsTx(tx, imageID, present, func(r ratingRow) bool {
		return RatingRank(r.name) >= bestRank
	})
	if err != nil {
		return fmt.Errorf("prune lower rating: %w", err)
	}
	return nil
}

// pruneRatingsTx removes every rating row on the image that keep
// rejects, returning the names it took off. removeTagFromImageTx does
// the usage-count and implied-closure work per row.
func pruneRatingsTx(tx *sql.Tx, imageID int64, present []ratingRow, keep func(ratingRow) bool) ([]string, error) {
	var displaced []string
	for _, r := range present {
		if keep(r) {
			continue
		}
		if _, err := removeTagFromImageTx(tx, imageID, r.tagID); err != nil {
			return displaced, fmt.Errorf("remove rating %d: %w", r.tagID, err)
		}
		displaced = append(displaced, r.name)
	}
	return displaced, nil
}

// pruneOtherRatingsTx is the manual-add twin of pruneLowerRatingsTx:
// it keeps only keepTagID and sweeps every other rating row off the
// image so the user's just-typed rating always wins, even when its
// rank is below an existing auto-tagger value. Mirrors the prune
// shape so the usage_count decrements still flow through
// removeTagFromImageTx. Returns the displaced tag names so the caller
// can surface "replaced rating:general" in a flash.
func pruneOtherRatingsTx(tx *sql.Tx, ratingCatID, imageID, keepTagID int64) ([]string, error) {
	if ratingCatID == 0 {
		return nil, nil
	}
	present, err := ratingRowsOnImageTx(tx, ratingCatID, imageID)
	if err != nil {
		return nil, fmt.Errorf("scan rating rows for overwrite: %w", err)
	}
	displaced, err := pruneRatingsTx(tx, imageID, present, func(r ratingRow) bool {
		return r.tagID == keepTagID
	})
	if err != nil {
		return nil, fmt.Errorf("overwrite prior rating: %w", err)
	}
	return displaced, nil
}

// RatingTagIDsAbove returns the canonical rating tag ids whose level
// ranks strictly above ceiling (e.g. ceiling="sensitive" returns the ids
// of "questionable" and "explicit"). An empty or unknown ceiling, or
// "explicit" (the no-ceiling sentinel), returns nil. The lookup runs a
// fresh SELECT each call so a tag pruned and re-created at runtime is
// resolved to its current id.
func (s *Service) RatingTagIDsAbove(ceiling string) []int64 {
	if s.ratingCatID == 0 {
		return nil
	}
	rank := RatingRank(ceiling)
	if rank < 0 || rank >= len(RatingLevels)-1 {
		return nil
	}
	above := RatingLevels[rank+1:]
	placeholders, nameArgs := db.InPlaceholders(above)
	args := append([]any{s.ratingCatID}, nameArgs...)
	ids, err := db.QueryIDs(s.db.Read,
		`SELECT id FROM tags WHERE category_id = ? AND name IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil
	}
	return ids
}
