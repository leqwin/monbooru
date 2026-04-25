package web

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/tags"
)

// lightManifestImage is one record inside the tags.json manifest of a light
// export. Tags use "name" for the general category and "category:name" for
// everything else so category attribution round-trips without extra fields.
type lightManifestImage struct {
	SHA256 string   `json:"sha256"`
	Path   string   `json:"path"`
	Tags   []string `json:"tags"`
}

// lightManifest is the root JSON document at tags.json inside a light zip.
type lightManifest struct {
	Version int                  `json:"version"`
	Images  []lightManifestImage `json:"images"`
}

// ExportGalleryLight streams a zip containing gallery/<rel> image files plus
// a tags.json manifest listing {sha256, path, tags} for each non-missing
// image. The archive omits monbooru-specific data (SD/ComfyUI metadata,
// saved searches, tag attribution), keeping it useful as a portable bundle
// that other software can read or produce.
func (s *Server) ExportGalleryLight(name string, w io.Writer) error {
	cx := s.Get(name)
	if cx == nil {
		return fmt.Errorf("unknown gallery %q", name)
	}
	zw := zip.NewWriter(w)
	defer zw.Close()

	inner, err := zw.CreateHeader(&zip.FileHeader{Name: "tags.json", Method: zip.Deflate})
	if err != nil {
		return err
	}
	if err := writeLightManifest(cx, inner); err != nil {
		return err
	}

	// Gallery files; mirrors ExportGalleryArchive's layout and Store method.
	return writeGalleryFilesToZip(zw, cx.GalleryPath)
}

// ExportGalleryLightManifest streams the same tags.json document as
// ExportGalleryLight but without the surrounding zip and without the gallery
// files. Used by the export handler when the user picks the light format
// without "Include image files".
func (s *Server) ExportGalleryLightManifest(name string, w io.Writer) error {
	cx := s.Get(name)
	if cx == nil {
		return fmt.Errorf("unknown gallery %q", name)
	}
	return writeLightManifest(cx, w)
}

// writeLightManifest streams the tags.json document using the existing
// jsonWriter so memory stays bounded to one image's tag list at a time.
func writeLightManifest(cx *galleryCtx, w io.Writer) error {
	bw := newJSONWriter(w)
	bw.objStart()
	bw.field("version", galleryExportVersion)
	bw.arrayStart("images")

	rows, err := cx.DB.Read.Query(`
		SELECT i.id, i.sha256, i.folder_path, i.canonical_path
		FROM images i WHERE i.is_missing = 0 ORDER BY i.id`)
	if err != nil {
		bw.arrayEnd()
		bw.objEnd()
		return err
	}
	type imgRow struct {
		id                     int64
		sha, folder, canonical string
	}
	var imgs []imgRow
	for rows.Next() {
		var r imgRow
		if err := rows.Scan(&r.id, &r.sha, &r.folder, &r.canonical); err != nil {
			rows.Close()
			bw.arrayEnd()
			bw.objEnd()
			return err
		}
		imgs = append(imgs, r)
	}
	rows.Close()

	first := true
	for _, r := range imgs {
		tagRows, err := cx.DB.Read.Query(`
			SELECT t.name, tc.name FROM image_tags it
			JOIN tags t ON t.id = it.tag_id
			JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id = ? AND t.is_alias = 0
			ORDER BY tc.name, t.name`, r.id)
		if err != nil {
			bw.arrayEnd()
			bw.objEnd()
			return err
		}
		tagsList := []string{}
		for tagRows.Next() {
			var tname, tcat string
			if err := tagRows.Scan(&tname, &tcat); err != nil {
				tagRows.Close()
				bw.arrayEnd()
				bw.objEnd()
				return err
			}
			if tcat == "general" {
				tagsList = append(tagsList, tname)
			} else {
				tagsList = append(tagsList, tcat+":"+tname)
			}
		}
		tagRows.Close()
		rel := filepath.ToSlash(filepath.Join(r.folder, filepath.Base(r.canonical)))
		bw.arrayItem(&first, lightManifestImage{SHA256: r.sha, Path: rel, Tags: tagsList})
	}
	bw.arrayEnd()
	bw.objEnd()
	return bw.err
}

// replaceFromLightArchive wipes the target gallery (db, thumbnails, source
// folder) and rebuilds from a light zip: a fresh db gets bootstrapped,
// every gallery/<rel> file is extracted into the target gallery, then each
// image is ingested and tagged from the manifest. Shares its per-record
// ingest loop with the merge path.
func replaceFromLightArchive(manifest *zip.File, galleryFiles []*zip.File, dbPath, thumbsPath, galleryPath string) error {
	mc, err := manifest.Open()
	if err != nil {
		return fmt.Errorf("open tags.json: %w", err)
	}
	var mf lightManifest
	err = json.NewDecoder(mc).Decode(&mf)
	mc.Close()
	if err != nil {
		return fmt.Errorf("decode tags.json: %w", err)
	}
	if mf.Version != galleryExportVersion {
		return fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, galleryExportVersion)
	}

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if err := os.RemoveAll(thumbsPath); err != nil {
		return fmt.Errorf("clear thumbnails: %w", err)
	}
	if err := os.MkdirAll(thumbsPath, 0o755); err != nil {
		return fmt.Errorf("recreate thumbnails dir: %w", err)
	}
	if err := wipeDirContents(galleryPath); err != nil {
		return fmt.Errorf("wipe gallery: %w", err)
	}

	for _, f := range galleryFiles {
		rel := strings.TrimPrefix(f.Name, "gallery/")
		dst, err := safeArchiveDest(galleryPath, rel)
		if err != nil {
			return fmt.Errorf("rejecting archive entry %q: %w", f.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyZipFile(f, dst); err != nil {
			return err
		}
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open new db: %w", err)
	}
	defer database.Close()
	if err := db.Bootstrap(database); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	ingestLightManifestEntries(database, galleryPath, thumbsPath, mf.Images)
	return nil
}

// replaceFromLightManifest is the no-images variant of
// replaceFromLightArchive: the uploaded file is a bare tags.json, so the
// gallery's on-disk files stay in place. The db is wiped and rebuilt by
// ingesting only those manifest entries whose path resolves to an existing
// file under galleryPath. Entries with no matching file on disk are dropped
// with a warning.
func replaceFromLightManifest(srcPath, dbPath, thumbsPath, galleryPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open tags.json: %w", err)
	}
	var mf lightManifest
	err = json.NewDecoder(f).Decode(&mf)
	f.Close()
	if err != nil {
		return fmt.Errorf("decode tags.json: %w", err)
	}
	if mf.Version != galleryExportVersion {
		return fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, galleryExportVersion)
	}

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if err := os.RemoveAll(thumbsPath); err != nil {
		return fmt.Errorf("clear thumbnails: %w", err)
	}
	if err := os.MkdirAll(thumbsPath, 0o755); err != nil {
		return fmt.Errorf("recreate thumbnails dir: %w", err)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open new db: %w", err)
	}
	defer database.Close()
	if err := db.Bootstrap(database); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	ingestLightManifestEntries(database, galleryPath, thumbsPath, mf.Images)
	return nil
}

// ingestLightManifestEntries walks each manifest entry, stats the matching
// file under galleryPath, and on success ingests it and applies its tags.
// Entries whose file isn't on disk are still recorded as is_missing=1 rows
// so the manifest's tags are preserved and the user can spot the gap with
// the missing:true filter, mirroring how Sync flags vanished files.
// Shared by the light-zip and light-json replace paths so both end up with
// identical row shapes in the freshly bootstrapped db.
func ingestLightManifestEntries(database *db.DB, galleryPath, thumbsPath string, entries []lightManifestImage) {
	tagSvc := tags.New(database)
	generalID := lookupCategoryID(database, "general")
	for _, r := range entries {
		path, err := safeArchiveDest(galleryPath, r.Path)
		if err != nil {
			logx.Warnf("light import: skipping entry %q: %v", r.Path, err)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			imgID, err := insertMissingImageRow(database, r.SHA256, path, galleryPath)
			if err != nil {
				logx.Warnf("light import: record missing %q: %v", r.Path, err)
				continue
			}
			applyImportTagsToImage(database, tagSvc, imgID, r.Tags, generalID)
			continue
		}
		ft, err := gallery.DetectFileType(path)
		if err != nil {
			logx.Warnf("light import: unsupported file %q: %v", r.Path, err)
			continue
		}
		img, _, err := gallery.Ingest(database, galleryPath, thumbsPath, path, ft, models.OriginIngest)
		if err != nil {
			logx.Warnf("light import: ingest %q: %v", r.Path, err)
			continue
		}
		applyImportTagsToImage(database, tagSvc, img.ID, r.Tags, generalID)
	}
}

// insertMissingImageRow records a manifest entry whose file isn't on disk.
// File type comes from the extension (DetectFileType's magic-byte fallback
// can't run without a readable file); unknown extensions surface as an
// error so the caller logs+skips. Width/height stay NULL since we never
// decoded the image.
func insertMissingImageRow(database *db.DB, sha, path, galleryPath string) (int64, error) {
	ft, err := gallery.DetectFileType(path)
	if err != nil {
		return 0, err
	}
	if sha == "" {
		return 0, fmt.Errorf("manifest entry has empty sha256")
	}
	folder := gallery.FolderPath(galleryPath, path)
	tx, err := database.Write.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size,
		                    is_missing, source_type, origin)
		 VALUES (?, ?, ?, ?, 0, 1, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING
		 RETURNING id`,
		sha, path, folder, ft, models.SourceTypeNone, models.OriginIngest,
	).Scan(&id)
	if err == sql.ErrNoRows {
		if err := tx.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, sha).Scan(&id); err != nil {
			return 0, fmt.Errorf("fetch existing sha: %w", err)
		}
	} else if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)`,
		id, path,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// mergeRecord is a single image worth of data in a non-destructive import.
// SourcePath and zipEntry are populated for zip uploads so new images can be
// brought into the target gallery; db/json-only uploads leave them empty and
// the record only applies tags to an already-existing image matched by SHA.
type mergeRecord struct {
	SHA256     string
	Tags       []string
	SourcePath string    // relative path under gallery/; "" when no file is provided
	zipEntry   *zip.File // when set, extract into galleryPath/<unique SourcePath>
}

// MergeGallery additively brings images and tags from the uploaded file into
// the named gallery. Unlike ImportGallery it does not wipe anything and is
// permitted on the active and default galleries. db and json uploads apply
// tags to existing images matched by SHA; zip uploads (full or light) also
// ingest new images when the archive carries their files.
func (s *Server) MergeGallery(name, format string, upload io.Reader) error {
	if s.jobs.IsRunning() {
		return errJobRunning
	}
	s.ctxMu.Lock()
	cx, ok := s.contexts[name]
	s.ctxMu.Unlock()
	if !ok {
		return fmt.Errorf("unknown gallery %q", name)
	}
	dataDir := filepath.Dir(cx.DBPath)

	tmp, err := os.CreateTemp(dataDir, "merge-*.upload")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, upload); err != nil {
		tmp.Close()
		return fmt.Errorf("buffer upload: %w", err)
	}
	tmp.Close()

	var mergeErr error
	switch format {
	case "db":
		mergeErr = mergeFromDB(cx, tmpPath)
	case "json":
		// Disambiguate full monbooru export vs bare light tags.json with
		// the same probe used on the replace path.
		isLight, err := isLightManifestJSON(tmpPath)
		if err != nil {
			mergeErr = fmt.Errorf("inspect json: %w", err)
			break
		}
		if isLight {
			mergeErr = mergeFromLightJSON(cx, tmpPath)
		} else {
			mergeErr = mergeFromJSON(cx, tmpPath)
		}
	case "zip":
		mergeErr = mergeFromZip(cx, tmpPath)
	default:
		mergeErr = fmt.Errorf("unknown merge format %q", format)
	}
	if mergeErr == nil {
		cx.InvalidateCaches()
		logx.Infof("gallery: merged into %q (format=%s)", name, format)
	}
	return mergeErr
}

func mergeFromDB(cx *galleryCtx, tmpPath string) error {
	if err := validateSQLiteFile(tmpPath); err != nil {
		return fmt.Errorf("uploaded file is not a valid monbooru database: %w", err)
	}
	src, err := db.Open(tmpPath)
	if err != nil {
		return err
	}
	defer src.Close()
	records, err := readDBMergeRecords(src)
	if err != nil {
		return err
	}
	applyMergeRecords(cx, records)
	return nil
}

func mergeFromJSON(cx *galleryCtx, tmpPath string) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open json: %w", err)
	}
	defer f.Close()
	var exp galleryExport
	if err := json.NewDecoder(f).Decode(&exp); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	if exp.Version != galleryExportVersion {
		return fmt.Errorf("unsupported export version %d (expected %d)", exp.Version, galleryExportVersion)
	}
	applyMergeRecords(cx, readExportMergeRecords(exp))
	return nil
}

// mergeFromLightJSON applies a bare tags.json (no gallery files) onto cx.
// Records carry an empty zipEntry so applyMergeRecords falls through its
// no-file branch: tags are attached to whichever target images already match
// by sha, and entries with no match are skipped.
func mergeFromLightJSON(cx *galleryCtx, tmpPath string) error {
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open tags.json: %w", err)
	}
	var mf lightManifest
	err = json.NewDecoder(f).Decode(&mf)
	f.Close()
	if err != nil {
		return fmt.Errorf("decode tags.json: %w", err)
	}
	if mf.Version != galleryExportVersion {
		return fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, galleryExportVersion)
	}
	records := make([]mergeRecord, 0, len(mf.Images))
	for _, img := range mf.Images {
		records = append(records, mergeRecord{SHA256: img.SHA256, Tags: img.Tags})
	}
	applyMergeRecords(cx, records)
	return nil
}

func mergeFromZip(cx *galleryCtx, tmpPath string) error {
	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	var innerDB, innerJSON, innerLight *zip.File
	galleryFiles := map[string]*zip.File{}
	for _, f := range zr.File {
		switch {
		case f.Name == "monbooru.db":
			innerDB = f
		case f.Name == "monbooru.json":
			innerJSON = f
		case f.Name == "tags.json":
			innerLight = f
		case strings.HasPrefix(f.Name, "gallery/") && !strings.HasSuffix(f.Name, "/"):
			galleryFiles[strings.TrimPrefix(f.Name, "gallery/")] = f
		}
	}

	var records []mergeRecord
	dataDir := filepath.Dir(cx.DBPath)
	switch {
	case innerLight != nil && innerDB == nil && innerJSON == nil:
		rc, err := innerLight.Open()
		if err != nil {
			return fmt.Errorf("open tags.json: %w", err)
		}
		var mf lightManifest
		err = json.NewDecoder(rc).Decode(&mf)
		rc.Close()
		if err != nil {
			return fmt.Errorf("decode tags.json: %w", err)
		}
		if mf.Version != galleryExportVersion {
			return fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, galleryExportVersion)
		}
		for _, img := range mf.Images {
			records = append(records, mergeRecord{SHA256: img.SHA256, Tags: img.Tags, SourcePath: img.Path})
		}
	case innerDB != nil:
		inner, err := extractZipEntryToTemp(innerDB, dataDir)
		if err != nil {
			return err
		}
		defer os.Remove(inner)
		if err := validateSQLiteFile(inner); err != nil {
			return fmt.Errorf("inner db invalid: %w", err)
		}
		src, err := db.Open(inner)
		if err != nil {
			return err
		}
		defer src.Close()
		records, err = readDBMergeRecords(src)
		if err != nil {
			return err
		}
	case innerJSON != nil:
		rc, err := innerJSON.Open()
		if err != nil {
			return err
		}
		var exp galleryExport
		err = json.NewDecoder(rc).Decode(&exp)
		rc.Close()
		if err != nil {
			return fmt.Errorf("decode inner json: %w", err)
		}
		if exp.Version != galleryExportVersion {
			return fmt.Errorf("unsupported export version %d (expected %d)", exp.Version, galleryExportVersion)
		}
		records = readExportMergeRecords(exp)
	default:
		return fmt.Errorf("archive missing monbooru.db, monbooru.json, or tags.json")
	}

	for i := range records {
		if rel := records[i].SourcePath; rel != "" {
			if entry, ok := galleryFiles[rel]; ok {
				records[i].zipEntry = entry
			}
		}
	}
	applyMergeRecords(cx, records)
	return nil
}

// applyMergeRecords processes every record against the live gallery. When the
// record carries a zip entry and its SHA is unknown to the target, the entry
// is extracted into the gallery and ingested; otherwise only the tags are
// applied to the pre-existing image.
func applyMergeRecords(cx *galleryCtx, records []mergeRecord) {
	generalID := lookupCategoryID(cx.DB, "general")
	for _, r := range records {
		var imgID int64
		err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, r.SHA256).Scan(&imgID)
		if err == nil {
			applyImportTagsToImage(cx.DB, cx.TagSvc, imgID, r.Tags, generalID)
			continue
		}
		if err != sql.ErrNoRows {
			logx.Warnf("merge: lookup sha %s: %v", r.SHA256, err)
			continue
		}
		if r.zipEntry == nil || r.SourcePath == "" {
			// No file available for this sha; tags-only merge skips missing targets.
			continue
		}
		safeBase, err := safeArchiveDest(cx.GalleryPath, r.SourcePath)
		if err != nil {
			logx.Warnf("merge: skipping entry %q: %v", r.SourcePath, err)
			continue
		}
		// UniqueDestPath operates on (dir, basename); apply it relative to
		// the resolved parent so collisions are auto-suffixed within the
		// destination subdirectory rather than the gallery root.
		dst := gallery.UniqueDestPath(filepath.Dir(safeBase), filepath.Base(safeBase))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			logx.Warnf("merge: mkdir for %q: %v", r.SourcePath, err)
			continue
		}
		if err := copyZipFile(r.zipEntry, dst); err != nil {
			logx.Warnf("merge: extract %q: %v", r.SourcePath, err)
			continue
		}
		ft, err := gallery.DetectFileType(dst)
		if err != nil {
			logx.Warnf("merge: unsupported file %q: %v", r.SourcePath, err)
			os.Remove(dst)
			continue
		}
		img, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, dst, ft, models.OriginIngest)
		if err != nil {
			logx.Warnf("merge: ingest %q: %v", r.SourcePath, err)
			os.Remove(dst)
			continue
		}
		applyImportTagsToImage(cx.DB, cx.TagSvc, img.ID, r.Tags, generalID)
	}
}

// readDBMergeRecords extracts one record per non-missing image from a secondary
// SQLite file. Tags are emitted under their canonical name; aliases are skipped.
func readDBMergeRecords(src *db.DB) ([]mergeRecord, error) {
	rows, err := src.Read.Query(`
		SELECT i.id, i.sha256, i.folder_path, i.canonical_path
		FROM images i WHERE i.is_missing = 0`)
	if err != nil {
		return nil, err
	}
	type imgRow struct {
		id                     int64
		sha, folder, canonical string
	}
	var imgs []imgRow
	for rows.Next() {
		var r imgRow
		if err := rows.Scan(&r.id, &r.sha, &r.folder, &r.canonical); err != nil {
			rows.Close()
			return nil, err
		}
		imgs = append(imgs, r)
	}
	rows.Close()

	var recs []mergeRecord
	for _, r := range imgs {
		tagRows, err := src.Read.Query(`
			SELECT t.name, tc.name FROM image_tags it
			JOIN tags t ON t.id = it.tag_id
			JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id = ? AND t.is_alias = 0`, r.id)
		if err != nil {
			return nil, err
		}
		var tagsList []string
		for tagRows.Next() {
			var n, c string
			if err := tagRows.Scan(&n, &c); err != nil {
				tagRows.Close()
				return nil, err
			}
			if c == "general" {
				tagsList = append(tagsList, n)
			} else {
				tagsList = append(tagsList, c+":"+n)
			}
		}
		tagRows.Close()
		recs = append(recs, mergeRecord{
			SHA256:     r.sha,
			Tags:       tagsList,
			SourcePath: filepath.ToSlash(filepath.Join(r.folder, filepath.Base(r.canonical))),
		})
	}
	return recs, nil
}

// readExportMergeRecords builds the same per-image record list from a parsed
// galleryExport document (JSON import path).
func readExportMergeRecords(exp galleryExport) []mergeRecord {
	catByID := map[int64]string{}
	for _, c := range exp.TagCategories {
		catByID[c.ID] = c.Name
	}
	tagTokens := map[int64]string{}
	for _, t := range exp.Tags {
		if t.IsAlias == 1 {
			continue
		}
		cat := catByID[t.CategoryID]
		if cat == "general" || cat == "" {
			tagTokens[t.ID] = t.Name
		} else {
			tagTokens[t.ID] = cat + ":" + t.Name
		}
	}
	byImg := map[int64][]string{}
	for _, it := range exp.ImageTags {
		if tok, ok := tagTokens[it.TagID]; ok {
			byImg[it.ImageID] = append(byImg[it.ImageID], tok)
		}
	}
	var recs []mergeRecord
	for _, img := range exp.Images {
		if img.IsMissing == 1 {
			continue
		}
		recs = append(recs, mergeRecord{
			SHA256:     img.SHA256,
			Tags:       byImg[img.ID],
			SourcePath: filepath.ToSlash(filepath.Join(img.FolderPath, filepath.Base(img.CanonicalPath))),
		})
	}
	return recs
}

// applyImportTagsToImage resolves each "name" or "category:name" token and
// attaches it to imageID through the tag service so alias resolution and
// usage-count maintenance match the rest of the app.
func applyImportTagsToImage(database *db.DB, tagSvc *tags.Service, imageID int64, tokens []string, generalID int64) {
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		catID, bareName := resolveImportTag(database, token, generalID)
		t, err := tagSvc.GetOrCreateTag(bareName, catID)
		if err != nil {
			logx.Warnf("import tag %q: %v", token, err)
			continue
		}
		if err := tagSvc.AddTagToImageFromTagger(imageID, t.ID, false, nil, "import"); err != nil {
			logx.Warnf("import tag %q to image %d: %v", token, imageID, err)
		}
	}
}

func resolveImportTag(database *db.DB, token string, generalID int64) (int64, string) {
	if idx := strings.Index(token, ":"); idx > 0 {
		var catID int64
		if err := database.Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, token[:idx],
		).Scan(&catID); err == nil {
			return catID, token[idx+1:]
		}
	}
	return generalID, token
}

func lookupCategoryID(database *db.DB, name string) int64 {
	var id int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id)
	return id
}

func copyZipFile(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

func extractZipEntryToTemp(f *zip.File, dataDir string) (string, error) {
	tmp, err := os.CreateTemp(dataDir, "merge-inner-*")
	if err != nil {
		return "", err
	}
	rc, err := f.Open()
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	_, copyErr := io.Copy(tmp, rc)
	rc.Close()
	tmp.Close()
	if copyErr != nil {
		os.Remove(tmp.Name())
		return "", copyErr
	}
	return tmp.Name(), nil
}
