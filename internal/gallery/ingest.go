package gallery

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
)

// FolderPath computes the relative directory of filePath under
// galleryPath. Returns "" for files at the gallery root, and for paths
// that fall outside it. The result is always "/"-separated:
// folder_path is a DB value, not a filesystem path, so it must be
// portable across platforms.
func FolderPath(galleryPath, filePath string) string {
	// Rel cleans both sides, so a gallery_path configured with "/" still
	// matches the native separators the filesystem walk hands back.
	rel, err := filepath.Rel(galleryPath, filepath.Dir(filePath))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// Ingest processes a single file: hash, dimension probe, metadata
// extraction, DB insert, thumbnail. Returns (image, isDuplicate, error).
// origin records how the file got in ("ingest" / "upload" / caller-supplied
// string); empty defaults to "ingest".
func Ingest(database *db.DB, galleryPath, thumbnailsPath, path, origin string) (*models.Image, bool, error) {
	hash, sum, err := HashFileDigests(path)
	if err != nil {
		return nil, false, fmt.Errorf("hashing file: %w", err)
	}
	ClaimOwnership(path)
	return ingestWithHash(database, galleryPath, thumbnailsPath, path, hash, sum, origin)
}

// decodeImageDimensions reads just the header of the image at path and
// returns its dimensions, or nils when the file can't be opened or
// decoded - callers treat missing dimensions as non-fatal.
func decodeImageDimensions(path string) (w, h *int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, nil
	}
	return &cfg.Width, &cfg.Height
}

// ingestWithHash is the body of Ingest minus the HashFileDigests +
// ClaimOwnership preamble. Sync uses it directly to avoid double-hashing
// the same file on large libraries.
func ingestWithHash(database *db.DB, galleryPath, thumbnailsPath, path, hash, sum, origin string) (*models.Image, bool, error) {
	origin = cmp.Or(origin, models.OriginIngest)
	var existingID int64
	err := database.Read.QueryRow(
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
			return nil, true, fmt.Errorf("looking up duplicate image %d: %w", existingID, scanErr)
		}
		img.IsMissing = isMissingInt == 1

		if img.IsMissing {
			// Previously-missing file has reappeared; reactivate it.
			// Demote the previous canonical to alias and upsert the new
			// path as canonical (mirrors the watcher-mv branch below) so
			// the prior path stays in image_paths for history.
			newFolder := FolderPath(galleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, false, fmt.Errorf("begin reactivation tx: %w", txErr)
			}
			defer func() { _ = tx.Rollback() }()
			if err := repointCanonical(tx, existingID, path, newFolder, ""); err != nil {
				return nil, false, fmt.Errorf("reactivate image: %w", err)
			}
			if _, err := tx.Exec(`UPDATE images SET md5 = ? WHERE id = ?`, sum, existingID); err != nil {
				return nil, false, fmt.Errorf("reactivate image md5: %w", err)
			}
			// Restore the usage_count slots markFileMissing decremented
			// when the file vanished. usage_count tracks visible images,
			// so a reactivated row owes +1 to every tag still attached.
			if _, err := tx.Exec(
				`UPDATE tags SET usage_count = usage_count + 1
				 WHERE id IN (SELECT tag_id FROM image_tags WHERE image_id = ?)`,
				existingID,
			); err != nil {
				return nil, false, fmt.Errorf("restore usage counts: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, false, fmt.Errorf("commit reactivation: %w", err)
			}
			_ = Generate(path, thumbnailsPath, existingID, img.FileType)
			img.IsMissing = false
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: reactivated previously missing image id=%d path=%q", existingID, path)
			return &img, false, nil
		}

		// Watcher-observed mv inside the gallery: fsnotify emits a Create
		// for the new path while the old canonical_path is already gone
		// from disk. Promote the new path and drop the old one - a path
		// whose file no longer exists must not linger as an alias, or it
		// resurfaces as a phantom duplicate on the detail page.
		if _, statErr := os.Stat(img.CanonicalPath); statErr != nil {
			newFolder := FolderPath(galleryPath, path)
			tx, txErr := database.Write.Begin()
			if txErr != nil {
				return nil, false, fmt.Errorf("begin promote tx: %w", txErr)
			}
			defer func() { _ = tx.Rollback() }()
			if err := repointCanonical(tx, existingID, path, newFolder, img.CanonicalPath); err != nil {
				return nil, false, fmt.Errorf("promote canonical path: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, false, fmt.Errorf("commit promote: %w", err)
			}
			img.CanonicalPath = path
			img.FolderPath = newFolder
			logx.Infof("ingest: promoted alias to canonical for image id=%d (old path gone) %q", existingID, path)
			return &img, false, nil
		}

		// Normal duplicate: record this path as an alias.
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

	// DetectFileType trusts the extension so the sync walk stays free of
	// per-file reads, which leaves bytes that are not media at all - a
	// text file renamed .png - reaching here, where they would insert a
	// row with no dimensions, no thumbnail and no phash. The signature
	// decides both whether to commit a row and what type it records, so
	// a PNG saved as .jpg keeps its metadata and answers type:png.
	fileType, err := detectMagicType(path)
	if err != nil {
		logx.Warnf("ingest: skip %q: contents are not a supported media type", path)
		return nil, false, fmt.Errorf("ingest %q: %w", path, err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, false, fmt.Errorf("stat file: %w", err)
	}

	folderPath := FolderPath(galleryPath, path)

	// The path is already registered under a different SHA: the file was
	// rewritten where it stands (a crop in an external editor, a watcher
	// WRITE on a known file). image_paths.path is UNIQUE, so the insert
	// below would die on the constraint and roll the whole ingest back,
	// leaving the row describing bytes that are gone.
	var prevID int64
	var prevCanonical int
	switch pathErr := database.Read.QueryRow(
		`SELECT image_id, is_canonical FROM image_paths WHERE path = ?`, path,
	).Scan(&prevID, &prevCanonical); {
	case pathErr == nil && prevCanonical == 1:
		if editErr := applyInPlaceEdit(database, thumbnailsPath, path, hash, sum,
			fi.ModTime().Unix(), fi.ModTime().UnixNano(), fi.Size()); editErr != nil {
			return nil, false, editErr
		}
		return &models.Image{
			ID: prevID, SHA256: hash, MD5: sum, CanonicalPath: path, FolderPath: folderPath,
			FileType: fileType, FileSize: fi.Size(),
		}, false, nil
	case pathErr == nil:
		// An alias path whose bytes no longer match the image it was a copy
		// of. The row it pointed at still has its own file; this one is a
		// new image and needs the path.
		if _, delErr := database.Write.Exec(`DELETE FROM image_paths WHERE path = ?`, path); delErr != nil {
			return nil, false, fmt.Errorf("dropping stale alias path: %w", delErr)
		}
	case pathErr != sql.ErrNoRows:
		return nil, false, fmt.Errorf("checking image_paths: %w", pathErr)
	}

	var imgWidth, imgHeight *int
	var pageCount *int
	var durationSec *float64
	var prefilledSeries string
	var mangaMeta *models.MangaMetadata
	if IsVideoType(fileType) {
		if d, ok := ProbeDurationSeconds(path); ok {
			durationSec = &d
		}
		if w, h, ok := ProbeVideoDimensions(path); ok {
			imgWidth, imgHeight = &w, &h
		}
	}
	if fileType == models.FileTypeCBZ {
		archive, openErr := OpenManga(path)
		if openErr != nil {
			// Empty / corrupt cbz: log and skip without inserting a
			// row. The watcher / sync caller treats this as a
			// non-fatal "skip" the same way a video without ffmpeg is
			// treated - the file stays on disk for the operator.
			logx.Warnf("ingest: skip manga %q: %v", path, openErr)
			return nil, false, fmt.Errorf("ingest manga: %w", openErr)
		}
		w, h, dimErr := archive.CoverDimensions()
		if dimErr == nil {
			imgWidth, imgHeight = &w, &h
		} else {
			logx.Warnf("ingest manga %q: cover dimensions: %v", path, dimErr)
		}
		mm, mmErr := metadata.ParseComicInfo(archive.Reader())
		if mmErr != nil {
			logx.Warnf("ingest manga %q: ComicInfo: %v", path, mmErr)
		} else if mm != nil {
			mangaMeta = mm
			prefilledSeries = mm.Series
		}
		pcVal := len(archive.Pages)
		pageCount = &pcVal
		_ = archive.Close()
	} else if !IsVideoType(fileType) {
		imgWidth, imgHeight = decodeImageDimensions(path)
	}

	sdMeta, comfyMeta, sourceType := extractGenerationMeta(path, fileType)

	// ON CONFLICT(sha256) DO NOTHING so a concurrent ingest that wrote the
	// same SHA between our read-pool check and this transaction falls into
	// the duplicate branch instead of failing with a UNIQUE constraint.
	tx, err := database.Write.Begin()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var imgID int64
	insertErr := tx.QueryRow(
		`INSERT INTO images (sha256, md5, canonical_path, folder_path, file_type, width, height, file_size, source_type, origin, page_count, series, duration_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING
		 RETURNING id`,
		hash, sum, path, folderPath, fileType, toNullInt(imgWidth), toNullInt(imgHeight), fi.Size(), sourceType, origin, toNullInt(pageCount), prefilledSeries, toNullFloat(durationSec),
	).Scan(&imgID)

	if insertErr == sql.ErrNoRows {
		// Lost the race to another concurrent ingest. Record this path as
		// an alias of whichever id now owns the SHA and return a duplicate
		// result so the caller logs "duplicate" instead of an error.
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
			return nil, true, fmt.Errorf("race: reload existing image %d: %w", existingID, err)
		}
		img.IsMissing = isMissingInt == 1
		return &img, true, nil
	}
	if insertErr != nil {
		return nil, false, fmt.Errorf("inserting image: %w", insertErr)
	}

	if _, err := tx.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical, mtime_unix, mtime_nsec) VALUES (?, ?, 1, ?, ?)`,
		imgID, path, fi.ModTime().Unix(), fi.ModTime().UnixNano(),
	); err != nil {
		return nil, false, fmt.Errorf("inserting image_path: %w", err)
	}

	if sdMeta != nil {
		sdMeta.ImageID = imgID
		if err := insertSDMeta(context.Background(), tx, sdMeta); err != nil {
			return nil, false, fmt.Errorf("inserting sd_metadata: %w", err)
		}
	}
	if comfyMeta != nil {
		comfyMeta.ImageID = imgID
		if err := insertComfyMeta(context.Background(), tx, comfyMeta); err != nil {
			return nil, false, fmt.Errorf("inserting comfyui_metadata: %w", err)
		}
	}
	if mangaMeta != nil {
		mangaMeta.ImageID = imgID
		if err := insertMangaMeta(tx, mangaMeta); err != nil {
			return nil, false, fmt.Errorf("inserting manga_metadata: %w", err)
		}
	}
	// Mirror the prefilled home collection into image_collections.
	if prefilledSeries != "" {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_collections (image_id, name, position) VALUES (?, ?, NULL)`,
			imgID, prefilledSeries,
		); err != nil {
			return nil, false, fmt.Errorf("inserting image_collection: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("committing ingest: %w", err)
	}

	RegenerateDerived(database, thumbnailsPath, path, imgID, fileType, "ingest")

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
		Origin:        origin,
		PageCount:     pageCount,
		DurationSec:   durationSec,
		Series:        prefilledSeries,
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

func toNullFloat(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

// extractGenerationMeta reads whichever generation blobs the file
// carries and classifies the pair into the source_type the row records.
// Every path that rewrites a file's bytes owes the row all three.
func extractGenerationMeta(path, fileType string) (*models.SDMetadata, *models.ComfyUIMetadata, string) {
	sd, comfy, _ := metadata.Extract(path, fileType)
	switch {
	case sd != nil && comfy != nil:
		return sd, comfy, models.SourceTypeBoth
	case sd != nil:
		return sd, comfy, models.SourceTypeA1111
	case comfy != nil:
		return sd, comfy, models.SourceTypeComfyUI
	}
	return sd, comfy, models.SourceTypeNone
}

// ReplaceGenerationMetadata swaps the sd / comfyui side-table rows for
// imageID. An edit, a replace or a re-extract can add a recipe, change it,
// or strip one, so the old rows go whether or not new ones arrive - which
// also makes the inserts' OR IGNORE moot here, on a single-writer DB.
func ReplaceGenerationMetadata(ctx context.Context, tx *sql.Tx, imageID int64, sd *models.SDMetadata, comfy *models.ComfyUIMetadata) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sd_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear sd_metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM comfyui_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear comfyui_metadata: %w", err)
	}
	if sd != nil {
		sd.ImageID = imageID
		if err := insertSDMeta(ctx, tx, sd); err != nil {
			return fmt.Errorf("insert sd_metadata: %w", err)
		}
	}
	if comfy != nil {
		comfy.ImageID = imageID
		if err := insertComfyMeta(ctx, tx, comfy); err != nil {
			return fmt.Errorf("insert comfyui_metadata: %w", err)
		}
	}
	return nil
}

func insertSDMeta(ctx context.Context, tx *sql.Tx, sd *models.SDMetadata) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sd.ImageID, sd.Prompt, sd.NegativePrompt, sd.Model, sd.Seed, sd.Sampler, sd.Steps, sd.CFGScale, sd.RawParams, sd.GenerationHash,
	)
	return err
}

func insertComfyMeta(ctx context.Context, tx *sql.Tx, comfy *models.ComfyUIMetadata) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		comfy.ImageID, comfy.Prompt, comfy.ModelCheckpoint, comfy.Seed, comfy.Sampler, comfy.Steps, comfy.CFGScale, comfy.RawWorkflow, comfy.GenerationHash,
	)
	return err
}

func insertMangaMeta(tx *sql.Tx, m *models.MangaMetadata) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO manga_metadata (image_id, title, series, number, volume, count, summary, notes,
		     year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		     imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ImageID, m.Title, m.Series, m.Number, m.Volume,
		toNullInt(m.Count), m.Summary, m.Notes,
		toNullInt(m.Year), toNullInt(m.Month), toNullInt(m.Day),
		m.Writer, m.Penciller, m.Inker, m.Colorist, m.Letterer, m.CoverArtist, m.Editor, m.Publisher,
		m.Imprint, m.Genre, m.Web, m.LanguageISO, m.Format, m.Manga, m.AgeRating,
		toNullFloat(m.CommunityRating), toNullInt(m.XMLPageCount), m.RawXML,
	)
	return err
}

// DropDuplicateCopy removes the file and the alias row an ingest recorded for
// bytes the gallery already holds under another path. A re-uploaded archive
// would otherwise cost its own size again with no UI to reclaim it. logCtx
// names the caller in the warnings; failures are logged, never fatal, since
// the row the caller keeps is already correct.
func DropDuplicateCopy(database *db.DB, imageID int64, path, logCtx string) {
	if _, err := database.Write.Exec(
		`DELETE FROM image_paths WHERE image_id = ? AND path = ? AND is_canonical = 0`,
		imageID, path,
	); err != nil {
		logx.Warnf("%s: drop duplicate alias for %q: %v", logCtx, path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logx.Warnf("%s: remove duplicate copy %q: %v", logCtx, path, err)
	}
}
