package web

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tags"
)

// safeArchiveDest joins a relative archive path under root and returns the
// resolved absolute destination, rejecting paths that escape root through
// `..` segments or absolute roots. Replaces the older
// `strings.Contains(rel, "..")` substring check, which both rejected legal
// names like "foo..bar.ext" and didn't actually exercise the same path
// containment helper the upload/move flows use.
func safeArchiveDest(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute archive entry path")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	dst, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if !gallery.PathInside(rootAbs, dst) {
		return "", fmt.Errorf("archive entry escapes gallery root")
	}
	return dst, nil
}

const galleryExportVersion = 1

// galleryExport is the root JSON document. Field order mirrors schema.sql so a
// human opening the file reads the schema top-down.
type galleryExport struct {
	Version         int                `json:"version"`
	GalleryName     string             `json:"gallery_name"`
	GalleryPath     string             `json:"gallery_path"`
	TagCategories   []tagCategoryRow   `json:"tag_categories"`
	Tags            []tagRow           `json:"tags"`
	Images          []imageRow         `json:"images"`
	ImagePaths      []imagePathRow     `json:"image_paths"`
	ImageTags       []imageTagRow      `json:"image_tags"`
	SDMetadata      []sdMetadataRow    `json:"sd_metadata"`
	ComfyUIMetadata []comfyMetadataRow `json:"comfyui_metadata"`
	SavedSearches   []savedSearchRow   `json:"saved_searches"`
}

type tagCategoryRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	IsBuiltin int    `json:"is_builtin"`
}

type tagRow struct {
	ID             int64         `json:"id"`
	Name           string        `json:"name"`
	CategoryID     int64         `json:"category_id"`
	UsageCount     int           `json:"usage_count"`
	IsAlias        int           `json:"is_alias"`
	CanonicalTagID sql.NullInt64 `json:"canonical_tag_id"`
	CreatedAt      string        `json:"created_at"`
}

type imageRow struct {
	ID            int64          `json:"id"`
	SHA256        string         `json:"sha256"`
	CanonicalPath string         `json:"canonical_path"`
	FolderPath    string         `json:"folder_path"`
	FileType      string         `json:"file_type"`
	Width         sql.NullInt64  `json:"width"`
	Height        sql.NullInt64  `json:"height"`
	FileSize      int64          `json:"file_size"`
	IsMissing     int            `json:"is_missing"`
	IsFavorited   int            `json:"is_favorited"`
	AutoTaggedAt  sql.NullString `json:"auto_tagged_at"`
	SourceType    string         `json:"source_type"`
	Origin        string         `json:"origin"`
	IngestedAt    string         `json:"ingested_at"`
}

type imagePathRow struct {
	ID          int64  `json:"id"`
	ImageID     int64  `json:"image_id"`
	Path        string `json:"path"`
	IsCanonical int    `json:"is_canonical"`
}

type imageTagRow struct {
	ImageID    int64           `json:"image_id"`
	TagID      int64           `json:"tag_id"`
	IsAuto     int             `json:"is_auto"`
	Confidence sql.NullFloat64 `json:"confidence"`
	TaggerName sql.NullString  `json:"tagger_name"`
	CreatedAt  string          `json:"created_at"`
}

type sdMetadataRow struct {
	ImageID        int64           `json:"image_id"`
	Prompt         sql.NullString  `json:"prompt"`
	NegativePrompt sql.NullString  `json:"negative_prompt"`
	Model          sql.NullString  `json:"model"`
	Seed           sql.NullInt64   `json:"seed"`
	Sampler        sql.NullString  `json:"sampler"`
	Steps          sql.NullInt64   `json:"steps"`
	CFGScale       sql.NullFloat64 `json:"cfg_scale"`
	RawParams      sql.NullString  `json:"raw_params"`
	GenerationHash sql.NullString  `json:"generation_hash"`
}

type comfyMetadataRow struct {
	ImageID         int64           `json:"image_id"`
	Prompt          sql.NullString  `json:"prompt"`
	ModelCheckpoint sql.NullString  `json:"model_checkpoint"`
	Seed            sql.NullInt64   `json:"seed"`
	Sampler         sql.NullString  `json:"sampler"`
	Steps           sql.NullInt64   `json:"steps"`
	CFGScale        sql.NullFloat64 `json:"cfg_scale"`
	RawWorkflow     sql.NullString  `json:"raw_workflow"`
	GenerationHash  sql.NullString  `json:"generation_hash"`
}

type savedSearchRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Query     string `json:"query"`
	CreatedAt string `json:"created_at"`
}

// ExportGalleryDB produces a clean, WAL-consolidated SQLite snapshot via
// VACUUM INTO and streams it to w. Safe to call while the source gallery is
// being read/written; VACUUM INTO sees a consistent point-in-time view.
func (s *Server) ExportGalleryDB(name string, w io.Writer) error {
	cx := s.Get(name)
	if cx == nil {
		return fmt.Errorf("unknown gallery %q", name)
	}
	tmp, err := os.CreateTemp(filepath.Dir(cx.DBPath), "export-*.db")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if _, err := cx.DB.Write.Exec("VACUUM INTO ?", tmpPath); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// ExportGalleryJSON streams every table of the gallery as a single JSON
// document. Streams array-by-array so memory stays proportional to the
// largest single table (image_tags on a big library).
func (s *Server) ExportGalleryJSON(name string, w io.Writer) error {
	cx := s.Get(name)
	if cx == nil {
		return fmt.Errorf("unknown gallery %q", name)
	}

	bw := newJSONWriter(w)
	bw.objStart()
	bw.field("version", galleryExportVersion)
	bw.field("gallery_name", cx.Name)
	bw.field("gallery_path", cx.GalleryPath)

	if err := streamRows(bw, "tag_categories", cx.DB,
		`SELECT id, name, color, is_builtin FROM tag_categories ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r tagCategoryRow
			err := rows.Scan(&r.ID, &r.Name, &r.Color, &r.IsBuiltin)
			return r, err
		}); err != nil {
		return err
	}
	if err := streamRows(bw, "tags", cx.DB,
		`SELECT id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at FROM tags ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r tagRow
			err := rows.Scan(&r.ID, &r.Name, &r.CategoryID, &r.UsageCount, &r.IsAlias, &r.CanonicalTagID, &r.CreatedAt)
			return r, err
		}); err != nil {
		return err
	}
	if err := streamRows(bw, "images", cx.DB,
		`SELECT id, sha256, canonical_path, folder_path, file_type, width, height,
		        file_size, is_missing, is_favorited, auto_tagged_at, source_type, origin, ingested_at
		 FROM images ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r imageRow
			err := rows.Scan(&r.ID, &r.SHA256, &r.CanonicalPath, &r.FolderPath, &r.FileType,
				&r.Width, &r.Height, &r.FileSize, &r.IsMissing, &r.IsFavorited,
				&r.AutoTaggedAt, &r.SourceType, &r.Origin, &r.IngestedAt)
			return r, err
		}); err != nil {
		return err
	}
	if err := streamRows(bw, "image_paths", cx.DB,
		`SELECT id, image_id, path, is_canonical FROM image_paths ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r imagePathRow
			err := rows.Scan(&r.ID, &r.ImageID, &r.Path, &r.IsCanonical)
			return r, err
		}); err != nil {
		return err
	}
	if err := streamRows(bw, "image_tags", cx.DB,
		`SELECT image_id, tag_id, is_auto, confidence, tagger_name, created_at FROM image_tags`,
		func(rows *sql.Rows) (any, error) {
			var r imageTagRow
			err := rows.Scan(&r.ImageID, &r.TagID, &r.IsAuto, &r.Confidence, &r.TaggerName, &r.CreatedAt)
			return r, err
		}); err != nil {
		return err
	}
	if err := streamRows(bw, "sd_metadata", cx.DB,
		`SELECT image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash FROM sd_metadata`,
		func(rows *sql.Rows) (any, error) {
			var r sdMetadataRow
			err := rows.Scan(&r.ImageID, &r.Prompt, &r.NegativePrompt, &r.Model, &r.Seed,
				&r.Sampler, &r.Steps, &r.CFGScale, &r.RawParams, &r.GenerationHash)
			return r, err
		}); err != nil {
		return err
	}
	if err := streamRows(bw, "comfyui_metadata", cx.DB,
		`SELECT image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash FROM comfyui_metadata`,
		func(rows *sql.Rows) (any, error) {
			var r comfyMetadataRow
			err := rows.Scan(&r.ImageID, &r.Prompt, &r.ModelCheckpoint, &r.Seed,
				&r.Sampler, &r.Steps, &r.CFGScale, &r.RawWorkflow, &r.GenerationHash)
			return r, err
		}); err != nil {
		return err
	}
	if err := streamRows(bw, "saved_searches", cx.DB,
		`SELECT id, name, query, created_at FROM saved_searches ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r savedSearchRow
			err := rows.Scan(&r.ID, &r.Name, &r.Query, &r.CreatedAt)
			return r, err
		}); err != nil {
		return err
	}
	bw.objEnd()
	return bw.err
}

// ExportGalleryArchive packs the chosen export format together with every
// source file under the gallery root into a ZIP archive. The inner DB/JSON
// file is at the root; images live under `gallery/<relative_path>` so an
// import restores them into the same subfolder layout.
func (s *Server) ExportGalleryArchive(name, format string, w io.Writer) error {
	cx := s.Get(name)
	if cx == nil {
		return fmt.Errorf("unknown gallery %q", name)
	}
	zw := zip.NewWriter(w)
	defer zw.Close()

	// Inner DB/JSON gets deflated (usually compresses well); image files are
	// already compressed so they go in as Store.
	header := &zip.FileHeader{Method: zip.Deflate}
	switch format {
	case "db":
		header.Name = "monbooru.db"
	case "json":
		header.Name = "monbooru.json"
	default:
		return fmt.Errorf("unknown archive format %q", format)
	}
	inner, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	switch format {
	case "db":
		if err := s.ExportGalleryDB(name, inner); err != nil {
			return err
		}
	case "json":
		if err := s.ExportGalleryJSON(name, inner); err != nil {
			return err
		}
	}

	return writeGalleryFilesToZip(zw, cx.GalleryPath)
}

// writeGalleryFilesToZip walks galleryPath and appends every file under it
// as `gallery/<relative_path>` entries in zw, using zip.Store for the
// already-compressed image payloads. A missing root surfaces as an empty
// section (degraded mode) so the inner db/json still rides along for a
// headers-only restore. Shared by ExportGalleryArchive and
// ExportGalleryLight, which used to carry near-identical walkers.
func writeGalleryFilesToZip(zw *zip.Writer, galleryPath string) error {
	if _, err := os.Stat(galleryPath); err != nil {
		logx.Warnf("export: gallery path %q unreadable; archive will not include gallery files: %v", galleryPath, err)
		return nil
	}
	return filepath.Walk(galleryPath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(galleryPath, path)
		if err != nil {
			return err
		}
		fh := &zip.FileHeader{
			Name:   "gallery/" + filepath.ToSlash(rel),
			Method: zip.Store,
		}
		fh.Modified = info.ModTime()
		entry, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(entry, f)
		return err
	})
}

// ImportGallery replaces the target gallery's database (and optionally its
// source files) with the contents of the uploaded archive/file. Destructive;
// the caller's UI is responsible for confirming intent.
//
// format is one of "db", "json", "zip". For "zip" the inner format is detected
// from the archive. importOver is rejected when the target is the active or
// default gallery (mirrors RemoveGallery's guard).
func (s *Server) ImportGallery(name, format string, upload io.Reader) error {
	if s.jobs.IsRunning() {
		return errJobRunning
	}

	s.ctxMu.Lock()
	cx, ok := s.contexts[name]
	if !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", name)
	}
	if name == s.activeName {
		s.ctxMu.Unlock()
		return fmt.Errorf("cannot import over the active gallery; switch to another first")
	}
	if name == s.cfg.DefaultGallery {
		s.ctxMu.Unlock()
		return fmt.Errorf("cannot import over the default gallery; set another as default first")
	}

	galleryPath := cx.GalleryPath
	dbPath := cx.DBPath
	thumbsPath := cx.ThumbnailsPath
	dataDir := filepath.Dir(dbPath)

	// Buffer the upload to a temp file on the same filesystem as the data
	// directory so the later rename is atomic. The upload may be a multi-GB
	// zip; we cannot keep it in RAM.
	tmp, err := os.CreateTemp(dataDir, "import-*.upload")
	if err != nil {
		s.ctxMu.Unlock()
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, upload); err != nil {
		tmp.Close()
		s.ctxMu.Unlock()
		return fmt.Errorf("buffer upload: %w", err)
	}
	tmp.Close()

	// Close the target DB and stop its watcher before touching on-disk state.
	cx.close()

	applyErr := applyImport(format, tmpPath, dbPath, thumbsPath, galleryPath)

	// Reopen regardless so we leave the gallery usable even after a failed import.
	newCx, openErr := openGalleryCtx(config.Gallery{
		Name: name, GalleryPath: galleryPath, DBPath: dbPath, ThumbnailsPath: thumbsPath,
	})
	if openErr != nil {
		s.ctxMu.Unlock()
		if applyErr != nil {
			return fmt.Errorf("import failed: %w (reopen also failed: %v)", applyErr, openErr)
		}
		return fmt.Errorf("reopen gallery: %w", openErr)
	}
	s.contexts[name] = newCx
	newCx.startWatcher(s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB, s.jobs)
	s.ctxMu.Unlock()

	go newCx.warmCaches()

	if applyErr != nil {
		return applyErr
	}
	logx.Infof("gallery: imported %q (format=%s)", name, format)

	// Make the imported gallery active before queuing the rebuild-thumbs job.
	// Otherwise the job-manager lock the rebuild takes would keep SwitchGallery
	// blocked for the duration of the rebuild, leaving the user pinned to
	// whatever gallery they had active at Import time. Failures here are
	// non-fatal: the import already succeeded and a failed switch just leaves
	// the previous gallery active.
	if err := s.SwitchGallery(name); err != nil {
		logx.Infof("gallery %q: post-import switch skipped: %v", name, err)
	}

	// Import wiped the thumbnails directory as part of the swap, so the newly
	// installed DB now references images that have no thumbnail on disk.
	// Queue a rebuild so the user doesn't have to reach for Maintenance →
	// Rebuild thumbnails manually. Non-fatal: import already succeeded; a
	// concurrent job or empty gallery just skips the kickoff.
	if err := s.startRebuildThumbsJob(newCx); err != nil {
		logx.Infof("gallery %q: skipped post-import rebuild: %v", name, err)
	}
	return nil
}

// applyImport runs the destructive file-system work outside ctxMu so the
// caller can defer lock release around it. Kept as a package function so the
// early-return error paths are linear.
func applyImport(format, tmpPath, dbPath, thumbsPath, galleryPath string) error {
	switch format {
	case "db":
		return replaceDBFromFile(tmpPath, dbPath, thumbsPath, galleryPath)
	case "json":
		// A .json upload may be either a full monbooru export or a bare
		// light tags.json manifest; sniff the document so each routes to
		// its own replacer rather than misdecoding into the wrong shape.
		isLight, err := isLightManifestJSON(tmpPath)
		if err != nil {
			return fmt.Errorf("inspect json: %w", err)
		}
		if isLight {
			return replaceFromLightManifest(tmpPath, dbPath, thumbsPath, galleryPath)
		}
		return replaceDBFromJSON(tmpPath, dbPath, thumbsPath, galleryPath)
	case "zip":
		return replaceFromArchive(tmpPath, dbPath, thumbsPath, galleryPath)
	}
	return fmt.Errorf("unknown import format %q", format)
}

// isLightManifestJSON peeks the JSON file at path and reports whether it
// looks like a light tags.json (only {version, images:[...]}) rather than a
// full monbooru export. The full export carries a non-empty gallery_name and
// a tag_categories array; the light manifest carries neither.
func isLightManifestJSON(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var probe struct {
		GalleryName   string            `json:"gallery_name"`
		TagCategories []json.RawMessage `json:"tag_categories"`
	}
	if err := json.NewDecoder(f).Decode(&probe); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	return probe.GalleryName == "" && len(probe.TagCategories) == 0, nil
}

// replaceDBFromFile atomically swaps the target DB file with the uploaded
// snapshot, wipes the thumbnails directory so stale thumbnail ids don't
// reference images that no longer exist, and rebases every image path onto
// the target gallery_path so an import into a gallery whose filesystem root
// differs from the source doesn't leave every image pointing at the old
// location.
func replaceDBFromFile(srcPath, dbPath, thumbsPath, galleryPath string) error {
	// Validate the snapshot opens cleanly before clobbering the live DB.
	if err := validateSQLiteFile(srcPath); err != nil {
		return fmt.Errorf("uploaded file is not a valid monbooru database: %w", err)
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if err := os.Rename(srcPath, dbPath); err != nil {
		return fmt.Errorf("install db: %w", err)
	}
	if err := os.RemoveAll(thumbsPath); err != nil {
		return fmt.Errorf("clear thumbnails: %w", err)
	}
	if err := os.MkdirAll(thumbsPath, 0o755); err != nil {
		return fmt.Errorf("recreate thumbnails dir: %w", err)
	}
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("reopen installed db: %w", err)
	}
	defer database.Close()
	if err := sanitizeImportedCategoryColors(database); err != nil {
		return fmt.Errorf("sanitize colors: %w", err)
	}
	return rebaseImagePaths(database, galleryPath)
}

// sanitizeImportedCategoryColors walks tag_categories on a freshly imported
// DB and replaces any color value that doesn't match the documented hex
// shape with the neutral fallback. Mirrors the per-row coercion that the
// JSON import path applies on insert; needed here because a `.db` import
// drops the entire SQLite file in place without going through monbooru's
// validators.
func sanitizeImportedCategoryColors(database *db.DB) error {
	rows, err := database.Read.Query(`SELECT id, color FROM tag_categories`)
	if err != nil {
		return err
	}
	type row struct {
		id    int64
		color string
	}
	var bad []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.color); err != nil {
			rows.Close()
			return err
		}
		if !tags.IsValidCategoryColor(r.color) {
			bad = append(bad, r)
		}
	}
	rows.Close()
	for _, r := range bad {
		logx.Warnf("import: replaced invalid color %q on tag_category id=%d with #888888", r.color, r.id)
		if _, err := database.Write.Exec(
			`UPDATE tag_categories SET color = ? WHERE id = ?`, "#888888", r.id,
		); err != nil {
			return err
		}
	}
	return nil
}

// replaceDBFromJSON decodes the uploaded JSON document, removes the current
// DB, creates a fresh one, and loads every table's rows in a single write
// transaction. Keeps primary keys from the export so image_tags still line up.
// Also rebases every canonical_path / image_paths.path onto the target
// gallery_path so a cross-root import doesn't dangle every link.
func replaceDBFromJSON(srcPath, dbPath, thumbsPath, galleryPath string) error {
	f, err := os.Open(srcPath)
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

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
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
	if err := loadExportIntoDB(database, exp); err != nil {
		return err
	}
	if err := rebaseImagePaths(database, galleryPath); err != nil {
		return err
	}

	if err := os.RemoveAll(thumbsPath); err != nil {
		return fmt.Errorf("clear thumbnails: %w", err)
	}
	if err := os.MkdirAll(thumbsPath, 0o755); err != nil {
		return fmt.Errorf("recreate thumbnails dir: %w", err)
	}
	return nil
}

// replaceFromArchive opens the uploaded ZIP, extracts the inner DB or JSON
// via the matching replaceDBFrom* helper, and when `gallery/` entries are
// present wipes the source folder and extracts them into it.
func replaceFromArchive(srcPath, dbPath, thumbsPath, galleryPath string) error {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	var innerDB, innerJSON, innerLight *zip.File
	var galleryFiles []*zip.File
	for _, f := range zr.File {
		switch {
		case f.Name == "monbooru.db":
			innerDB = f
		case f.Name == "monbooru.json":
			innerJSON = f
		case f.Name == "tags.json":
			innerLight = f
		case strings.HasPrefix(f.Name, "gallery/") && !strings.HasSuffix(f.Name, "/"):
			galleryFiles = append(galleryFiles, f)
		}
	}
	if innerDB == nil && innerJSON == nil && innerLight == nil {
		// No monbooru-native shape inside; fall through to the foreign-format translators.
		// They synthesise a light manifest plus a {rel → zip.File} map and
		// route through the same wipe+ingest path the native light replacer
		// uses, so the import flow stays identical past this point.
		if format := detectCompatFormat(zr.File); format != "" {
			return replaceFromCompatArchive(zr.File, format, dbPath, thumbsPath, galleryPath)
		}
		return fmt.Errorf("archive missing monbooru.db, monbooru.json, or tags.json")
	}
	// A light archive ships only tags.json + gallery/; route to the light
	// replacer which bootstraps a fresh db and ingests each image. A full
	// archive takes priority when both a monbooru.{db,json} and a tags.json
	// are present - that combination is unusual but the full payload wins.
	if innerDB == nil && innerJSON == nil {
		return replaceFromLightArchive(innerLight, galleryFiles, dbPath, thumbsPath, galleryPath)
	}

	// Extract the inner DB/JSON to a temp file alongside the upload, then
	// delegate to the single-file path so both import formats share the
	// thumbnail-wipe and validation behaviour.
	dataDir := filepath.Dir(dbPath)
	innerTmp, err := os.CreateTemp(dataDir, "inner-*.import")
	if err != nil {
		return fmt.Errorf("create inner temp: %w", err)
	}
	innerTmpPath := innerTmp.Name()
	defer os.Remove(innerTmpPath)

	var innerFile *zip.File
	var applyInner func(string, string, string, string) error
	if innerDB != nil {
		innerFile = innerDB
		applyInner = replaceDBFromFile
	} else {
		innerFile = innerJSON
		applyInner = replaceDBFromJSON
	}
	rc, err := innerFile.Open()
	if err != nil {
		innerTmp.Close()
		return err
	}
	if _, err := io.Copy(innerTmp, rc); err != nil {
		rc.Close()
		innerTmp.Close()
		return err
	}
	rc.Close()
	innerTmp.Close()
	if err := applyInner(innerTmpPath, dbPath, thumbsPath, galleryPath); err != nil {
		return err
	}

	if len(galleryFiles) == 0 {
		return nil
	}

	// Wipe the gallery tree and extract the archive's files into it. The
	// watcher is already stopped (caller's cx.close()), so the CREATE events
	// produced here do not re-ingest the new files behind our back.
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
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(dst)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		out.Close()
	}
	return nil
}

// rebaseImagePaths rewrites every images.canonical_path and
// image_paths.path so that the absolute prefix matches the target gallery's
// root. The export format stores absolute paths by design (the gallery is
// authoritatively at /foo/bar and everything keys off it); without this
// rewrite an import from a differently-mounted source gallery leaves every
// image dangling at its old location.
//
// folder_path is relative to gallery_path by construction, so rebuilding
// <targetRoot>/<folder_path>/<basename(old canonical)> gives us the new
// absolute path without needing to know what the source root was.
func rebaseImagePaths(database *db.DB, targetGalleryPath string) error {
	root := strings.TrimRight(targetGalleryPath, "/")
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type imgRow struct {
		id         int64
		folder     string
		canonical  string
	}
	rows, err := tx.Query(`SELECT id, folder_path, canonical_path FROM images`)
	if err != nil {
		return fmt.Errorf("scan images for rebase: %w", err)
	}
	var imgs []imgRow
	for rows.Next() {
		var r imgRow
		if err := rows.Scan(&r.id, &r.folder, &r.canonical); err != nil {
			rows.Close()
			return err
		}
		imgs = append(imgs, r)
	}
	rows.Close()

	// image_paths has its own absolute column; rebuild it from the row's
	// image_id by looking up the matching image's folder_path + basename.
	// Image_paths include the canonical (is_canonical=1) and any aliases.
	// We rebuild each one by computing newPath the same way.
	for _, r := range imgs {
		newCanonical := filepath.Join(root, r.folder, filepath.Base(r.canonical))
		if newCanonical == r.canonical {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE images SET canonical_path = ? WHERE id = ?`, newCanonical, r.id,
		); err != nil {
			return fmt.Errorf("update image %d: %w", r.id, err)
		}
	}

	// Rebase image_paths per-row. image_paths uniqueness is on the path
	// column; rebased paths can collide with existing rows on the same
	// basename across folders only when the export carried alias rows
	// that happen to share a name - rare but possible, so we dedupe by
	// letting the INSERT conflict drop the collider.
	pathRows, err := tx.Query(
		`SELECT ip.id, ip.image_id, ip.path, i.folder_path
		 FROM image_paths ip
		 JOIN images i ON i.id = ip.image_id`,
	)
	if err != nil {
		return fmt.Errorf("scan image_paths for rebase: %w", err)
	}
	type pathRow struct {
		id, imageID int64
		path        string
		folder      string
	}
	var paths []pathRow
	for pathRows.Next() {
		var r pathRow
		if err := pathRows.Scan(&r.id, &r.imageID, &r.path, &r.folder); err != nil {
			pathRows.Close()
			return err
		}
		paths = append(paths, r)
	}
	pathRows.Close()

	for _, p := range paths {
		newPath := filepath.Join(root, p.folder, filepath.Base(p.path))
		if newPath == p.path {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET path = ? WHERE id = ?`, newPath, p.id,
		); err != nil {
			return fmt.Errorf("update image_path %d: %w", p.id, err)
		}
	}

	// After rebasing, flag every row whose canonical file isn't on disk in
	// the target gallery as is_missing=1. Without this the user sees rows
	// that look healthy in the gallery view but 404 on click; the
	// missing:true filter then surfaces the gap so they can re-attach the
	// files or prune the rows. Mirrors what Sync does for vanished files.
	for _, r := range imgs {
		newCanonical := filepath.Join(root, r.folder, filepath.Base(r.canonical))
		if _, err := os.Stat(newCanonical); err == nil {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE images SET is_missing = 1 WHERE id = ?`, r.id,
		); err != nil {
			return fmt.Errorf("flag missing image %d: %w", r.id, err)
		}
	}

	return tx.Commit()
}

// wipeDirContents removes everything inside dir but keeps the directory
// itself (so a bind mount survives).
func wipeDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// validateSQLiteFile opens the file as a SQLite DB, checks the expected
// tables exist, and closes it. Cheaper than running the schema bootstrap
// twice and surfaces "uploaded an arbitrary blob" before we remove the live DB.
func validateSQLiteFile(path string) error {
	database, err := db.Open(path)
	if err != nil {
		return err
	}
	defer database.Close()
	for _, tbl := range []string{"tag_categories", "tags", "images", "image_tags"} {
		var n int
		if err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("missing table %q", tbl)
		}
	}
	return nil
}

// loadExportIntoDB reinserts every table from the export document into a
// freshly-bootstrapped DB. The bootstrap seeds built-in tag_categories; we
// overwrite their rows so any customized colors round-trip.
func loadExportIntoDB(database *db.DB, exp galleryExport) error {
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Defer FK checks until COMMIT so alias rows can reference canonical
	// tags that haven't been inserted yet (tags are emitted in ID order,
	// and an alias created before its canonical legitimately has a lower
	// id). The pragma is scoped to the current transaction.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer fk: %w", err)
	}

	// Bootstrap's INSERT OR IGNORE seeded the built-in categories; replace
	// them so the caller's color overrides survive.
	if _, err := tx.Exec(`DELETE FROM tag_categories`); err != nil {
		return fmt.Errorf("reset tag_categories: %w", err)
	}
	for _, r := range exp.TagCategories {
		// Imported colours haven't been through CreateCategory's regex; coerce
		// anything that doesn't match the documented #rgb / #rrggbb shape to
		// the neutral fallback so a malicious export can't drop arbitrary
		// strings into the inline `style="color:..."` template context.
		safeColor := tags.SafeCategoryColor(r.Color)
		if safeColor != r.Color {
			logx.Warnf("import: replaced invalid color %q for tag_category %q with %s", r.Color, r.Name, safeColor)
		}
		if _, err := tx.Exec(
			`INSERT INTO tag_categories (id, name, color, is_builtin) VALUES (?, ?, ?, ?)`,
			r.ID, r.Name, safeColor, r.IsBuiltin,
		); err != nil {
			return fmt.Errorf("insert tag_category %q: %w", r.Name, err)
		}
	}
	for _, r := range exp.Tags {
		var canonical any
		if r.CanonicalTagID.Valid {
			canonical = r.CanonicalTagID.Int64
		}
		if _, err := tx.Exec(
			`INSERT INTO tags (id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Name, r.CategoryID, r.UsageCount, r.IsAlias, canonical, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert tag %d: %w", r.ID, err)
		}
	}
	for _, r := range exp.Images {
		var width, height, auto any
		if r.Width.Valid {
			width = r.Width.Int64
		}
		if r.Height.Valid {
			height = r.Height.Int64
		}
		if r.AutoTaggedAt.Valid {
			auto = r.AutoTaggedAt.String
		}
		if _, err := tx.Exec(
			`INSERT INTO images (id, sha256, canonical_path, folder_path, file_type, width, height,
			                    file_size, is_missing, is_favorited, auto_tagged_at, source_type, origin, ingested_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.SHA256, r.CanonicalPath, r.FolderPath, r.FileType, width, height,
			r.FileSize, r.IsMissing, r.IsFavorited, auto, r.SourceType, r.Origin, r.IngestedAt,
		); err != nil {
			return fmt.Errorf("insert image %d: %w", r.ID, err)
		}
	}
	for _, r := range exp.ImagePaths {
		if _, err := tx.Exec(
			`INSERT INTO image_paths (id, image_id, path, is_canonical) VALUES (?, ?, ?, ?)`,
			r.ID, r.ImageID, r.Path, r.IsCanonical,
		); err != nil {
			return fmt.Errorf("insert image_path %d: %w", r.ID, err)
		}
	}
	for _, r := range exp.ImageTags {
		var conf, tname any
		if r.Confidence.Valid {
			conf = r.Confidence.Float64
		}
		if r.TaggerName.Valid {
			tname = r.TaggerName.String
		}
		if _, err := tx.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto, confidence, tagger_name, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.ImageID, r.TagID, r.IsAuto, conf, tname, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert image_tag (%d,%d): %w", r.ImageID, r.TagID, err)
		}
	}
	for _, r := range exp.SDMetadata {
		if _, err := tx.Exec(
			`INSERT INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, nullStringArg(r.Prompt), nullStringArg(r.NegativePrompt), nullStringArg(r.Model),
			nullInt64Arg(r.Seed), nullStringArg(r.Sampler), nullInt64Arg(r.Steps),
			nullFloat64Arg(r.CFGScale), nullStringArg(r.RawParams), nullStringArg(r.GenerationHash),
		); err != nil {
			return fmt.Errorf("insert sd_metadata %d: %w", r.ImageID, err)
		}
	}
	for _, r := range exp.ComfyUIMetadata {
		if _, err := tx.Exec(
			`INSERT INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, nullStringArg(r.Prompt), nullStringArg(r.ModelCheckpoint),
			nullInt64Arg(r.Seed), nullStringArg(r.Sampler), nullInt64Arg(r.Steps),
			nullFloat64Arg(r.CFGScale), nullStringArg(r.RawWorkflow), nullStringArg(r.GenerationHash),
		); err != nil {
			return fmt.Errorf("insert comfyui_metadata %d: %w", r.ImageID, err)
		}
	}
	for _, r := range exp.SavedSearches {
		if _, err := tx.Exec(
			`INSERT INTO saved_searches (id, name, query, created_at) VALUES (?, ?, ?, ?)`,
			r.ID, r.Name, r.Query, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert saved_search %d: %w", r.ID, err)
		}
	}
	return tx.Commit()
}

func nullStringArg(n sql.NullString) any {
	if n.Valid {
		return n.String
	}
	return nil
}

func nullInt64Arg(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func nullFloat64Arg(n sql.NullFloat64) any {
	if n.Valid {
		return n.Float64
	}
	return nil
}

// --- JSON stream writer ---

// jsonWriter emits a single JSON object incrementally so each table's rows
// stream out as we query them, bounding memory to one row at a time. Caller
// drives it with objStart / field / arrayStart+arrayItem+arrayEnd / objEnd;
// the first-field bookkeeping keeps commas correct without the caller
// juggling them.
type jsonWriter struct {
	w     io.Writer
	err   error
	first bool
}

func newJSONWriter(w io.Writer) *jsonWriter { return &jsonWriter{w: w} }

func (j *jsonWriter) writeStr(s string) {
	if j.err != nil {
		return
	}
	_, j.err = j.w.Write([]byte(s))
}

func (j *jsonWriter) raw(b []byte) {
	if j.err != nil {
		return
	}
	_, j.err = j.w.Write(b)
}

func (j *jsonWriter) objStart() {
	j.writeStr("{")
	j.first = true
}

func (j *jsonWriter) objEnd() { j.writeStr("}\n") }

func (j *jsonWriter) comma() {
	if j.first {
		j.first = false
	} else {
		j.writeStr(",")
	}
}

func (j *jsonWriter) field(name string, value any) {
	j.comma()
	j.marshalAndWrite(name)
	j.writeStr(":")
	j.marshalAndWrite(value)
}

func (j *jsonWriter) arrayStart(name string) {
	j.comma()
	j.marshalAndWrite(name)
	j.writeStr(":[")
}

func (j *jsonWriter) arrayEnd() { j.writeStr("]") }

func (j *jsonWriter) arrayItem(first *bool, value any) {
	if !*first {
		j.writeStr(",")
	}
	*first = false
	j.marshalAndWrite(value)
}

func (j *jsonWriter) marshalAndWrite(value any) {
	if j.err != nil {
		return
	}
	b, err := json.Marshal(value)
	if err != nil {
		j.err = err
		return
	}
	j.raw(b)
}

// streamRows runs query and emits each row as one element of a JSON array
// named `key`. scan builds the per-row value that will be JSON-marshaled.
func streamRows(j *jsonWriter, key string, database *db.DB, query string, scan func(*sql.Rows) (any, error)) error {
	j.arrayStart(key)
	first := true
	rows, err := database.Read.Query(query)
	if err != nil {
		j.arrayEnd()
		return err
	}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			rows.Close()
			j.arrayEnd()
			return err
		}
		j.arrayItem(&first, v)
		if j.err != nil {
			rows.Close()
			j.arrayEnd()
			return j.err
		}
	}
	err = rows.Err()
	rows.Close()
	j.arrayEnd()
	if err != nil {
		return err
	}
	return j.err
}

// --- HTTP handlers ---

// settingsGalleryExport serves GET /settings/galleries/{name}/export?format=&with_images=.
// Plain GET so the browser saves the response as a file without HTMX wiring.
func (s *Server) settingsGalleryExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	withImages := r.URL.Query().Get("with_images") == "true"

	if s.Get(name) == nil {
		http.Error(w, "unknown gallery", http.StatusNotFound)
		return
	}
	switch format {
	case "db", "json", "light":
	default:
		http.Error(w, "format must be db, json, or light", http.StatusBadRequest)
		return
	}

	filename, contentType := exportFilename(name, format, withImages)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	var err error
	switch {
	case format == "light" && withImages:
		err = s.ExportGalleryLight(name, w)
	case format == "light":
		err = s.ExportGalleryLightManifest(name, w)
	case withImages:
		err = s.ExportGalleryArchive(name, format, w)
	case format == "db":
		err = s.ExportGalleryDB(name, w)
	case format == "json":
		err = s.ExportGalleryJSON(name, w)
	}
	if err != nil {
		logx.Warnf("gallery export %q: %v", name, err)
		// Can't change headers after Write; the browser will see a truncated file.
	}
}

func exportFilename(name, format string, withImages bool) (string, string) {
	if format == "light" && withImages {
		return name + "-light.zip", "application/zip"
	}
	if format == "light" {
		return name + "-light.json", "application/json"
	}
	if withImages {
		return name + ".zip", "application/zip"
	}
	switch format {
	case "db":
		return name + ".db", "application/vnd.sqlite3"
	case "json":
		return name + ".json", "application/json"
	}
	return name, "application/octet-stream"
}

// settingsGalleryImport serves POST /settings/galleries/{name}/import.
// Expects a multipart form with `file` and `confirm_name` (must equal the
// target gallery name so accidental drops don't wipe the database).
func (s *Server) settingsGalleryImport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	const maxImport = 16 << 30 // 16 GiB cap; protects against runaway uploads on a LAN setup.
	r.Body = http.MaxBytesReader(w, r.Body, maxImport)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeFlash(w, "err", "upload too large or malformed")
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	if mode == "" {
		mode = "replace"
	}
	if mode != "replace" && mode != "merge" {
		writeFlash(w, "err", "mode must be replace or merge")
		return
	}
	// Replace wipes the target gallery, so keep the type-to-confirm gate.
	// Merge is additive; the confirm is waived.
	if mode == "replace" {
		confirm := strings.TrimSpace(r.FormValue("confirm_name"))
		if confirm != name {
			writeFlash(w, "err", "type-to-confirm name does not match")
			return
		}
	}
	file, fh, err := r.FormFile("file")
	if err != nil {
		writeFlash(w, "err", "missing file")
		return
	}
	defer file.Close()

	format := formatFromExt(fh.Filename)
	if format == "" {
		writeFlash(w, "err", "file must be .db, .json, or .zip")
		return
	}

	if mode == "merge" {
		if err := s.MergeGallery(name, format, file); err != nil {
			writeFlash(w, "err", err.Error())
			return
		}
		// Mirror the replace path (ImportGallery → SwitchGallery): a merge
		// brings new images into the target gallery, so the user expects to
		// land on it. No-op if the target is already active.
		if err := s.SwitchGallery(name); err != nil {
			logx.Infof("gallery %q: post-merge switch skipped: %v", name, err)
		}
		writeFlash(w, "ok", "Gallery "+name+" merged.")
		return
	}
	if err := s.ImportGallery(name, format, file); err != nil {
		writeFlash(w, "err", err.Error())
		return
	}
	// Write the success flash into #flash-galleries; the dialog's
	// after-request hook detects the flash-ok, closes the modal, and
	// triggers a reload so the newly-active gallery badge shows.
	writeFlash(w, "ok", "Gallery "+name+" imported. Rebuilding thumbnails in the background.")
}

func formatFromExt(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".db", ".sqlite":
		return "db"
	case ".json":
		return "json"
	case ".zip":
		return "zip"
	}
	return ""
}
