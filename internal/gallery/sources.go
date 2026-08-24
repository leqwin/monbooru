package gallery

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// Caps on the operator-editable provenance fields. The detail-page editor
// and the API both write these columns, so both refuse at the same length.
const (
	MaxSourceLabelLen    = 200
	MaxSourceURLLen      = 2048
	MaxCommentaryLen     = 10000
	MaxOriginalLen       = 2048
	MaxAnnotationBodyLen = 4000
)

// ValidExternalURL reports whether s carries a scheme monbooru will render
// as a link: both html/template's sanitiser and the explicit allowlist
// refuse anything but http(s), so an accepted value would render inert.
func ValidExternalURL(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// updateSourceField writes one column on an existing origin row, keyed by
// site+post_id, never creating one. Callers guard the no-op case (empty
// string / zero score) first; col is a trusted constant, never caller input.
func updateSourceField(database *db.DB, imageID int64, site, postID, col string, val any) error {
	_, err := database.Write.Exec(
		fmt.Sprintf(`UPDATE image_sources SET %s = ? WHERE image_id = ? AND site = ? AND post_id = ?`, col),
		val, imageID, strings.TrimSpace(site), strings.TrimSpace(postID))
	return err
}

// images.source / images.url mirror the "primary" origin - the lowest-rowid
// row in image_sources (the oldest, until MakeSourcePrimary reorders) - so
// the search executor, the image response, and gallery export keep riding
// the scalar columns. The invariant: they are non-empty iff the image has at
// least one origin, and when set they equal the primary row. The helpers
// below maintain it, the same way the collections helpers maintain
// images.series.

// SourcesForImage returns every origin of imageID, primary first.
func SourcesForImage(database *db.DB, imageID int64) ([]models.ImageSource, error) {
	return db.QueryAll(database.Read, func(rows *sql.Rows) (models.ImageSource, error) {
		var s models.ImageSource
		err := rows.Scan(&s.Site, &s.PostID, &s.URL, &s.Commentary, &s.Original, &s.Similarity, &s.MD5, &s.MD5Match,
			&s.UpgradeKept, &s.PostWidth, &s.PostHeight, &s.PostSize, &s.PostExt)
		return s, err
	},
		`SELECT site, post_id, url, commentary, original, similarity, md5, md5_match,
		        upgrade_kept, post_width, post_height, post_size, post_ext
		 FROM image_sources WHERE image_id = ? ORDER BY rowid`, imageID)
}

// AddSourceMembership upserts one origin (adding it or updating its url,
// keyed by site+post_id) and rebinds the primary mirror. An empty incoming
// url never clears a stored one - a url-less re-push or enrich of a known
// origin must not wipe operator-entered or previously-fetched provenance,
// matching the commentary path's empty-guard.
func AddSourceMembership(database *db.DB, imageID int64, site, postID, url string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	url = strings.TrimSpace(url)
	if site == "" && url == "" {
		return errors.New("source label or url required")
	}
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		if postID != "" && url != "" {
			// Rows written before pushes and enriches carried post ids are keyed
			// (site, ""); adopt one matching by url so a refetch updates it in
			// place instead of leaving a twin row for the same post.
			if _, err := tx.Exec(
				`UPDATE OR IGNORE image_sources SET post_id = ?
				 WHERE image_id = ? AND site = ? AND post_id = '' AND url = ?`,
				postID, imageID, site, url); err != nil {
				return err
			}
		}
		_, err := tx.Exec(
			`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, ?, ?)
			 ON CONFLICT(image_id, site, post_id) DO UPDATE SET
			   url = CASE WHEN excluded.url != '' THEN excluded.url ELSE url END`,
			imageID, site, postID, url)
		return err
	})
}

// SetSourceMD5 records the md5 the source claimed for one origin (the audit
// trail; never a dedup key). An empty incoming value keeps the stored one.
func SetSourceMD5(database *db.DB, imageID int64, site, postID, md5 string) error {
	md5 = strings.TrimSpace(md5)
	if md5 == "" {
		return nil
	}
	return updateSourceField(database, imageID, site, postID, "md5", md5)
}

// SetSourceParentURL records the canonical URL of the post one origin
// declared as its parent (booru parent/child). An empty incoming value keeps
// the stored one, so a parentless re-push never clears it.
func SetSourceParentURL(database *db.DB, imageID int64, site, postID, parentURL string) error {
	parentURL = strings.TrimSpace(parentURL)
	if parentURL == "" {
		return nil
	}
	return updateSourceField(database, imageID, site, postID, "parent_url", parentURL)
}

// ImageIDBySourceURL returns the image holding an origin with the given URL -
// the parent-side probe of the derivative-edge linking. When several images
// claim the same origin the lowest id wins, for a stable pick.
func ImageIDBySourceURL(database *db.DB, url string) (int64, bool) {
	var id int64
	err := database.Read.QueryRow(
		`SELECT image_id FROM image_sources WHERE url = ? ORDER BY image_id LIMIT 1`, url).Scan(&id)
	return id, err == nil
}

// ChildIDsByParentURL returns the images whose origin rows declare the given
// URL as their parent - the child-side probe of the derivative-edge linking.
func ChildIDsByParentURL(database *db.DB, url string) ([]int64, error) {
	return db.QueryIDs(database.Read,
		`SELECT DISTINCT image_id FROM image_sources WHERE parent_url = ? ORDER BY image_id`, url)
}

// SetSourceSimilarity records the score a similarity lookup matched one
// origin with - the mark that lets later refetches of that origin skip the
// md5 verify (its file differs from the stored one by design). A zero
// incoming score keeps the stored one, so a plain refetch never clears it.
func SetSourceSimilarity(database *db.DB, imageID int64, site, postID string, score float64) error {
	if score <= 0 {
		return nil
	}
	return updateSourceField(database, imageID, site, postID, "similarity", score)
}

// SetSourceMD5Match records the verdict of comparing one origin's claimed
// md5 against the local file: "match", "differ", or "" (unknown). The
// verify may see the origin under its exact key or, before the merge
// adopts the post id, under (site, ""), so a miss on the exact key falls
// back to the id-less row - the same dual key SourceSimilarityMatched
// checks.
func SetSourceMD5Match(database *db.DB, imageID int64, site, postID, verdict string) error {
	site = strings.TrimSpace(site)
	res, err := database.Write.Exec(
		`UPDATE image_sources SET md5_match = ? WHERE image_id = ? AND site = ? AND post_id = ?`,
		verdict, imageID, site, strings.TrimSpace(postID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 || strings.TrimSpace(postID) == "" {
		return nil
	}
	_, err = database.Write.Exec(
		`UPDATE image_sources SET md5_match = ? WHERE image_id = ? AND site = ? AND post_id = ''`,
		verdict, imageID, site)
	return err
}

// MarkSourceExact records that one origin now serves exactly the local
// bytes: similarity cleared, the claimed md5 replaced by the file's own,
// and the verdict set to match. A successful replace lands here; the
// unconditional writes bypass the keep-stored guards on the individual
// setters by design.
func MarkSourceExact(database *db.DB, imageID int64, site, postID, md5 string) error {
	_, err := database.Write.Exec(
		`UPDATE image_sources SET similarity = 0, md5 = ?, md5_match = 'match'
		 WHERE image_id = ? AND site = ? AND post_id = ?`,
		strings.TrimSpace(md5), imageID, strings.TrimSpace(site), strings.TrimSpace(postID))
	return err
}

// PostFile is what a post says about the file it serves, as opposed to what
// the local file measures. Zero fields mean the source published nothing,
// which is most sites for most fields.
type PostFile struct {
	Width, Height int
	Size          int64
	Ext           string
}

// SetSourcePostFile records what the post claims its file is. Each field
// keeps its stored value when the incoming one is empty, like the md5 and
// commentary setters: a site that publishes dimensions but no size must not
// wipe a size an earlier fetch got from somewhere else.
func SetSourcePostFile(database *db.DB, imageID int64, site, postID string, f PostFile) error {
	if f.Width <= 0 && f.Height <= 0 && f.Size <= 0 && strings.TrimSpace(f.Ext) == "" {
		return nil
	}
	_, err := database.Write.Exec(
		`UPDATE image_sources SET
		   post_width  = CASE WHEN ? > 0 THEN ? ELSE post_width END,
		   post_height = CASE WHEN ? > 0 THEN ? ELSE post_height END,
		   post_size   = CASE WHEN ? > 0 THEN ? ELSE post_size END,
		   post_ext    = CASE WHEN ? != '' THEN ? ELSE post_ext END
		 WHERE image_id = ? AND site = ? AND post_id = ?`,
		f.Width, f.Width, f.Height, f.Height, f.Size, f.Size,
		strings.ToLower(strings.TrimSpace(f.Ext)), strings.ToLower(strings.TrimSpace(f.Ext)),
		imageID, strings.TrimSpace(site), strings.TrimSpace(postID))
	return err
}

// SetSourceUpgradeKept records (or clears) the operator's decision to keep
// the local file over what this origin serves. The trigger on md5 clears it
// again when the post claims a digest it has not claimed before.
func SetSourceUpgradeKept(database *db.DB, imageID int64, site, postID string, kept bool) error {
	return updateSourceField(database, imageID, site, postID, "upgrade_kept", kept)
}

// SourceSimilarityMatched reports whether one origin was recorded by a
// similarity lookup.
func SourceSimilarityMatched(database *db.DB, imageID int64, site, postID string) bool {
	var matched bool
	err := database.Read.QueryRow(
		`SELECT similarity > 0 FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
		imageID, strings.TrimSpace(site), strings.TrimSpace(postID)).Scan(&matched)
	return err == nil && matched
}

// RenameSourceMembership relabels one origin in place, keeping its
// commentary / original / md5 / fetched_at and its age (a relabelled primary
// stays primary), and re-keys the origin's annotations so they follow the new
// identity. When the new identity already exists on the image the two rows
// merge: the target keeps its own commentary and original unless it has none,
// and the old row is dropped. A missing prev identity falls back to a plain
// upsert.
func RenameSourceMembership(database *db.DB, imageID int64, prevSite, prevPost, site, postID, url string) error {
	prevSite = strings.TrimSpace(prevSite)
	prevPost = strings.TrimSpace(prevPost)
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	url = strings.TrimSpace(url)
	if site == "" && url == "" {
		return errors.New("source label or url required")
	}
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		var prevRid int64
		switch err := tx.QueryRow(
			`SELECT rowid FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, prevSite, prevPost).Scan(&prevRid); {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(
				`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, ?, ?)
				 ON CONFLICT(image_id, site, post_id) DO UPDATE SET url = excluded.url`,
				imageID, site, postID, url); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			var targetRid int64
			switch err := tx.QueryRow(
				`SELECT rowid FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
				imageID, site, postID).Scan(&targetRid); {
			case errors.Is(err, sql.ErrNoRows):
				if _, err := tx.Exec(
					`UPDATE image_sources SET site = ?, post_id = ?, url = ? WHERE rowid = ?`,
					site, postID, url, prevRid); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				if _, err := tx.Exec(
					`UPDATE image_sources SET url = ?,
					        commentary = CASE WHEN commentary = '' THEN (SELECT commentary FROM image_sources WHERE rowid = ?) ELSE commentary END,
					        original = CASE WHEN original = '' THEN (SELECT original FROM image_sources WHERE rowid = ?) ELSE original END
					 WHERE rowid = ?`,
					url, prevRid, prevRid, targetRid); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM image_sources WHERE rowid = ?`, prevRid); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(
				`UPDATE image_annotations SET site = ?, post_id = ? WHERE image_id = ? AND site = ? AND post_id = ? AND manual = 0`,
				site, postID, imageID, prevSite, prevPost); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveSourceMembership drops one origin along with the annotations it
// pulled (they carry the same identity and have no other removal path) and
// rebinds the primary mirror.
func RemoveSourceMembership(database *db.DB, imageID int64, site, postID string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, site, postID); err != nil {
			return err
		}
		return deletePulledAnnotationsTx(tx, imageID, site, postID)
	})
}

// setSourceTextField sets one text column (commentary or original, a trusted
// constant) on an origin: a non-empty value upserts the origin (creating it
// when absent, so the field can be added to a source the image does not list
// yet), an empty value only clears an existing origin's field and never
// creates one. Rebinds the primary mirror since an upsert can add the first
// origin.
func setSourceTextField(database *db.DB, imageID int64, site, postID, col, value string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	value = strings.TrimSpace(value)
	if site == "" {
		return errors.New("source label required")
	}
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		if value == "" {
			_, err := tx.Exec(
				fmt.Sprintf(`UPDATE image_sources SET %s = '' WHERE image_id = ? AND site = ? AND post_id = ?`, col),
				imageID, site, postID)
			return err
		}
		_, err := tx.Exec(
			fmt.Sprintf(`INSERT INTO image_sources (image_id, site, post_id, %[1]s) VALUES (?, ?, ?, ?)
			 ON CONFLICT(image_id, site, post_id) DO UPDATE SET %[1]s = excluded.%[1]s`, col),
			imageID, site, postID, value)
		return err
	})
}

// SetSourceCommentary sets the artist commentary attributed to one origin. See
// setSourceTextField for the upsert / clear semantics.
func SetSourceCommentary(database *db.DB, imageID int64, site, postID, commentary string) error {
	return setSourceTextField(database, imageID, site, postID, "commentary", commentary)
}

// SetSourceOriginal sets the upstream artist source the booru post declared for
// one origin. See setSourceTextField for the upsert / clear semantics.
func SetSourceOriginal(database *db.DB, imageID int64, site, postID, original string) error {
	return setSourceTextField(database, imageID, site, postID, "original", original)
}

// ErrSourceIdentityExists reports a relabel that would collide with another
// origin already recorded on the image.
var ErrSourceIdentityExists = errors.New("another source with that label already exists on this image")

// SetPrimarySource edits the primary origin in place - the site / url the
// scalar mirrors - or clears it when both are empty, creating a first origin
// when the image has none. Used by the API PATCH, which carries a single
// source/url pair. Operating on the primary row's rowid keeps its age so it
// stays primary through a relabel.
func SetPrimarySource(database *db.DB, imageID int64, site, url string) error {
	site = strings.TrimSpace(site)
	url = strings.TrimSpace(url)
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		var rid int64
		var curSite, curPost string
		err := tx.QueryRow(`SELECT rowid, site, post_id FROM image_sources WHERE image_id = ? ORDER BY rowid LIMIT 1`, imageID).Scan(&rid, &curSite, &curPost)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if site != "" || url != "" {
				if _, err := tx.Exec(
					`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, '', ?)`,
					imageID, site, url); err != nil {
					return err
				}
			}
		case err != nil:
			return err
		case site == "" && url == "":
			if _, err := tx.Exec(`DELETE FROM image_sources WHERE rowid = ?`, rid); err != nil {
				return err
			}
			if err := deletePulledAnnotationsTx(tx, imageID, curSite, curPost); err != nil {
				return err
			}
		default:
			var clash bool
			if err := tx.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ? AND rowid != ?)`,
				imageID, site, curPost, rid).Scan(&clash); err != nil {
				return err
			}
			if clash {
				return ErrSourceIdentityExists
			}
			if _, err := tx.Exec(`UPDATE image_sources SET site = ?, url = ? WHERE rowid = ?`,
				site, url, rid); err != nil {
				return err
			}
		}
		return nil
	})
}

// MakeSourcePrimary reorders one existing origin to primary by moving it
// below every other rowid (the table-wide MIN keeps the new rowid unique)
// and rebinds the mirror. Promoting the current primary is a harmless no-op
// move.
func MakeSourcePrimary(database *db.DB, imageID int64, site, postID string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE image_sources SET rowid = (SELECT MIN(rowid) - 1 FROM image_sources)
			 WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, site, postID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("source not found on this image")
		}
		return nil
	})
}

// sourceTx runs one origin-table write and rebinds the primary mirror in
// the same transaction: the mirror follows whichever row is now lowest by
// rowid, so it has to be recomputed inside whatever changed the set. A
// sentinel work returns (ErrSourceIdentityExists, "source not found on this
// image") rolls the whole thing back, which is what those callers rely on.
func sourceTx(database *db.DB, imageID int64, work func(*sql.Tx) error) error {
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if err := work(tx); err != nil {
			return err
		}
		return rebindPrimarySourceTx(tx, imageID)
	})
}

// rebindPrimarySourceTx repoints images.source / images.url at the lowest-
// rowid origin row (or clears them when none remain).
func rebindPrimarySourceTx(tx *sql.Tx, imageID int64) error {
	var site, url string
	err := tx.QueryRow(`SELECT site, url FROM image_sources WHERE image_id = ? ORDER BY rowid LIMIT 1`, imageID).Scan(&site, &url)
	if errors.Is(err, sql.ErrNoRows) {
		_, e := tx.Exec(`UPDATE images SET source = '', url = '' WHERE id = ?`, imageID)
		return e
	}
	if err != nil {
		return err
	}
	_, e := tx.Exec(`UPDATE images SET source = ?, url = ? WHERE id = ?`, site, url, imageID)
	return e
}
