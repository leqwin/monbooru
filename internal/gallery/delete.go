package gallery

import (
	"fmt"
	"os"

	"github.com/leqwin/monbooru/internal/config"
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
func DeleteImage(database *db.DB, cfg *config.Config, id int64, removeAllTags func(int64) error) (*DeleteImageResult, error) {
	var canonPath, folderPath string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath, &isMissing); err != nil {
		return nil, fmt.Errorf("image not found: %w", err)
	}

	// Remove all tags (co-occurrence cleanup) before deleting the row.
	if err := removeAllTags(id); err != nil {
		logx.Warnf("remove tags for image %d: %v", id, err)
	}

	// Delete DB row and prune orphaned tags in a transaction.
	tx, err := database.Write.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin delete tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM images WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete image row: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM tags WHERE usage_count <= 0`); err != nil {
		return nil, fmt.Errorf("prune orphaned tags: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delete tx: %w", err)
	}

	// File cleanup (after successful DB commit).
	os.Remove(ThumbnailPath(cfg.Paths.ThumbnailsPath, id))
	os.Remove(HoverPath(cfg.Paths.ThumbnailsPath, id))

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
