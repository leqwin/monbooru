package gallery

import (
	"database/sql"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/metadata"
	"github.com/leqwin/monbooru/internal/models"
)

// FolderPath computes the relative folder path from galleryPath to the file's directory.
// Returns "" for files in the gallery root.
// Assumes Linux forward-slash paths.
func FolderPath(galleryPath, filePath string) string {
	dir := filepath.Dir(filePath)
	if dir == "." {
		return ""
	}
	rel := strings.TrimPrefix(dir, galleryPath)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "." {
		return ""
	}
	return rel
}

// Ingest processes a single file: hashes it, inserts into DB, extracts metadata, generates thumbnail.
// Returns (image, isDuplicate, error).
func Ingest(database *db.DB, cfg *config.Config, path, fileType string) (*models.Image, bool, error) {
	hash, err := HashFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("hashing file: %w", err)
	}

	// Take ownership so later cleanup (delete, rename, move) doesn't trip on
	// EACCES when the file was originally written by another user.
	ClaimOwnership(path)

	var existingID int64
	err = database.Read.QueryRow(
		`SELECT id FROM images WHERE sha256 = ?`, hash,
	).Scan(&existingID)

	if err == nil {
		var img models.Image
		var isMissingInt int
		scanErr := database.Read.QueryRow(
			`SELECT id, sha256, canonical_path, folder_path, file_type, file_size, is_missing FROM images WHERE id = ?`,
			existingID,
		).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType, &img.FileSize, &isMissingInt)
		if scanErr != nil {
			return nil, true, nil
		}
		img.IsMissing = isMissingInt == 1

		if img.IsMissing {
			// File was previously marked missing and has now reappeared — re-activate it.
			newFolder := FolderPath(cfg.Paths.GalleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, false, fmt.Errorf("begin reactivation tx: %w", txErr)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(
				`UPDATE images SET is_missing = 0, canonical_path = ?, folder_path = ? WHERE id = ?`,
				path, newFolder, existingID,
			); err != nil {
				return nil, false, fmt.Errorf("reactivate image: %w", err)
			}
			if _, err := tx.Exec(
				`UPDATE image_paths SET path = ?, is_canonical = 1 WHERE image_id = ? AND is_canonical = 1`,
				path, existingID,
			); err != nil {
				return nil, false, fmt.Errorf("update canonical path: %w", err)
			}
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)`,
				existingID, path,
			); err != nil {
				return nil, false, fmt.Errorf("insert path row: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, false, fmt.Errorf("commit reactivation: %w", err)
			}
			Generate(path, cfg.Paths.ThumbnailsPath, existingID, img.FileType)
			img.IsMissing = false
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: reactivated previously missing image id=%d path=%q", existingID, path)
			return &img, false, nil
		}

		// Normal duplicate — insert alias path row
		_, aliasErr := database.Write.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
			existingID, path,
		)
		if aliasErr != nil {
			logx.Warnf("ingest alias: %v", aliasErr)
		}
		return &img, true, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, fmt.Errorf("checking sha256: %w", err)
	}

	// New file — gather info
	fi, err := os.Stat(path)
	if err != nil {
		return nil, false, fmt.Errorf("stat file: %w", err)
	}

	folderPath := FolderPath(cfg.Paths.GalleryPath, path)

	var imgWidth, imgHeight *int
	if !IsVideoType(fileType) {
		f, openErr := os.Open(path)
		if openErr == nil {
			if cfg2, _, decErr := image.DecodeConfig(f); decErr == nil {
				w, h := cfg2.Width, cfg2.Height
				imgWidth, imgHeight = &w, &h
			}
			f.Close()
		}
	}

	sdMeta, comfyMeta, _ := metadata.Extract(path, fileType)
	sourceType := models.SourceTypeNone
	if sdMeta != nil && comfyMeta != nil {
		sourceType = models.SourceTypeBoth
	} else if sdMeta != nil {
		sourceType = models.SourceTypeA1111
	} else if comfyMeta != nil {
		sourceType = models.SourceTypeComfyUI
	}

	// ON CONFLICT(sha256) DO NOTHING so a concurrent ingest that wrote the same SHA
	// between our read-pool check and this transaction falls into the duplicate
	// branch instead of failing the handler with a UNIQUE constraint error.
	tx, err := database.Write.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var imgID int64
	insertErr := tx.QueryRow(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, width, height, file_size, source_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING
		 RETURNING id`,
		hash, path, folderPath, fileType, toNullInt(imgWidth), toNullInt(imgHeight), fi.Size(), sourceType,
	).Scan(&imgID)

	if insertErr == sql.ErrNoRows {
		// Lost the race to another concurrent ingest. Add our path as an
		// alias pointing at whichever id now owns that SHA and return a
		// duplicate result so the caller logs "duplicate" instead of error.
		var existingID int64
		if err := tx.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, hash).Scan(&existingID); err != nil {
			return nil, false, fmt.Errorf("race: fetch existing sha: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
			existingID, path,
		); err != nil {
			return nil, false, fmt.Errorf("race: insert alias path: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, false, fmt.Errorf("race: commit alias: %w", err)
		}
		// Reload the existing image record so callers see the real state.
		var img models.Image
		var isMissingInt int
		if err := database.Read.QueryRow(
			`SELECT id, sha256, canonical_path, folder_path, file_type, file_size, is_missing FROM images WHERE id = ?`,
			existingID,
		).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType, &img.FileSize, &isMissingInt); err != nil {
			return nil, true, nil
		}
		img.IsMissing = isMissingInt == 1
		return &img, true, nil
	}
	if insertErr != nil {
		return nil, false, fmt.Errorf("inserting image: %w", insertErr)
	}

	if _, err := tx.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)`,
		imgID, path,
	); err != nil {
		return nil, false, fmt.Errorf("inserting image_path: %w", err)
	}

	if sdMeta != nil {
		sdMeta.ImageID = imgID
		if err := insertSDMeta(tx, sdMeta); err != nil {
			return nil, false, fmt.Errorf("inserting sd_metadata: %w", err)
		}
	}
	if comfyMeta != nil {
		comfyMeta.ImageID = imgID
		if err := insertComfyMeta(tx, comfyMeta); err != nil {
			return nil, false, fmt.Errorf("inserting comfyui_metadata: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("committing ingest: %w", err)
	}

	if err := Generate(path, cfg.Paths.ThumbnailsPath, imgID, fileType); err != nil {
		logx.Warnf("thumbnail generation failed for %q: %v", path, err)
	}

	img := &models.Image{
		ID:            imgID,
		SHA256:        hash,
		CanonicalPath: path,
		FolderPath:    folderPath,
		FileType:      fileType,
		Width:         imgWidth,
		Height:        imgHeight,
		FileSize:      fi.Size(),
		SourceType:    sourceType,
		IngestedAt:    time.Now().UTC(),
	}
	return img, false, nil
}

func toNullInt(v *int) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func insertSDMeta(tx *sql.Tx, sd *models.SDMetadata) error {
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sd.ImageID, sd.Prompt, sd.NegativePrompt, sd.Model, sd.Seed, sd.Sampler, sd.Steps, sd.CFGScale, sd.RawParams, sd.GenerationHash,
	)
	return err
}

func insertComfyMeta(tx *sql.Tx, comfy *models.ComfyUIMetadata) error {
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		comfy.ImageID, comfy.Prompt, comfy.ModelCheckpoint, comfy.Seed, comfy.Sampler, comfy.Steps, comfy.CFGScale, comfy.RawWorkflow, comfy.GenerationHash,
	)
	return err
}
