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
// (relative to galleryPath). On a filename collision within the target folder
// the file is renamed with `_1`, `_2`, … via UniqueDestPath, mirroring the
// upload and API create paths so two callers don't drift on collision rules.
// Callers that hold a watcher for galleryPath should gate this under a job
// whose type is in the watcher's suppression list, otherwise the resulting
// CREATE/REMOVE events race with the DB update.
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
		// No-op: image already lives in the target folder.
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

	// DB first: once the row's canonical_path and image_paths entry point at
	// newPath, the subsequent REMOVE event the watcher sees on oldCanonical
	// won't match any row, and the CREATE event on newPath collapses into a
	// duplicate INSERT OR IGNORE against an already-canonical row. Watcher
	// suppression during a `move` job belt-and-braces this, but the ordering
	// stands on its own too.
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
		// Revert the DB rows so a retry sees consistent state. Errors on the
		// revert itself are rare (same rows, same connection) but worth a
		// loud log because the library is now inconsistent.
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
