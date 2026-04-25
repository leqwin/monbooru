package gallery

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
)

// MoveImageResult summarises a successful move so the caller can clean up the
// old parent directory and invalidate caches without re-querying.
type MoveImageResult struct {
	OldCanonicalPath string
	OldFolderPath    string
	NewCanonicalPath string
	NewFolderPath    string
}

// MoveImage relocates the canonical file of image id into targetFolder
// (relative to galleryPath). Filename collisions auto-suffix via
// UniqueDestPath, matching the upload and API paths. Callers that hold a
// watcher should gate this under a job type the watcher suppresses,
// otherwise the resulting CREATE/REMOVE events race with the DB update.
func MoveImage(database *db.DB, galleryPath string, id int64, targetFolder string) (*MoveImageResult, error) {
	var oldCanonical, oldFolder string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&oldCanonical, &oldFolder, &isMissing); err != nil {
		return nil, fmt.Errorf("image %d not found: %w", id, err)
	}
	if isMissing == 1 {
		return nil, fmt.Errorf("image %d is missing from disk", id)
	}

	destDir, err := ResolveSubdir(galleryPath, targetFolder)
	if err != nil {
		return nil, err
	}
	newFolder, err := filepath.Rel(galleryPath, destDir)
	if err != nil {
		return nil, fmt.Errorf("resolve folder: %w", err)
	}
	if newFolder == "." {
		newFolder = ""
	}

	if newFolder == oldFolder {
		return &MoveImageResult{
			OldCanonicalPath: oldCanonical,
			OldFolderPath:    oldFolder,
			NewCanonicalPath: oldCanonical,
			NewFolderPath:    oldFolder,
		}, nil
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create destination folder: %w", err)
	}

	newPath := UniqueDestPath(destDir, filepath.Base(oldCanonical))

	// DB first: once the row points at newPath the watcher's REMOVE on
	// oldCanonical matches no row, and its CREATE on newPath collapses
	// into an INSERT OR IGNORE against the already-canonical row. Move-job
	// suppression in the watcher is belt-and-braces on top of that.
	tx, err := database.Write.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin move tx: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
		newPath, newFolder, id,
	); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
		newPath, id,
	); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("update image_paths row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit move tx: %w", err)
	}

	if err := os.Rename(oldCanonical, newPath); err != nil {
		// Revert the DB rows so a retry sees consistent state. A revert
		// failure leaves the library inconsistent, hence the loud log.
		if _, rbErr := database.Write.Exec(
			`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
			oldCanonical, oldFolder, id,
		); rbErr != nil {
			logx.Errorf("move: restore images row for %d: %v (original: %v)", id, rbErr, err)
		}
		if _, rbErr := database.Write.Exec(
			`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
			oldCanonical, id,
		); rbErr != nil {
			logx.Errorf("move: restore image_paths row for %d: %v (original: %v)", id, rbErr, err)
		}
		return nil, fmt.Errorf("rename file: %w", err)
	}

	return &MoveImageResult{
		OldCanonicalPath: oldCanonical,
		OldFolderPath:    oldFolder,
		NewCanonicalPath: newPath,
		NewFolderPath:    newFolder,
	}, nil
}
