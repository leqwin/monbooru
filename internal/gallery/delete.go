package gallery

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

// DeleteImageResult holds metadata about a deleted image for post-delete cleanup.
type DeleteImageResult struct {
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
}

// DeleteImage removes one image from the database, then cleans up files
// on disk. Two callbacks are injected (rather than direct package imports)
// to avoid internal/gallery → internal/tags / internal/relations cycles:
//   - removeAllTags clears the image_tags rows for id and prunes any
//     zero-usage tag that the image alone was carrying.
//   - onImageDelete (may be nil) fixes up relations-graph state that the
//     FK CASCADE can't reach - specifically dup_groups.original_image_id,
//     which is NOT NULL with no CASCADE so the parent DELETE would fail
//     while the image is still wearing the original badge.
//
// The on-disk half runs through RemoveImageArtifacts and
// UnlinkImageFile, shared with the bulk delete and prune-missing jobs
// so the containment gate cannot go missing on one of them.
func DeleteImage(database *db.DB, galleryPath, thumbnailsPath string, id int64, removeAllTags func(*sql.Tx, int64) error, onImageDelete func(*sql.Tx, int64) error) (*DeleteImageResult, error) {
	var canonPath, folderPath, fileType string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing, file_type FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath, &isMissing, &fileType); err != nil {
		return nil, fmt.Errorf("image not found: %w", err)
	}
	aliases, err := AliasPathsFor(database.Read, []int64{id})
	if err != nil {
		return nil, fmt.Errorf("alias paths for image %d: %w", id, err)
	}

	// One transaction for all three writes: a failure between them would
	// otherwise leave the row with its tags stripped and usage_count
	// decremented, drifting until the next RecalcCount. removeAllTags
	// prunes zero-usage tags scoped to this image's own tag set, so no
	// follow-up unscoped prune (which could touch unrelated rows) is
	// needed.
	tx, err := database.Write.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin delete image %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := removeAllTags(tx, id); err != nil {
		return nil, fmt.Errorf("remove tags for image %d: %w", id, err)
	}
	if onImageDelete != nil {
		if err := onImageDelete(tx, id); err != nil {
			return nil, fmt.Errorf("relations cleanup for image %d: %w", id, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM images WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete image row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delete image %d: %w", id, err)
	}

	RemoveImageArtifacts(thumbnailsPath, id, fileType)

	result := &DeleteImageResult{
		CanonicalPath: canonPath,
		FolderPath:    folderPath,
		IsMissing:     isMissing == 1,
	}

	if !result.IsMissing {
		UnlinkImageFile(galleryPath, canonPath, id)
	}
	UnlinkAliasFiles(galleryPath, id, aliases[id])

	return result, nil
}

// AliasCopy is one non-canonical copy of an image and the size the row's
// bytes have. Sha-dedup folded them onto one row, so every copy is the
// same length; a path whose file was replaced while nothing was watching
// is not, and that is what the size tells apart.
type AliasCopy struct {
	Path string
	Size int64
}

// AliasPathsFor reads each id's non-canonical copies on disk. Callers read
// them before the rows go: image_paths cascades with images.
func AliasPathsFor(q db.Querier, ids []int64) (map[int64][]AliasCopy, error) {
	placeholders, args := db.InPlaceholders(ids)
	if placeholders == "" {
		return nil, nil
	}
	rows, err := q.Query(
		`SELECT ip.image_id, ip.path, i.file_size
		   FROM image_paths ip
		   JOIN images i ON i.id = ip.image_id
		  WHERE ip.is_canonical = 0 AND ip.image_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64][]AliasCopy{}
	for rows.Next() {
		var id int64
		var c AliasCopy
		if err := rows.Scan(&id, &c.Path, &c.Size); err != nil {
			return nil, err
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}

// UnlinkAliasFiles removes the copies sha-dedup folded onto one row. They
// go with the image: once the row is gone nothing in the database names
// them, the duplicates walker cannot surface them, and the next sync
// ingests one as a new image with none of the tagging that was on it.
//
// A copy whose file is no longer the row's length is left where it is.
// With the watcher off, an overwrite at a recorded path survives until a
// sync, and pruneStaleAliasPaths only drops rows whose file is gone - so
// without the check the delete takes a file belonging to some other,
// untracked image, and unlike the duplicates walker nothing showed the
// operator the second path.
func UnlinkAliasFiles(galleryPath string, id int64, copies []AliasCopy) {
	for _, c := range copies {
		if info, err := os.Stat(c.Path); err == nil && info.Size() != c.Size {
			logx.Warnf("delete image %d: %q holds %d bytes, not the row's %d - left alone",
				id, c.Path, info.Size(), c.Size)
			continue
		}
		UnlinkImageFile(galleryPath, c.Path, id)
	}
}

// RemoveImageArtifacts deletes the derived files monbooru generated for
// one image: thumbnail, hover preview, display rendition, and the manga
// page cache. An empty fileType means the caller doesn't know it and the
// cache is removed unconditionally; passing the real type skips a
// RemoveAll for the static rows that never had one.
func RemoveImageArtifacts(thumbnailsPath string, id int64, fileType string) {
	_ = os.Remove(ThumbnailPath(thumbnailsPath, id))
	_ = os.Remove(HoverPath(thumbnailsPath, id))
	_ = os.Remove(ViewRenditionPath(thumbnailsPath, id))
	if fileType == "" || fileType == "cbz" {
		RemoveMangaCache(thumbnailsPath, id)
	}
}

// UnlinkImageFile removes one image's own file. galleryPath gates the
// unlink behind PathInside so a row whose canonical_path drifted
// outside the gallery root (a hand-edited DB, a repointed mount) can't
// make a delete remove an arbitrary filesystem path.
func UnlinkImageFile(galleryPath, canonPath string, id int64) {
	if canonPath == "" {
		return
	}
	if galleryPath != "" && !PathInside(galleryPath, canonPath) {
		logx.Warnf("delete image %d: refusing to unlink %q outside gallery root %q", id, canonPath, galleryPath)
		return
	}
	if err := os.Remove(canonPath); err != nil && !os.IsNotExist(err) {
		logx.Warnf("delete image file %q: %v", canonPath, err)
	}
}
