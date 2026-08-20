package gallery

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// images.series / series_order mirror one "home" membership so the
// global order-sort and the adjacency cursor keep riding the scalar
// columns. The invariant: series != '' iff the image has at least one
// membership, and when set it equals the home row in image_collections.
// The helpers below maintain that invariant.

func orderValue(order *int) any {
	if order == nil {
		return nil
	}
	return *order
}

// CollectionsForImage returns every membership of imageID, ordered as the
// detail page renders them: positioned rows first (ascending), then the
// unordered ones by name.
func CollectionsForImage(database *db.DB, imageID int64) ([]models.Collection, error) {
	return db.QueryAll(database.Read, func(rows *sql.Rows) (models.Collection, error) {
		var c models.Collection
		var pos sql.NullInt64
		err := rows.Scan(&c.Name, &pos)
		if pos.Valid {
			v := int(pos.Int64)
			c.Order = &v
		}
		return c, err
	}, `SELECT name, position FROM image_collections WHERE image_id = ?
		 ORDER BY position IS NULL, position, name`, imageID)
}

// AddCollectionMembership upserts a membership (adding it or just updating
// its position) and keeps the home mirror in step: an image with no home
// adopts this one; re-setting the home's own position updates the mirror.
func AddCollectionMembership(database *db.DB, imageID int64, name string, order *int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("collection name required")
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return addMembershipTx(tx, imageID, name, order)
	})
}

func addMembershipTx(tx *sql.Tx, imageID int64, name string, order *int) error {
	if _, err := tx.Exec(
		`INSERT INTO image_collections (image_id, name, position) VALUES (?, ?, ?)
		 ON CONFLICT(image_id, name) DO UPDATE SET position = excluded.position`,
		imageID, name, orderValue(order)); err != nil {
		return err
	}
	home, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	switch {
	case home == "":
		_, err = tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
			name, orderValue(order), imageID)
	case strings.EqualFold(home, name):
		_, err = tx.Exec(`UPDATE images SET series_order = ? WHERE id = ?`,
			orderValue(order), imageID)
	}
	return err
}

// RemoveCollectionMembership drops a membership; when it was the home the
// next membership is promoted (or the mirror cleared if none remain).
func RemoveCollectionMembership(database *db.DB, imageID int64, name string) error {
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return removeMembershipTx(tx, imageID, name)
	})
}

func removeMembershipTx(tx *sql.Tx, imageID int64, name string) error {
	if _, err := tx.Exec(`DELETE FROM image_collections WHERE image_id = ? AND name = ?`,
		imageID, name); err != nil {
		return err
	}
	return rebindHomeTx(tx, imageID, name)
}

// RenameCollectionMembership relabels imageID's membership from prev to
// name in one transaction. Split across two writes, a failure on the
// second leaves the image in neither collection with nothing saying so.
func RenameCollectionMembership(database *db.DB, imageID int64, prev, name string, order *int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("collection name required")
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if err := removeMembershipTx(tx, imageID, prev); err != nil {
			return err
		}
		return addMembershipTx(tx, imageID, name, order)
	})
}

// SetHomeCollection points imageID's home at name with the given order,
// renaming or clearing the previous home and keeping image_collections in
// sync. Used by the API and ingest, which carry a single collection field.
// An empty name clears the home, promoting another membership if one is
// left so the series != "" invariant holds. Pointing the home at a label
// the image already belongs to promotes that membership in place and
// leaves the former home as an extra; only relabelling onto a new name
// (or clearing) drops the old home.
func SetHomeCollection(database *db.DB, imageID int64, name string, order *int) error {
	name = strings.TrimSpace(name)
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	oldHome, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	relabel := oldHome != "" && !strings.EqualFold(oldHome, name)
	if relabel && name != "" {
		var x int
		switch err := tx.QueryRow(
			`SELECT 1 FROM image_collections WHERE image_id = ? AND name = ?`, imageID, name).Scan(&x); {
		case err == nil:
			relabel = false // target already a member: promote, don't drop the old home
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	if relabel {
		if _, err := tx.Exec(`DELETE FROM image_collections WHERE image_id = ? AND name = ?`,
			imageID, oldHome); err != nil {
			return err
		}
	}
	if name == "" {
		if err := rebindHomeTx(tx, imageID, oldHome); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.Exec(
		`INSERT INTO image_collections (image_id, name, position) VALUES (?, ?, ?)
		 ON CONFLICT(image_id, name) DO UPDATE SET position = excluded.position`,
		imageID, name, orderValue(order)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
		name, orderValue(order), imageID); err != nil {
		return err
	}
	return tx.Commit()
}

func homeName(tx *sql.Tx, imageID int64) (string, error) {
	var home sql.NullString
	if err := tx.QueryRow(`SELECT series FROM images WHERE id = ?`, imageID).Scan(&home); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !home.Valid {
		return "", nil
	}
	return home.String, nil
}

// rebindHomeTx repoints the mirror after changedName left the membership
// set. A no-op unless changedName was the home; then it promotes the next
// membership or clears the mirror when none remain.
func rebindHomeTx(tx *sql.Tx, imageID int64, changedName string) error {
	home, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	if home == "" || !strings.EqualFold(home, changedName) {
		return nil
	}
	var name sql.NullString
	var pos sql.NullInt64
	err = tx.QueryRow(
		`SELECT name, position FROM image_collections WHERE image_id = ?
		 ORDER BY position IS NULL, position, name LIMIT 1`, imageID).Scan(&name, &pos)
	if errors.Is(err, sql.ErrNoRows) {
		_, e := tx.Exec(`UPDATE images SET series = '', series_order = NULL WHERE id = ?`, imageID)
		return e
	}
	if err != nil {
		return err
	}
	var ord any
	if pos.Valid {
		ord = pos.Int64
	}
	_, e := tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
		name.String, ord, imageID)
	return e
}

// CollectionSummary is one row of the collections management page: a
// label, its visible member count, and a few members for the preview.
// FindRelations reports the collection's opt-in to the relations
// session surfacing pairs among its own members.
type CollectionSummary struct {
	Name          string
	Count         int
	FindRelations bool
	Samples       []CollectionSample
}

// CollectionSample is one preview tile: the image id, its position within the
// collection (nil when the membership is unordered), and its filename for the
// reorder dialog's filename mode / tooltip.
type CollectionSample struct {
	ID       int64
	Order    *int
	Filename string
}

// collectionFilterWhere returns the substring-match fragment (and its
// args) for a non-empty name filter against col, empty otherwise so it
// splices into a WHERE clause without juggling the boundary.
func collectionFilterWhere(col, nameFilter string) (string, []any) {
	if nameFilter == "" {
		return "", nil
	}
	return ` AND ` + col + ` LIKE ? ESCAPE '\'`, []any{"%" + db.EscapeLike(nameFilter) + "%"}
}

// ListCollections returns one page of collection labels with their
// visible (non-missing) member counts. sort "name" orders alphabetically;
// any other value orders by member count descending, name as tiebreaker.
// Members carrying a tag in excludeIDs (the rating ceiling) drop from the
// count, so a collection with no visible member left falls off the page.
func ListCollections(database *db.DB, nameFilter, sort string, limit, offset int, excludeIDs []int64) ([]CollectionSummary, error) {
	where, filterArgs := collectionFilterWhere("c.name", nameFilter)
	var query string
	var args []any
	if len(excludeIDs) == 0 {
		// No ceiling: the trigger-maintained per-label counts make the
		// listing one row per label instead of a walk over every
		// membership with a per-row visibility probe.
		orderBy := "c.visible_count DESC, c.name ASC"
		if sort == "name" {
			orderBy = "c.name ASC"
		}
		query = `SELECT c.name, c.visible_count,
		        EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = c.name)
		 FROM collection_counts c
		 WHERE c.visible_count > 0` + where + `
		 ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
		args = append(filterArgs, limit, offset)
	} else {
		// Ceiling active: the stored counts are ceiling-blind, so fall
		// back to the aggregation. EXISTS visibility (vs a join to
		// images) lets the GROUP BY stream off idx_image_collections_name
		// instead of a temp B-tree over every member.
		exclude, excludeArgs := excludeNotExists("c.image_id", excludeIDs)
		orderBy := "cnt DESC, c.name ASC"
		if sort == "name" {
			orderBy = "c.name ASC"
		}
		query = `SELECT c.name, COUNT(*) cnt,
		        EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = c.name)
		 FROM image_collections c
		 WHERE EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0)` + exclude + where + `
		 GROUP BY c.name ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
		args = append(append(excludeArgs, filterArgs...), limit, offset)
	}
	return db.QueryAll(database.Read, func(rows *sql.Rows) (CollectionSummary, error) {
		var c CollectionSummary
		err := rows.Scan(&c.Name, &c.Count, &c.FindRelations)
		return c, err
	}, query, args...)
}

// SetCollectionFindRelations flips a collection's find-relations opt-in.
// The flag is a bare presence row; disabling just deletes it.
func SetCollectionFindRelations(database *db.DB, name string, enabled bool) error {
	if enabled {
		_, err := database.Write.Exec(
			`INSERT OR IGNORE INTO collection_find_relations (name) VALUES (?)`, name)
		return err
	}
	_, err := database.Write.Exec(
		`DELETE FROM collection_find_relations WHERE name = ?`, name)
	return err
}

// CountCollections returns the number of distinct collection labels with
// at least one visible member, honoring the same substring filter and the
// rating ceiling (excludeIDs).
func CountCollections(database *db.DB, nameFilter string, excludeIDs []int64) (int, error) {
	var n int
	if len(excludeIDs) == 0 {
		where, args := collectionFilterWhere("name", nameFilter)
		err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM collection_counts WHERE visible_count > 0`+where, args...).Scan(&n)
		return n, err
	}
	// Ceiling active: enumerate distinct labels off the name index and
	// keep those with a visible, ceiling-clear member; the per-label
	// EXISTS short-circuits, so cost tracks the label count, not the
	// membership count.
	exclude, args := excludeNotExists("c.image_id", excludeIDs)
	where, filterArgs := collectionFilterWhere("d.name", nameFilter)
	args = append(args, filterArgs...)
	err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (SELECT DISTINCT name FROM image_collections) d
		 WHERE EXISTS (SELECT 1 FROM image_collections c JOIN images i ON i.id = c.image_id
		   WHERE c.name = d.name AND i.is_missing = 0`+exclude+`)`+where, args...).Scan(&n)
	return n, err
}

// CollectionSamples returns up to per visible members for each named
// collection, in reading order (position first with NULLs last, then id).
// Members above the rating ceiling (excludeIDs) are skipped so the preview
// matches the listing. The map is keyed by lower-cased label so a single
// key survives images that stored the same NOCASE label in different cases.
// One LIMITed query per name: the reading index stops each walk after the
// first per visible members, where a single ROW_NUMBER window would rank
// every member of every listed label first.
func CollectionSamples(database *db.DB, names []string, per int, excludeIDs []int64) (map[string][]CollectionSample, error) {
	out := make(map[string][]CollectionSample, len(names))
	if len(names) == 0 || per <= 0 {
		return out, nil
	}
	for _, name := range names {
		samples, err := collectionWalk(database, name, excludeIDs, per, 0)
		if err != nil {
			return out, err
		}
		key := strings.ToLower(name)
		if len(out[key]) == 0 {
			out[key] = samples
		}
	}
	return out, nil
}

// collectionWalk reads one window of name's visible members in reading
// order, riding idx_image_collections_reading so the LIMIT stops the
// scan early instead of sorting the whole label.
func collectionWalk(database *db.DB, name string, excludeIDs []int64, limit, offset int) ([]CollectionSample, error) {
	exclude, args := excludeNotExists("i.id", excludeIDs)
	args = append([]any{name}, args...)
	args = append(args, limit, offset)
	return db.QueryAll(database.Read, func(rows *sql.Rows) (CollectionSample, error) {
		var sample CollectionSample
		var pos sql.NullInt64
		err := rows.Scan(&sample.ID, &pos, &sample.Filename)
		if pos.Valid {
			v := int(pos.Int64)
			sample.Order = &v
		}
		return sample, err
	},
		`SELECT c.image_id, c.position, basename(i.canonical_path)
		 FROM image_collections c JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND i.is_missing = 0`+exclude+`
		 ORDER BY c.position IS NULL, c.position, c.image_id LIMIT ? OFFSET ?`, args...)
}

// CollectionMembers returns one window of name's visible members
// (NOCASE) in reading order (position first with NULLs last, then id),
// skipping members above the rating ceiling (excludeIDs) like the page
// listing. Windowed so a huge label can't force a full render in one
// dialog body.
func CollectionMembers(database *db.DB, name string, excludeIDs []int64, limit, offset int) ([]CollectionSample, error) {
	return collectionWalk(database, name, excludeIDs, limit, offset)
}

// ReorderCollection rewrites name's ordering from ids: 1-based positions
// in slice order, every other membership cleared to unordered. Ids not
// filed under name are no-ops. The home mirror follows for rows homed on
// the collection, the same resync shape the rename job uses. A list that
// fits one chunk (the click-order path, capped at the 200 window) runs in a
// single atomic transaction; a larger filename sort splits the position
// writes into 500-id chunks so the write tx stays bounded.
func ReorderCollection(database *db.DB, name string, ids []int64) error {
	const chunkSize = 500
	const clearAll = `UPDATE image_collections SET position = NULL WHERE name = ?`
	const setPos = `UPDATE image_collections SET position = ? WHERE image_id = ? AND name = ?`
	const resync = `UPDATE images SET series_order =
		   (SELECT position FROM image_collections WHERE image_id = images.id AND name = ?)
		 WHERE series = ? COLLATE NOCASE`

	if len(ids) <= chunkSize {
		return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			if _, err := tx.Exec(clearAll, name); err != nil {
				return err
			}
			for i, id := range ids {
				if _, err := tx.Exec(setPos, i+1, id, name); err != nil {
					return err
				}
			}
			_, err := tx.Exec(resync, name, name)
			return err
		})
	}

	if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		_, e := tx.Exec(clearAll, name)
		return e
	}); err != nil {
		return err
	}
	for start := 0; start < len(ids); start += chunkSize {
		lo, hi := start, min(start+chunkSize, len(ids))
		if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			for i := lo; i < hi; i++ {
				if _, err := tx.Exec(setPos, i+1, ids[i], name); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		_, e := tx.Exec(resync, name, name)
		return e
	})
}

// SortCollectionByFilename orders every non-missing member of name by filename
// (natural order over the basename), ceiling-blind over the whole collection
// rather than just the reorder window, and applies the result through
// ReorderCollection.
func SortCollectionByFilename(database *db.DB, name string) error {
	type member struct {
		id       int64
		filename string
	}
	members, err := db.QueryAll(database.Read, func(rows *sql.Rows) (member, error) {
		var m member
		err := rows.Scan(&m.id, &m.filename)
		return m, err
	}, `SELECT c.image_id, basename(i.canonical_path)
		 FROM image_collections c JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND i.is_missing = 0`, name)
	if err != nil {
		return err
	}
	sort.SliceStable(members, func(i, j int) bool {
		return NaturalLess(strings.ToLower(members[i].filename), strings.ToLower(members[j].filename))
	})
	ids := make([]int64, len(members))
	for i, m := range members {
		ids[i] = m.id
	}
	return ReorderCollection(database, name, ids)
}

// CollectionCeilingHidden returns how many of name's visible members the rating
// ceiling (excludeIDs) hides: the ceiling-blind visible count minus the
// ceiling-filtered count. Zero when no ceiling is active or none are hidden.
func CollectionCeilingHidden(database *db.DB, name string, excludeIDs []int64) (int, error) {
	if len(excludeIDs) == 0 {
		return 0, nil
	}
	var blind int
	err := database.Read.QueryRow(
		`SELECT COALESCE(visible_count, 0) FROM collection_counts WHERE name = ?`, name).Scan(&blind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	exclude, exArgs := excludeNotExists("c.image_id", excludeIDs)
	var filtered int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM image_collections c
		 WHERE c.name = ? AND EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0)`+exclude,
		append([]any{name}, exArgs...)...).Scan(&filtered); err != nil {
		return 0, err
	}
	if blind < filtered {
		return 0, nil
	}
	return blind - filtered, nil
}

// CollectionMemberIDs returns every image id filed under name (case-
// insensitive), missing rows included, so a rename or dissolve reaches
// the whole collection rather than only its visible members.
func CollectionMemberIDs(database *db.DB, name string) ([]int64, error) {
	return db.QueryIDs(database.Read,
		`SELECT image_id FROM image_collections WHERE name = ? COLLATE NOCASE`, name)
}

// CollectionCBZMembers returns every visible member of name (NOCASE) in
// generation order: positioned members first by position, then unordered
// members by natural filename order. Like rename and dissolve, the
// rating ceiling does not filter the result.
func CollectionCBZMembers(database *db.DB, name string) ([]CBZMember, error) {
	rows, err := database.Read.Query(
		`SELECT i.canonical_path, i.file_type, basename(i.canonical_path), c.position
		 FROM image_collections c JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND i.is_missing = 0
		 ORDER BY c.position IS NULL, c.position, c.image_id`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out, unordered []CBZMember
	for rows.Next() {
		var m CBZMember
		var pos sql.NullInt64
		if err := rows.Scan(&m.Path, &m.FileType, &m.filename, &pos); err != nil {
			return nil, err
		}
		if pos.Valid {
			out = append(out, m)
		} else {
			unordered = append(unordered, m)
		}
	}
	// Numbered order wins; members without a position fall back to
	// natural filename order.
	sort.SliceStable(unordered, func(i, j int) bool {
		return NaturalLess(strings.ToLower(unordered[i].filename), strings.ToLower(unordered[j].filename))
	})
	return append(out, unordered...), rows.Err()
}
