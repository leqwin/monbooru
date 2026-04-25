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

// DeleteImage removes one image from the database, then cleans up files
// on disk. removeAllTags is injected (rather than called directly via the
// tags package) to avoid an internal/gallery → internal/tags import cycle.
func DeleteImage(database *db.DB, thumbnailsPath string, id int64, removeAllTags func(int64) error) (*DeleteImageResult, error) {
	var canonPath, folderPath string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath, &isMissing); err != nil {
		return nil, fmt.Errorf("image not found: %w", err)
	}

	// removeAllTags prunes zero-usage tags scoped to this image's own tag
	// set, so we don't need a follow-up unscoped prune that could touch
	// unrelated rows.
	if err := removeAllTags(id); err != nil {
		logx.Warnf("remove tags for image %d: %v", id, err)
	}

	if _, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete image row: %w", err)
	}

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
