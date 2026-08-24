package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// MaxImplicationDepth bounds transitive closure walks. Real-world booru
// implication graphs sit well under ten levels; the cap is a runtime
// belt-and-braces guard so a future cycle that slipped past the
// create-time check can't spin forever.
const MaxImplicationDepth = 16

// ErrImplicationCycle is returned when AddImplication would create a
// path from ImpliedID back to ParentID through the existing graph.
var ErrImplicationCycle = errors.New("implication would form a cycle")

// scanImplications collects the rows of an implication SELECT. The
// implied-side display columns are only present on the parent-side
// listing; withImpliedCols says whether to expect them.
func scanImplications(rows *sql.Rows, withImpliedCols bool) ([]models.Implication, error) {
	var out []models.Implication
	for rows.Next() {
		var im models.Implication
		var created string
		var stale int64
		cols := []any{
			&im.ParentID, &im.ImpliedID,
			&im.ParentName, &im.ParentCategoryName, &im.ParentCategoryColor,
		}
		if withImpliedCols {
			cols = append(cols, &im.ImpliedName, &im.ImpliedCategoryName, &im.ImpliedCategoryColor)
		}
		cols = append(cols, &created, &im.Origin, &stale)
		if err := rows.Scan(cols...); err != nil {
			return nil, err
		}
		im.CreatedAt, _ = time.Parse(time.RFC3339, created)
		im.Stale = stale == 1
		out = append(out, im)
	}
	return out, rows.Err()
}

// ListImplications returns every direct implication whose parent is
// parentID, with display fields joined for the /tags dialog.
func (s *Service) ListImplications(parentID int64) ([]models.Implication, error) {
	return s.implicationsQuery(
		`SELECT ti.parent_tag_id, ti.implied_tag_id,
		        p.name, pc.name, pc.color,
		        i.name, ic.name, ic.color,
		        ti.created_at, ti.origin, ti.stale
		 FROM tag_implications ti
		 JOIN tags p ON p.id = ti.parent_tag_id
		 JOIN tag_categories pc ON pc.id = p.category_id
		 JOIN tags i ON i.id = ti.implied_tag_id
		 JOIN tag_categories ic ON ic.id = i.category_id
		 WHERE ti.parent_tag_id = ?
		 ORDER BY i.name`, parentID, true)
}

// implicationsQuery runs one edge listing and owns its cursor. full says
// whether the row set carries the implied side's display fields too,
// which is the only thing the two listings differ in beyond their SQL.
func (s *Service) implicationsQuery(query string, arg int64, full bool) ([]models.Implication, error) {
	rows, err := s.db.Read.Query(query, arg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanImplications(rows, full)
}

// ImpliedBy returns the direct edges whose implied side is tagID, with
// parent display fields joined for the detail page's reverse view.
func (s *Service) ImpliedBy(tagID int64) ([]models.Implication, error) {
	return s.implicationsQuery(
		`SELECT ti.parent_tag_id, ti.implied_tag_id,
		        p.name, pc.name, pc.color,
		        ti.created_at, ti.origin, ti.stale
		 FROM tag_implications ti
		 JOIN tags p ON p.id = ti.parent_tag_id
		 JOIN tag_categories pc ON pc.id = p.category_id
		 WHERE ti.implied_tag_id = ?
		 ORDER BY p.name`, tagID, false)
}

// SyncImplicationStaleness mirrors SyncAliasStaleness for a parent's
// origin-attributed implication edges, keyed by implied tag id: edges
// absent from fresh are flagged stale, edges listed again are cleared.
// Returns how many edges were newly flagged.
func (s *Service) SyncImplicationStaleness(parentID int64, origin string, fresh map[int64]bool) (int, error) {
	flagged := 0
	err := s.inWriteTx(func(tx *sql.Tx) error {
		type edgeRow struct{ impliedID, stale int64 }
		present, err := db.QueryAll(tx, func(rows *sql.Rows) (edgeRow, error) {
			var e edgeRow
			err := rows.Scan(&e.impliedID, &e.stale)
			return e, err
		}, `SELECT implied_tag_id, stale FROM tag_implications
			 WHERE parent_tag_id = ? AND origin = ?`, parentID, origin)
		if err != nil {
			return err
		}
		var flag, clear []int64
		for _, e := range present {
			switch current := fresh[e.impliedID]; {
			case !current && e.stale == 0:
				flag = append(flag, e.impliedID)
			case current && e.stale == 1:
				clear = append(clear, e.impliedID)
			}
		}
		if err := setImplicationsStaleTx(tx, parentID, flag, 1); err != nil {
			return err
		}
		if err := setImplicationsStaleTx(tx, parentID, clear, 0); err != nil {
			return err
		}
		flagged = len(flag)
		return nil
	})
	return flagged, err
}

func setImplicationsStaleTx(tx *sql.Tx, parentID int64, impliedIDs []int64, stale int) error {
	return setStaleTx(tx, "tag_implications", "implied_tag_id",
		`parent_tag_id = `+strconv.FormatInt(parentID, 10)+` AND `, impliedIDs, stale)
}

// ImplicationsForParents returns the direct implications keyed by
// parent id, with display fields joined for the /tags listing. One
// query per call regardless of input size (chunked at the SQLite
// parameter cap).
func (s *Service) ImplicationsForParents(parentIDs []int64) (map[int64][]models.Implication, error) {
	out := make(map[int64][]models.Implication, len(parentIDs))
	if len(parentIDs) == 0 {
		return out, nil
	}
	err := db.Chunked(parentIDs, 500, func(batch []int64) error {
		placeholders, args := db.InPlaceholders(batch)
		imps, err := db.QueryAll(s.db.Read, func(rows *sql.Rows) (models.Implication, error) {
			var im models.Implication
			err := rows.Scan(
				&im.ParentID, &im.ImpliedID,
				&im.ImpliedName, &im.ImpliedCategoryName, &im.ImpliedCategoryColor,
			)
			return im, err
		},
			`SELECT ti.parent_tag_id, ti.implied_tag_id,
			        i.name, ic.name, ic.color
			 FROM tag_implications ti
			 JOIN tags i ON i.id = ti.implied_tag_id
			 JOIN tag_categories ic ON ic.id = i.category_id
			 WHERE ti.parent_tag_id IN (`+placeholders+`)
			 ORDER BY i.name`,
			args...)
		if err != nil {
			return err
		}
		for _, im := range imps {
			out[im.ParentID] = append(out[im.ParentID], im)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddImplication declares parent -> implied. Refuses self-implication,
// alias rows on either side (alias resolution is name-only and would
// silently bypass the link), and any edge that would close a cycle
// through the existing graph. Rating tags are allowed on either side
// because the implication graph doesn't mutate the rating vocabulary -
// the tag row itself is still immutable. The returned bool reports
// whether the row was new; false means the edge already existed.
func (s *Service) AddImplication(parentID, impliedID int64) (bool, error) {
	return s.AddImplicationFrom(parentID, impliedID, "user")
}

// AddImplicationFrom is AddImplication with an explicit creation origin,
// stamped only when the edge is actually inserted.
func (s *Service) AddImplicationFrom(parentID, impliedID int64, origin string) (bool, error) {
	if parentID == impliedID {
		return false, fmt.Errorf("cannot imply a tag from itself")
	}

	var created bool
	err := s.inWriteTx(func(tx *sql.Tx) error {
		for _, id := range [2]int64{parentID, impliedID} {
			var isAlias int
			if err := tx.QueryRow(`SELECT is_alias FROM tags WHERE id = ?`, id).Scan(&isAlias); err == sql.ErrNoRows {
				return ErrTagNotFound
			} else if err != nil {
				return err
			}
			if isAlias == 1 {
				return fmt.Errorf("cannot involve an alias in an implication; use its canonical")
			}
		}

		// Cycle check: walk the existing graph from impliedID; if we reach
		// parentID, the new edge closes a loop.
		if reaches, err := implicationReachesTx(tx, impliedID, parentID); err != nil {
			return err
		} else if reaches {
			return ErrImplicationCycle
		}

		res, err := tx.Exec(
			`INSERT OR IGNORE INTO tag_implications (parent_tag_id, implied_tag_id, origin) VALUES (?, ?, ?)`,
			parentID, impliedID, origin,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		created = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// RemoveImplication deletes the parent -> implied edge. Image-side
// cleanup (removing implied rows that were only there because of this
// edge) is the caller's responsibility - it lives in the propagation
// job rather than the synchronous DELETE so the user's click returns
// fast on libraries with millions of image_tags.
func (s *Service) RemoveImplication(parentID, impliedID int64) error {
	res, err := s.db.Write.Exec(
		`DELETE FROM tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?`,
		parentID, impliedID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("implication not found")
	}
	return nil
}

// bfsImpliedTx walks the implied closure of start breadth-first, bounded
// by MaxImplicationDepth, invoking visit once per newly-discovered tag id
// (start ids never fire); visit returning false stops the walk early.
func bfsImpliedTx(tx *sql.Tx, start []int64, visit func(int64) bool) error {
	seen := make(map[int64]struct{}, len(start))
	for _, p := range start {
		seen[p] = struct{}{}
	}
	frontier := append([]int64(nil), start...)
	for depth := 0; depth < MaxImplicationDepth && len(frontier) > 0; depth++ {
		placeholders, args := db.InPlaceholders(frontier)
		// Read the whole level before visiting any of it: visit is the
		// caller's own code and must not run with a cursor held open.
		ids, err := db.QueryIDs(tx,
			`SELECT DISTINCT implied_tag_id FROM tag_implications WHERE parent_tag_id IN (`+placeholders+`)`,
			args...)
		if err != nil {
			return err
		}
		var next []int64
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			if !visit(id) {
				return nil
			}
			next = append(next, id)
		}
		frontier = next
	}
	return nil
}

// implicationReachesTx returns whether a directed path from start
// reaches target through tag_implications. Used for cycle detection
// inside AddImplication's transaction; callers never pass
// start == target (AddImplication rejects self-implication first).
func implicationReachesTx(tx *sql.Tx, start, target int64) (bool, error) {
	reached := false
	err := bfsImpliedTx(tx, []int64{start}, func(id int64) bool {
		if id == target {
			reached = true
			return false
		}
		return true
	})
	return reached, err
}

// ApplyImpliedFanoutTx fans out implications on an open transaction so
// callers outside the tags package (the auto-tagger insert path and
// the propagation job) get the same is_implied=1 rows the tags-service
// write path produces. The is_auto value is the parent's; implied rows
// inherit it so the detail-page source grouping keeps tracking origin.
// ratingCatID, if non-zero, gates a pruneLowerRatingsTx pass after the
// fan-out so an implication whose implied side is a rating tag doesn't
// leave the image with multiple rating rows.
func ApplyImpliedFanoutTx(tx *sql.Tx, imageID, parentID, ratingCatID int64, isAuto bool) error {
	isAutoInt := 0
	if isAuto {
		isAutoInt = 1
	}
	return fanOutImpliedTxImpl(tx, imageID, parentID, ratingCatID, isAutoInt)
}

// fanOutImpliedTxImpl is the package-internal twin shared between the
// service's addTagToImageTxReportingDup and the public ApplyImpliedFanoutTx
// entrypoint. Kept private so the fan-out logic lives in one place.
func fanOutImpliedTxImpl(tx *sql.Tx, imageID, parentID, ratingCatID int64, isAutoInt int) error {
	implied, err := transitiveImpliedTx(tx, []int64{parentID})
	if err != nil {
		return err
	}
	return applyImpliedClosureTx(tx, imageID, implied, ratingCatID, isAutoInt)
}

// applyImpliedClosureTx is fanOutImpliedTxImpl with the closure walk
// hoisted to the caller. MergeTags uses it to avoid recomputing the
// canonical's invariant implied closure once per newly-carrying image
// in the post-move loop; pure addTagToImage* callers go through
// fanOutImpliedTxImpl which resolves the closure for them.
func applyImpliedClosureTx(tx *sql.Tx, imageID int64, implied []int64, ratingCatID int64, isAutoInt int) error {
	insertedRating := false
	for _, id := range implied {
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name)
			 VALUES (?, ?, ?, 1, NULL, NULL)`,
			imageID, id, isAutoInt,
		)
		if err != nil {
			return fmt.Errorf("inserting implied tag %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := BumpTagUsageTx(tx, id, imageID); err != nil {
			return err
		}
		// If this newly-inserted implied tag is a rating, mark for the
		// post-fanout prune so the image keeps the highest-wins
		// invariant the executor's fast counts depend on.
		if ratingCatID != 0 && !insertedRating {
			var catID int64
			if err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&catID); err == nil && catID == ratingCatID {
				insertedRating = true
			}
		}
	}
	if insertedRating && ratingCatID != 0 {
		if err := pruneLowerRatingsTx(tx, ratingCatID, imageID); err != nil {
			return fmt.Errorf("prune lower ratings after implied fan-out: %w", err)
		}
	}
	return nil
}

// impliedByParentOnImage reports whether the (imageID, tagID) row is an
// implication fan-out that a parent still on the image justifies. Removing
// such a row directly breaks the operator's own declaration - the image
// keeps the parent, loses the child, and drops out of searches for a tag
// its own tags imply - so the operator-facing removal paths refuse it.
func impliedByParentOnImage(tx *sql.Tx, imageID, tagID int64) (bool, error) {
	var isImplied int
	switch err := tx.QueryRow(
		`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, tagID,
	).Scan(&isImplied); {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	}
	if isImplied != 1 {
		return false, nil
	}
	// An orphan - is_implied = 1 with no parent left on the image - stays
	// removable: refusing it would trap a row nothing justifies.
	parents, err := implicationParentsOnImageExcluding(tx, imageID, tagID, 0)
	if err != nil {
		return false, err
	}
	return len(parents) > 0, nil
}

// implicationParentsOnImageExcluding returns the tag ids on the image
// that still imply impliedID, optionally excluding one parent (the one
// being removed). Used by the propagation cleanup job and by
// removeTagFromImageTx to decide whether an implied row should stay.
func implicationParentsOnImageExcluding(tx *sql.Tx, imageID, impliedID, excludeParent int64) ([]int64, error) {
	return db.QueryAll(tx, func(rows *sql.Rows) (int64, error) {
		var id int64
		err := rows.Scan(&id)
		return id, err
	},
		`SELECT ti.parent_tag_id
		 FROM tag_implications ti
		 JOIN image_tags it ON it.tag_id = ti.parent_tag_id
		 WHERE ti.implied_tag_id = ? AND it.image_id = ? AND ti.parent_tag_id != ?`,
		impliedID, imageID, excludeParent,
	)
}
