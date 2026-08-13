package gallery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

// MoveImageResult reports the new location of a moved image so the caller
// can render it and invalidate caches without re-querying.
type MoveImageResult struct {
	NewCanonicalPath string
	NewFolderPath    string
}

// MoveImage relocates the canonical file of image id into targetFolder
// (relative to galleryPath). Filename collisions auto-suffix via
// UniqueDestPath, matching the upload and API paths. Callers that hold a
// watcher should gate this under a job type the watcher suppresses,
// otherwise the resulting CREATE/REMOVE events race with the DB update.
func MoveImage(database *db.DB, galleryPath string, id int64, targetFolder string) (*MoveImageResult, error) {
	// The destination is already root-bounded by ResolveSubdir.
	oldCanonical, oldFolder, err := loadMoveSource(database, galleryPath, id, "move")
	if err != nil {
		return nil, err
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
	// folder_path is stored "/"-separated on every platform.
	newFolder = filepath.ToSlash(newFolder)

	if newFolder == oldFolder {
		return &MoveImageResult{
			NewCanonicalPath: oldCanonical,
			NewFolderPath:    oldFolder,
		}, nil
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create destination folder: %w", err)
	}

	newPath := UniqueDestPath(destDir, filepath.Base(oldCanonical))

	// UniqueDestPath only checks the filesystem, not image_paths. A stale
	// alias row for a different image (file long gone but row never
	// pruned) would otherwise trip the UNIQUE constraint on path mid-tx
	// with no useful diagnostic. Surface the collision up front so the
	// caller can suggest "prune duplicate paths" from the Settings
	// maintenance page.
	if err := refuseAliasCollision(database, id, newPath); err != nil {
		return nil, err
	}
	if err := commitRename(database, "move", id, oldCanonical, newPath, &newFolder); err != nil {
		return nil, err
	}

	return &MoveImageResult{
		NewCanonicalPath: newPath,
		NewFolderPath:    newFolder,
	}, nil
}

// RenameImage renames the canonical file of image id in place: the
// folder stays, the basename becomes newName with the original
// extension preserved. Collisions auto-suffix via uniqueRenamePath and
// the same watcher-suppression caveat as MoveImage applies.
func RenameImage(database *db.DB, galleryPath string, id int64, newName string) (*MoveImageResult, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" || newName != filepath.Base(newName) || newName == "." || newName == ".." {
		return nil, fmt.Errorf("invalid file name %q", newName)
	}
	oldCanonical, oldFolder, err := loadMoveSource(database, galleryPath, id, "rename")
	if err != nil {
		return nil, err
	}

	if ext := filepath.Ext(oldCanonical); !strings.EqualFold(filepath.Ext(newName), ext) {
		newName += ext
	}
	if newName == filepath.Base(oldCanonical) {
		return &MoveImageResult{
			NewCanonicalPath: oldCanonical,
			NewFolderPath:    oldFolder,
		}, nil
	}

	newPath := uniqueRenamePath(filepath.Dir(oldCanonical), newName)

	if err := refuseAliasCollision(database, id, newPath); err != nil {
		return nil, err
	}
	if err := commitRename(database, "rename", id, oldCanonical, newPath, nil); err != nil {
		return nil, err
	}

	return &MoveImageResult{
		NewCanonicalPath: newPath,
		NewFolderPath:    oldFolder,
	}, nil
}

// loadMoveSource reads the row a move or rename acts on and refuses what
// neither can handle: a file already gone from disk, and a canonical_path
// that drifted outside the gallery root (mirroring DeleteImage). verb names
// the refused action in the error.
func loadMoveSource(database *db.DB, galleryPath string, id int64, verb string) (oldCanonical, oldFolder string, err error) {
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&oldCanonical, &oldFolder, &isMissing); err != nil {
		return "", "", fmt.Errorf("image %d not found: %w", id, err)
	}
	if isMissing == 1 {
		return "", "", fmt.Errorf("image %d is missing from disk", id)
	}
	if galleryPath != "" && !PathInside(galleryPath, oldCanonical) {
		return "", "", fmt.Errorf("refusing to %s %q outside gallery root %q", verb, oldCanonical, galleryPath)
	}
	return oldCanonical, oldFolder, nil
}

// refuseAliasCollision rejects a destination another image already records
// as an alias: a stale image_paths row would otherwise trip the UNIQUE
// constraint mid-tx with no useful diagnostic.
func refuseAliasCollision(database *db.DB, id int64, newPath string) error {
	var collidingImage int64
	if err := database.Read.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ? AND image_id != ?`,
		newPath, id,
	).Scan(&collidingImage); err == nil {
		return fmt.Errorf("destination collides with an existing alias on image %d", collidingImage)
	}
	return nil
}

// commitRename repoints both path rows and renames the file inside the open
// tx, so a rename failure rolls the row updates back automatically. The
// watcher suppresses events while the job runs, so the window where newPath
// exists on disk before the commit does not race a concurrent ingest. A
// commit failure (rare - SQLite COMMIT is essentially an fsync) reverses the
// rename; if that fails too the library is wedged and needs a manual sync.
// newFolder nil leaves folder_path alone, which is what a rename in place
// wants.
func commitRename(database *db.DB, verb string, id int64, oldCanonical, newPath string, newFolder *string) error {
	tx, err := database.Write.Begin()
	if err != nil {
		return fmt.Errorf("begin %s tx: %w", verb, err)
	}
	update, args := `UPDATE images SET canonical_path = ? WHERE id = ?`, []any{newPath, id}
	if newFolder != nil {
		update = `UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`
		args = []any{newPath, *newFolder, id}
	}
	if _, err := tx.Exec(update, args...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
		newPath, id,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update image_paths row: %w", err)
	}
	if err := os.Rename(oldCanonical, newPath); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("rename file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if rnErr := os.Rename(newPath, oldCanonical); rnErr != nil {
			logx.Errorf("%s: reverse rename for %d after commit fail: %v (original: %v)", verb, id, rnErr, err)
		}
		return fmt.Errorf("commit %s tx: %w", verb, err)
	}
	return nil
}

// uniqueRenamePath returns dir/filename if free, else appends a
// zero-padded counter to the stem (name01.png, name02.png, ...) so
// rename collisions read like the batch rename's numbered sequence
// instead of UniqueDestPath's `_N` upload suffixes.
func uniqueRenamePath(dir, filename string) string {
	return uniquePathBy(dir, filename, func(stem, ext string, i int) string {
		return fmt.Sprintf("%s%02d%s", stem, i, ext)
	})
}
