package gallery

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
)

// ApplyReplacedFile swaps an image's bytes for the staged file, keeping the
// row's identity (tags, sources, collections, relations, notes) while every
// content-derived column and artifact is re-derived: sha, size, dimensions,
// file/source type, side-table metadata, thumbnail, phash. Annotation boxes
// are scaled to the new dimensions - the old file is a resize of the same
// picture, so linear scaling holds.
//
// Ordering: the old file moves to a backup beside the staged one (both
// outside the watched tree), the DB commits pointing at the new state, then
// the staged file renames into the canonical path. The watcher event that
// fires sees a path whose sha the DB already carries and dedups to a no-op;
// either crash window is one rename wide and surfaces as the standard
// missing-file banner, with the backup restorable by hand.
func ApplyReplacedFile(database *db.DB, thumbnailsPath string, imageID int64, stagedPath, newSHA, newMD5, newType string) error {
	var oldPath string
	var oldW, oldH *int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, width, height FROM images WHERE id = ?`, imageID,
	).Scan(&oldPath, &oldW, &oldH); err != nil {
		return fmt.Errorf("load image %d: %w", imageID, err)
	}

	fi, err := os.Stat(stagedPath)
	if err != nil {
		return fmt.Errorf("stat staged file: %w", err)
	}
	// newType came from the pushed filename, which the client chose. The
	// bytes decide what the row records and what the file is renamed to.
	if actual, magicErr := detectMagicType(stagedPath); magicErr == nil {
		newType = actual
	}
	newW, newH := decodeImageDimensions(stagedPath)
	sdMeta, comfyMeta, sourceType := extractGenerationMeta(stagedPath, newType)

	// monbooru names this file, so it names it after the type it holds; an
	// extension that already claims that type is left as the operator spelled it.
	newPath := oldPath
	if newExt := ExtForFileType(newType); newExt != "" && ExtFileType(oldPath) != newType {
		stem := filepath.Base(oldPath)
		stem = stem[:len(stem)-len(filepath.Ext(stem))]
		newPath = UniqueDestPath(filepath.Dir(oldPath), stem+newExt)
	}

	backupPath := stagedPath + ".old"
	if err := moveIntoPlace(oldPath, backupPath); err != nil {
		return fmt.Errorf("move old file aside: %w", err)
	}

	commit := func() error {
		tx, err := database.Write.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(
			`UPDATE images SET sha256 = ?, md5 = ?, canonical_path = ?, file_type = ?, file_size = ?,
			        width = ?, height = ?, source_type = ?, is_missing = 0 WHERE id = ?`,
			newSHA, newMD5, newPath, newType, fi.Size(), toNullInt(newW), toNullInt(newH), sourceType, imageID,
		); err != nil {
			return fmt.Errorf("update images row: %w", err)
		}
		// Alias paths hold copies of the old bytes; the canonical row follows
		// the swap, the aliases no longer describe this row's content.
		if _, err := tx.Exec(`DELETE FROM image_paths WHERE image_id = ? AND is_canonical = 0`, imageID); err != nil {
			return fmt.Errorf("drop stale aliases: %w", err)
		}
		// The recorded lookup misses are about bytes this row no longer has.
		if err := lookup.DeleteForImage(tx, imageID); err != nil {
			return fmt.Errorf("drop lookup history: %w", err)
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET path = ?, mtime_unix = ?, mtime_nsec = ? WHERE image_id = ? AND is_canonical = 1`,
			newPath, fi.ModTime().Unix(), fi.ModTime().UnixNano(), imageID,
		); err != nil {
			return fmt.Errorf("update canonical path: %w", err)
		}
		if err := ReplaceGenerationMetadata(context.Background(), tx, imageID, sdMeta, comfyMeta); err != nil {
			return err
		}
		if oldW != nil && oldH != nil && newW != nil && newH != nil &&
			*oldW > 0 && *oldH > 0 && (*oldW != *newW || *oldH != *newH) {
			rw := float64(*newW) / float64(*oldW)
			rh := float64(*newH) / float64(*oldH)
			if _, err := tx.Exec(
				`UPDATE image_annotations SET
				   x = CAST(ROUND(x * ?) AS INTEGER), w = CAST(ROUND(w * ?) AS INTEGER),
				   y = CAST(ROUND(y * ?) AS INTEGER), h = CAST(ROUND(h * ?) AS INTEGER)
				 WHERE image_id = ?`, rw, rw, rh, rh, imageID,
			); err != nil {
				return fmt.Errorf("scale annotations: %w", err)
			}
		}
		return tx.Commit()
	}
	if err := commit(); err != nil {
		// The DB still describes the old bytes; put the old file back so
		// nothing changed at all.
		if rbErr := moveIntoPlace(backupPath, oldPath); rbErr != nil {
			logx.Warnf("replace: restore of %q failed after aborted swap: %v", oldPath, rbErr)
		}
		return err
	}

	// The staged file came from os.CreateTemp at 0600 and the rename
	// carries that through onto the gallery path, where every other
	// write path lands at what os.Create gives under the usual umask.
	if err := os.Chmod(stagedPath, 0o644); err != nil {
		logx.Warnf("replace: chmod staged file %q: %v", stagedPath, err)
	}
	if err := moveIntoPlace(stagedPath, newPath); err != nil {
		// The row already points at the new sha; keep the backup for hand
		// recovery and surface the standard missing-file state.
		return fmt.Errorf("place replaced file: %w", err)
	}
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		logx.Warnf("replace: removing backup %q: %v", backupPath, err)
	}

	if err := Generate(newPath, thumbnailsPath, imageID, newType); err != nil {
		logx.Warnf("replace: thumbnail regen for %q: %v", newPath, err)
		// The phash is computed off the thumbnail, so there is nothing to
		// rehash. Clearing it beats leaving the old bytes' value behind:
		// the backfill job picks up NULL phashes, a stale one it never
		// revisits.
		if _, err := database.Write.Exec(`UPDATE images SET phash = NULL WHERE id = ?`, imageID); err != nil {
			logx.Warnf("replace: clearing stale phash for %q: %v", newPath, err)
		}
	} else if err := RecomputeAndStorePhash(context.Background(), database, imageID, thumbnailsPath); err != nil {
		logx.Warnf("replace: phash recompute for %q: %v", newPath, err)
	}
	logx.Infof("replace: image id=%d now %q (sha %s)", imageID, newPath, newSHA)
	return nil
}

// moveIntoPlace renames src onto dst, falling back to copy-and-remove when
// the staging dir and the gallery tree sit on different filesystems (the
// data and gallery mounts need not share a device). The watcher's debounced
// ingest dedups the write event against the already-committed row, the same
// way a direct multipart upload lands.
func moveIntoPlace(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := CopyFileContents(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// CopyFileContents streams src to a new file at dst, unlinking a partial
// dst on failure. A copy rather than a rename, for the callers whose two
// paths can sit on different filesystems: the cross-device replace above
// and the transfer between gallery roots.
func CopyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}
