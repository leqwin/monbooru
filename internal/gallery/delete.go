package gallery

import (
	"fmt"
	"os"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
)

// DeleteImageResult holds metadata about a deleted image for post-delete cleanup.
type DeleteImageResult struct {
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
}

// DeleteImage removes an image from the database in a single transaction,
// then cleans up files on disk. removeAllTags is called within the transaction
// to handle tag/co-occurrence cleanup (injected to avoid circular dependency on tags package).
func DeleteImage(database *db.DB, thumbnailsPath string, id int64, removeAllTags func(int64) error) (*DeleteImageResult, error) {
	var canonPath, folderPath string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath, &isMissing); err != nil {
		return nil, fmt.Errorf("image not found: %w", err)
	}

	// Remove all tags (co-occurrence cleanup) before deleting the row.
	// removeAllTags already prunes zero-usage tags scoped to the image's
	// own tag set, so deleting the image row afterwards no longer needs a
	// follow-up unscoped `DELETE FROM tags WHERE usage_count <= 0`. The
	// previous implementation scanned the whole tag table on every single
	// delete and could prune unrelated rows that happened to be at zero
	// usage from prior batch operations.
	if err := removeAllTags(id); err != nil {
		logx.Warnf("remove tags for image %d: %v", id, err)
	}

	if _, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete image row: %w", err)
	}

	// File cleanup (after successful DB commit).
	os.Remove(ThumbnailPath(thumbnailsPath, id))
	os.Remove(HoverPath(thumbnailsPath, id))

	result := &DeleteImageResult{
		CanonicalPath: canonPath,
		FolderPath:    folderPath,
		IsMissing:     isMissing == 1,
	}

	if !result.IsMissing && canonPath != "" {
		if err := os.Remove(canonPath); err != nil && !os.IsNotExist(err) {
			logx.Warnf("delete image file %q: %v", canonPath, err)
		}
	}

	return result, nil
}
