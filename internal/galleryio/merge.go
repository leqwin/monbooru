package galleryio

import (
	"archive/zip"
	"cmp"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// LightManifestImage is one record inside the tags.json manifest of a light
// export. Tags use "name" for the general category and "category:name" for
// everything else so category attribution round-trips without extra fields.
type LightManifestImage struct {
	SHA256 string   `json:"sha256"`
	Path   string   `json:"path"`
	Tags   []string `json:"tags"`
}

// LightManifest is the root JSON document at tags.json inside a light zip.
type LightManifest struct {
	Version int                  `json:"version"`
	Images  []LightManifestImage `json:"images"`
}

// decodeLightManifest reads a tags.json manifest and rejects an
// unsupported version. The caller owns opening/closing the reader (zip
// entry vs file on disk).
func decodeLightManifest(r io.Reader) (LightManifest, error) {
	var mf LightManifest
	if err := json.NewDecoder(r).Decode(&mf); err != nil {
		return mf, fmt.Errorf("decode tags.json: %w", err)
	}
	if mf.Version != LightManifestVersion {
		return mf, fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, LightManifestVersion)
	}
	return mf, nil
}

// tagToken renders a tag as the manifest stores it: a bare name for the
// general category (or an unattributed JOIN row), else "category:name"
// so non-general attribution round-trips.
func tagToken(category, name string) string {
	if category == "general" || category == "" {
		return name
	}
	return category + ":" + name
}

// ImportSourceNative is the tagger_name attached to image_tags rows
// produced by monbooru-native imports (full export and light archive).
// Compat translators pass their format name (`hydrus`...) so the
// detail page can credit the originating provider.
const ImportSourceNative = "import"

// ExportGalleryLight streams a zip containing gallery/<rel> image files plus
// a tags.json manifest listing {sha256, path, tags} for each non-missing
// image. The archive omits monbooru-specific data (SD/ComfyUI metadata,
// saved searches, tag attribution), keeping it useful as a portable bundle
// that other software can read or produce.
func ExportGalleryLight(cx gallery.Handle, w io.Writer) error {
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	inner, err := zw.CreateHeader(&zip.FileHeader{Name: "tags.json", Method: zip.Deflate})
	if err != nil {
		return err
	}
	if err := writeLightManifest(cx, inner); err != nil {
		return err
	}

	if err := writeGalleryFilesToZip(zw, cx.GalleryPath); err != nil {
		return err
	}
	// Close writes the central directory; until it succeeds the response
	// body is not a readable archive. The deferred close stays for the
	// error paths, where its already-closed error is discarded.
	return zw.Close()
}

// ExportGalleryLightManifest streams the same tags.json document as
// ExportGalleryLight but without the surrounding zip and without the gallery
// files. Used by the export handler when the user picks the light format
// without "Include image files".
func ExportGalleryLightManifest(cx gallery.Handle, w io.Writer) error {
	return writeLightManifest(cx, w)
}

// writeLightManifest streams the tags.json document using the existing
// jsonWriter. One image-ordered join carries the tags, so the cursor
// groups as it advances and neither the image list nor a per-image
// query round trip is paid.
func writeLightManifest(cx gallery.Handle, w io.Writer) error {
	bw := newJSONWriter(w)
	bw.objStart()
	bw.field("version", LightManifestVersion)
	bw.arrayStart("images")
	first := true
	err := walkLightRows(cx.DB.Read, func(sha, relPath string, tags []string) {
		// A tag-less image ships an empty array, not null.
		if tags == nil {
			tags = []string{}
		}
		bw.arrayItem(&first, LightManifestImage{SHA256: sha, Path: relPath, Tags: tags})
	})
	bw.arrayEnd()
	bw.objEnd()
	if err != nil {
		return err
	}
	return bw.err
}

// walkLightRows runs the light-manifest join and calls emit once per image
// with the tags collected across its rows. The query orders by image id, so
// grouping rides the cursor rather than a tag query per image.
func walkLightRows(read *sql.DB, emit func(sha, relPath string, tags []string)) error {
	rows, err := read.Query(lightManifestQuery)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var curID int64
	var curSHA, curPath string
	var tags []string
	open := false
	for rows.Next() {
		var id int64
		var sha, folder, canonical string
		var tname, tcat sql.NullString
		if err := rows.Scan(&id, &sha, &folder, &canonical, &tname, &tcat); err != nil {
			return err
		}
		if !open || id != curID {
			if open {
				emit(curSHA, curPath, tags)
			}
			curID, open = id, true
			curSHA = sha
			curPath = filepath.ToSlash(filepath.Join(folder, storedBasename(canonical)))
			tags = nil
		}
		if tname.Valid {
			tags = append(tags, tagToken(tcat.String, tname.String))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if open {
		emit(curSHA, curPath, tags)
	}
	return nil
}

// lightManifestQuery emits one row per (image, tag) pair and a single
// tag-less row for an image carrying none. Alias rows fall out through
// the join condition rather than a WHERE so they don't take their image
// with them.
const lightManifestQuery = `
	SELECT i.id, i.sha256, i.folder_path, i.canonical_path, t.name, tc.name
	FROM images i
	LEFT JOIN image_tags it ON it.image_id = i.id
	LEFT JOIN tags t ON t.id = it.tag_id AND t.is_alias = 0
	LEFT JOIN tag_categories tc ON tc.id = t.category_id
	WHERE i.is_missing = 0
	ORDER BY i.id, tc.name, t.name`

// replaceFromLightArchive wipes the target gallery (db, thumbnails, source
// folder) and rebuilds from a light zip: a fresh db gets bootstrapped,
// every gallery/<rel> file is extracted into the target gallery, then each
// image is ingested and tagged from the manifest. Shares its per-record
// ingest loop with the merge path.
func replaceFromLightArchive(manifest *zip.File, galleryFiles []*zip.File, dbPath, thumbsPath, galleryPath string, maxFileSizeMB int) error {
	mc, err := manifest.Open()
	if err != nil {
		return fmt.Errorf("open tags.json: %w", err)
	}
	mf, err := decodeLightManifest(mc)
	_ = mc.Close()
	if err != nil {
		return err
	}
	files := make([]translatedFile, 0, len(galleryFiles))
	for _, f := range galleryFiles {
		files = append(files, translatedFile{
			rel:  strings.TrimPrefix(f.Name, "gallery/"),
			file: f,
		})
	}
	return ApplyLightReplace(mf, files, dbPath, thumbsPath, galleryPath, ImportSourceNative, maxFileSizeMB)
}

// translatedFile pairs a zip entry with the relative gallery path it should
// extract to.
type translatedFile struct {
	rel  string
	file *zip.File
}

// ApplyLightReplace wipes the target's db / thumbnails / gallery dir, drops
// the listed files at their relative gallery paths, and bootstraps a fresh
// db that ingests every manifest entry. Shared by the native light-zip
// replacer and the compat (hydrus...) replacer.
func ApplyLightReplace(mf LightManifest, files []translatedFile, dbPath, thumbsPath, galleryPath, source string, maxFileSizeMB int) error {
	if mf.Version != LightManifestVersion {
		return fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, LightManifestVersion)
	}
	if err := resetDBAndThumbs(dbPath, thumbsPath); err != nil {
		return err
	}
	// Skip the gallery wipe when the archive ships no files. A tags.json-only
	// upload (or a foreign translator that emitted an empty Files map) is a
	// "rebuild the DB against files already on disk" workflow; wiping the
	// folder there destroys the very files the manifest references.
	if len(files) > 0 {
		if err := wipeDirContents(galleryPath); err != nil {
			return fmt.Errorf("wipe gallery: %w", err)
		}
	}
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	for _, tf := range files {
		dst, err := SafeArchiveDest(galleryPath, tf.rel)
		if err != nil {
			return fmt.Errorf("rejecting archive entry %q: %w", tf.rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyZipFile(tf.file, dst, maxBytes); err != nil {
			return err
		}
	}
	return rebuildFromLightManifest(dbPath, galleryPath, thumbsPath, mf.Images, source)
}

// resetDBAndThumbs removes the live DB sidecars and the thumbnails directory
// so a fresh DB can be bootstrapped onto cleared state. Shared by every
// destructive replace path.
func resetDBAndThumbs(dbPath, thumbsPath string) error {
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
	mf, err := decodeLightManifest(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	if err := resetDBAndThumbs(dbPath, thumbsPath); err != nil {
		return err
	}
	return rebuildFromLightManifest(dbPath, galleryPath, thumbsPath, mf.Images, ImportSourceNative)
}

// rebuildFromLightManifest bootstraps a fresh database at dbPath and ingests
// every manifest entry into it. The caller has already cleared the old state.
func rebuildFromLightManifest(dbPath, galleryPath, thumbsPath string, images []LightManifestImage, source string) error {
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open new db: %w", err)
	}
	defer func() { _ = database.Close() }()
	if err := db.Bootstrap(database); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	ingestLightManifestEntries(database, galleryPath, thumbsPath, images, source)
	return nil
}

// ingestLightManifestEntries walks each manifest entry, stats the matching
// file under galleryPath, and on success ingests it and applies its tags.
// Entries whose file isn't on disk are still recorded as is_missing=1 rows
// so the manifest's tags are preserved and the user can spot the gap with
// the missing:true filter, mirroring how Sync flags vanished files.
// Shared by the light-zip and light-json replace paths so both end up with
// identical row shapes in the freshly bootstrapped db.
func ingestLightManifestEntries(database *db.DB, galleryPath, thumbsPath string, entries []LightManifestImage, source string) {
	tagSvc := tags.New(database)
	generalID := LookupCategoryID(database, "general")
	for _, r := range entries {
		path, err := SafeArchiveDest(galleryPath, r.Path)
		if err != nil {
			logx.Warnf("light import: skipping entry %q: %v", r.Path, err)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			imgID, err := insertMissingImageRow(database, r.SHA256, path, galleryPath, source)
			if err != nil {
				logx.Warnf("light import: record missing %q: %v", r.Path, err)
				continue
			}
			applyImportTagsToImage(database, tagSvc, imgID, r.Tags, generalID, source)
			continue
		}
		if _, err := gallery.DetectFileType(path); err != nil {
			logx.Warnf("light import: unsupported file %q: %v", r.Path, err)
			continue
		}
		img, _, err := gallery.Ingest(database, galleryPath, thumbsPath, path, source)
		if err != nil {
			logx.Warnf("light import: ingest %q: %v", r.Path, err)
			continue
		}
		applyImportTagsToImage(database, tagSvc, img.ID, r.Tags, generalID, source)
	}
}

// insertMissingImageRow records a manifest entry whose file isn't on disk.
// File type comes from the extension (DetectFileType's magic-byte fallback
// can't run without a readable file); unknown extensions surface as an
// error so the caller logs+skips. Width/height stay NULL since we never
// decoded the image. origin defaults to ImportSourceNative when empty so
// a missing-file row from a foreign translator credits the provider.
func insertMissingImageRow(database *db.DB, sha, path, galleryPath, origin string) (int64, error) {
	ft, err := gallery.DetectFileType(path)
	if err != nil {
		return 0, err
	}
	if sha == "" {
		return 0, fmt.Errorf("manifest entry has empty sha256")
	}
	origin = cmp.Or(origin, models.OriginIngest)
	folder := gallery.FolderPath(galleryPath, path)
	tx, err := database.Write.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	err = tx.QueryRow(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size,
		                    is_missing, source_type, origin)
		 VALUES (?, ?, ?, ?, 0, 1, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING
		 RETURNING id`,
		sha, path, folder, ft, models.SourceTypeNone, origin,
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

// MergeResult counts what one merge did, so the caller can say it. A
// `.db` or `.json` upload carries no image bytes, so its Added is always
// zero and the whole file may land as Skipped - a result the operator has
// to be told rather than left to infer from an unchanged gallery.
type MergeResult struct {
	Added   int // images ingested from files the archive carried
	Tagged  int // images already in the gallery that the upload named
	Skipped int // records whose sha the gallery does not hold and no file backed
}

// MergeGallery additively brings images and tags from the uploaded file into
// the named gallery. Unlike ImportGallery it does not wipe anything and is
// permitted on the active and default galleries. db and json uploads apply
// tags to existing images matched by SHA; zip uploads (full or light) also
// ingest new images when the archive carries their files.
func MergeGallery(cx gallery.Handle, format string, upload io.Reader, maxFileSizeMB int) (MergeResult, error) {
	var res MergeResult
	dataDir := filepath.Dir(cx.DBPath)

	tmp, err := os.CreateTemp(dataDir, "merge-*.upload")
	if err != nil {
		return res, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, upload); err != nil {
		_ = tmp.Close()
		return res, fmt.Errorf("buffer upload: %w", err)
	}
	_ = tmp.Close()

	var mergeErr error
	switch format {
	case "db":
		res, mergeErr = mergeFromDB(cx, tmpPath, maxFileSizeMB)
	case "json":
		// Disambiguate full monbooru export vs bare light tags.json with
		// the same probe used on the replace path.
		isLight, err := isLightManifestJSON(tmpPath)
		if err != nil {
			mergeErr = fmt.Errorf("inspect json: %w", err)
			break
		}
		if isLight {
			res, mergeErr = mergeFromLightJSON(cx, tmpPath, maxFileSizeMB)
		} else {
			res, mergeErr = mergeFromJSON(cx, tmpPath, maxFileSizeMB)
		}
	case "zip":
		res, mergeErr = mergeFromZip(cx, tmpPath, maxFileSizeMB)
	default:
		mergeErr = fmt.Errorf("unknown merge format %q", format)
	}
	if mergeErr == nil {
		logx.Infof("gallery: merged into %q (format=%s): +%d image(s), %d tagged, %d skipped",
			cx.Name, format, res.Added, res.Tagged, res.Skipped)
	}
	return res, mergeErr
}

// Summary says what the merge did in the operator's terms. A zero Added
// on a format that never carries files is the whole point: the dialog's
// "add new images and tags" is only true for a .zip.
func (r MergeResult) Summary() string {
	parts := []string{fmt.Sprintf("%d image(s) added", r.Added), fmt.Sprintf("%d tagged", r.Tagged)}
	if r.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d not in this gallery", r.Skipped))
	}
	return strings.Join(parts, ", ")
}

func mergeFromDB(cx gallery.Handle, tmpPath string, maxFileSizeMB int) (MergeResult, error) {
	records, err := readDBRecordsFromFile(tmpPath, "uploaded file is not a valid monbooru database")
	if err != nil {
		return MergeResult{}, err
	}
	return applyMergeRecords(cx, records, ImportSourceNative, maxFileSizeMB), nil
}

// readDBRecordsFromFile validates a SQLite file and reads its merge records.
// invalidMsg names the file in the validation error: an uploaded database and
// one unpacked from an archive read differently to the operator.
func readDBRecordsFromFile(path, invalidMsg string) ([]mergeRecord, error) {
	if err := validateSQLiteFile(path); err != nil {
		return nil, fmt.Errorf("%s: %w", invalidMsg, err)
	}
	src, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	return readDBMergeRecords(src)
}

func mergeFromJSON(cx gallery.Handle, tmpPath string, maxFileSizeMB int) (MergeResult, error) {
	f, err := os.Open(tmpPath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open json: %w", err)
	}
	defer func() { _ = f.Close() }()
	exp, err := DecodeExport(f)
	if err != nil {
		return MergeResult{}, err
	}
	return applyMergeRecords(cx, readExportMergeRecords(exp), ImportSourceNative, maxFileSizeMB), nil
}

// mergeFromLightJSON applies a bare tags.json (no gallery files) onto cx.
// Records carry an empty zipEntry so applyMergeRecords falls through its
// no-file branch: tags are attached to whichever target images already match
// by sha, and entries with no match are skipped.
func mergeFromLightJSON(cx gallery.Handle, tmpPath string, maxFileSizeMB int) (MergeResult, error) {
	f, err := os.Open(tmpPath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open tags.json: %w", err)
	}
	mf, err := decodeLightManifest(f)
	_ = f.Close()
	if err != nil {
		return MergeResult{}, err
	}
	records := make([]mergeRecord, 0, len(mf.Images))
	for _, img := range mf.Images {
		records = append(records, mergeRecord{SHA256: img.SHA256, Tags: img.Tags})
	}
	return applyMergeRecords(cx, records, ImportSourceNative, maxFileSizeMB), nil
}

func mergeFromZip(cx gallery.Handle, tmpPath string, maxFileSizeMB int) (MergeResult, error) {
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	innerDB, innerJSON, innerLight, inGallery := classifyArchive(zr.File)
	// Keyed by the path under gallery/, which is what a light manifest names.
	galleryFiles := make(map[string]*zip.File, len(inGallery))
	for _, f := range inGallery {
		galleryFiles[strings.TrimPrefix(f.Name, "gallery/")] = f
	}

	var records []mergeRecord
	dataDir := filepath.Dir(cx.DBPath)
	switch {
	case innerLight != nil && innerDB == nil && innerJSON == nil:
		rc, err := innerLight.Open()
		if err != nil {
			return MergeResult{}, fmt.Errorf("open tags.json: %w", err)
		}
		mf, err := decodeLightManifest(rc)
		_ = rc.Close()
		if err != nil {
			return MergeResult{}, err
		}
		for _, img := range mf.Images {
			records = append(records, mergeRecord{SHA256: img.SHA256, Tags: img.Tags, SourcePath: img.Path})
		}
	case innerDB != nil:
		inner, err := extractZipEntryToTemp(innerDB, dataDir, maxBytes)
		if err != nil {
			return MergeResult{}, err
		}
		defer func() { _ = os.Remove(inner) }()
		records, err = readDBRecordsFromFile(inner, "inner db invalid")
		if err != nil {
			return MergeResult{}, err
		}
	case innerJSON != nil:
		rc, err := innerJSON.Open()
		if err != nil {
			return MergeResult{}, err
		}
		exp, err := DecodeExport(rc)
		_ = rc.Close()
		if err != nil {
			return MergeResult{}, err
		}
		records = readExportMergeRecords(exp)
	default:
		// Try the foreign-format translators before giving up.
		// A hydrus files+sidecar zip lands here because none
		// of the monbooru-native shapes (monbooru.{db,json}, tags.json) are
		// present at the archive root.
		if format := detectCompatFormat(zr.File); format != "" {
			return mergeFromCompatArchive(cx, zr.File, format, maxFileSizeMB)
		}
		return MergeResult{}, fmt.Errorf("archive missing monbooru.db, monbooru.json, or tags.json")
	}

	for i := range records {
		if rel := records[i].SourcePath; rel != "" {
			if entry, ok := galleryFiles[rel]; ok {
				records[i].zipEntry = entry
			}
		}
	}
	return applyMergeRecords(cx, records, ImportSourceNative, maxFileSizeMB), nil
}

// applyMergeRecords processes every record against the live gallery. When the
// record carries a zip entry and its SHA is unknown to the target, the entry
// is extracted into the gallery and ingested; otherwise only the tags are
// applied to the pre-existing image. The source string is propagated to
// every inserted image_tags row so the detail page can credit the importer.
//
// The returned counts are what the caller reports: an upload with no image
// bytes can only ever tag what the gallery already holds, and the operator
// has to hear that rather than read it off a gallery that did not change.
func applyMergeRecords(cx gallery.Handle, records []mergeRecord, source string, maxFileSizeMB int) MergeResult {
	var res MergeResult
	generalID := LookupCategoryID(cx.DB, "general")
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	for _, r := range records {
		var imgID int64
		err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, r.SHA256).Scan(&imgID)
		if err == nil {
			applyImportTagsToImage(cx.DB, cx.TagSvc, imgID, r.Tags, generalID, source)
			res.Tagged++
			continue
		}
		if err != sql.ErrNoRows {
			logx.Warnf("merge: lookup sha %s: %v", r.SHA256, err)
			res.Skipped++
			continue
		}
		if r.zipEntry == nil || r.SourcePath == "" {
			// No file available for this sha; tags-only merge skips missing targets.
			res.Skipped++
			continue
		}
		safeBase, err := SafeArchiveDest(cx.GalleryPath, r.SourcePath)
		if err != nil {
			logx.Warnf("merge: skipping entry %q: %v", r.SourcePath, err)
			res.Skipped++
			continue
		}
		// UniqueDestPath operates on (dir, basename); apply it relative to
		// the resolved parent so collisions are auto-suffixed within the
		// destination subdirectory rather than the gallery root.
		dst := gallery.UniqueDestPath(filepath.Dir(safeBase), filepath.Base(safeBase))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			logx.Warnf("merge: mkdir for %q: %v", r.SourcePath, err)
			res.Skipped++
			continue
		}
		if err := copyZipFile(r.zipEntry, dst, maxBytes); err != nil {
			logx.Warnf("merge: extract %q: %v", r.SourcePath, err)
			res.Skipped++
			continue
		}
		if _, err := gallery.DetectFileType(dst); err != nil {
			logx.Warnf("merge: unsupported file %q: %v", r.SourcePath, err)
			_ = os.Remove(dst)
			res.Skipped++
			continue
		}
		// Pass source through as origin so the detail page credits the
		// originating provider ('hydrus', 'blombooru', ...) instead of
		// reporting every compat-merged row as a generic 'ingest'. The
		// tags side already inherits source via tagger_name; this aligns
		// the image row's attribution with the tag rows.
		img, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, dst, source)
		if err != nil {
			logx.Warnf("merge: ingest %q: %v", r.SourcePath, err)
			_ = os.Remove(dst)
			res.Skipped++
			continue
		}
		applyImportTagsToImage(cx.DB, cx.TagSvc, img.ID, r.Tags, generalID, source)
		res.Added++
	}
	return res
}

// readDBMergeRecords extracts one record per non-missing image from a secondary
// SQLite file. Tags are emitted under their canonical name; aliases are skipped.
// Rides the same image-ordered join the light export uses, grouping off the
// cursor rather than issuing a tag query per image.
func readDBMergeRecords(src *db.DB) ([]mergeRecord, error) {
	var recs []mergeRecord
	if err := walkLightRows(src.Read, func(sha, relPath string, tags []string) {
		recs = append(recs, mergeRecord{SHA256: sha, SourcePath: relPath, Tags: tags})
	}); err != nil {
		return nil, err
	}
	return recs, nil
}

// readExportMergeRecords builds the same per-image record list from a parsed
// Export document (JSON import path).
func readExportMergeRecords(exp Export) []mergeRecord {
	catByID := map[int64]string{}
	for _, c := range exp.TagCategories {
		catByID[c.ID] = c.Name
	}
	tagTokens := map[int64]string{}
	for _, t := range exp.Tags {
		if t.IsAlias == 1 {
			continue
		}
		tagTokens[t.ID] = tagToken(catByID[t.CategoryID], t.Name)
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
			SourcePath: filepath.ToSlash(filepath.Join(img.FolderPath, storedBasename(img.CanonicalPath))),
		})
	}
	return recs
}

// applyImportTagsToImage resolves each "name" or "category:name" token and
// attaches it to imageID through the tag service so alias resolution and
// usage-count maintenance match the rest of the app. The source string is
// stored on every inserted image_tags row (`tagger_name`) so the detail
// page can credit the import provider - `"import"` for native exports,
// the format name (`"hydrus"`...) for compat archives.
//
// Tags resolve outside the write transaction so GetOrCreateTag can
// take its own writer slot; the per-image insert batch then opens a
// single tx for every image_tags row, which is the difference between
// O(tokens) and 1 commit on merges that pour dozens of tags onto each
// image.
func applyImportTagsToImage(database *db.DB, tagSvc *tags.Service, imageID int64, tokens []string, generalID int64, source string) {
	// Foreign imports drop a namespace with no matching category (species:fox
	// -> fox) so they land the same tags the monloader paths do; native keeps
	// colon-bearing names verbatim so an export round-trip stays lossless.
	tagIDs, _ := resolveTokenTagIDsConf(database, tagSvc, tokens, nil, generalID, source != ImportSourceNative, source)
	if err := tagSvc.AddTagsToImageFromTagger(imageID, tagIDs, false, source); err != nil {
		logx.Warnf("import tags to image %d: %v", imageID, err)
	}
}

// resolveTokenTagIDsConf resolves each "name" or "category:name" token to a tag
// id in the target gallery (creating the tag when absent, stamped with origin),
// keeping a confidence slice aligned to the surviving ids (confs[i] pairs with
// tokens[i]); a skipped token drops its confidence too. Pass nil confs to
// ignore scores.
func resolveTokenTagIDsConf(database *db.DB, tagSvc *tags.Service, tokens []string, confs []*float64, generalID int64, dropUnknownNamespace bool, origin string) ([]int64, []*float64) {
	tagIDs := make([]int64, 0, len(tokens))
	outConfs := make([]*float64, 0, len(tokens))
	for i, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		catID, bareName := resolveImportTag(database, token, generalID, dropUnknownNamespace)
		t, err := tagSvc.GetOrCreateTagFrom(bareName, catID, origin)
		if err != nil {
			logx.Warnf("import tag %q: %v", token, err)
			continue
		}
		tagIDs = append(tagIDs, t.ID)
		if confs != nil {
			outConfs = append(outConfs, confs[i])
		}
	}
	return tagIDs, outConfs
}

// applyTransferTags re-applies each attribution group onto the target image,
// preserving is_auto, the source / auto-tagger label and each auto-tag's
// confidence so a transferred tag keeps crediting its origin instead of
// collapsing into a manual user tag. The error propagates so a move can gate
// its source-delete on the tags actually landing.
func applyTransferTags(database *db.DB, tagSvc *tags.Service, imageID int64, groups []transferTagGroup, generalID int64) error {
	for _, g := range groups {
		// A transferred group's attribution label is the closest thing to
		// the creator of a tag the target gallery has never seen; an
		// unlabelled group is an anonymous UI add on the source side.
		origin := g.taggerName
		origin = cmp.Or(origin, "user")
		tagIDs, confs := resolveTokenTagIDsConf(database, tagSvc, g.tokens, g.confs, generalID, false, origin)
		if err := tagSvc.AddTagsToImageFromTaggerConf(imageID, tagIDs, confs, g.isAuto, g.taggerName); err != nil {
			return fmt.Errorf("transfer tags to image %d: %w", imageID, err)
		}
	}
	return nil
}

// resolveImportTag maps a "category:name" or bare token to a (categoryID, name).
// A namespace matching a category routes there; an unknown namespace is dropped
// to its subtag in general when dropUnknownNamespace is set (foreign imports),
// otherwise the whole token is kept as a general name (native round-trip).
func resolveImportTag(database *db.DB, token string, generalID int64, dropUnknownNamespace bool) (int64, string) {
	if idx := strings.Index(token, ":"); idx > 0 {
		var catID int64
		if err := database.Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, token[:idx],
		).Scan(&catID); err == nil {
			return catID, token[idx+1:]
		}
		if dropUnknownNamespace {
			return generalID, token[idx+1:]
		}
	}
	return generalID, token
}

func LookupCategoryID(database *db.DB, name string) int64 {
	var id int64
	_ = database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id)
	return id
}

// copyZipEntry copies the contents of f into dst with a per-entry
// decompressed-size cap. maxBytes <= 0 disables the cap (matches Sync
// and the watcher's "no limit" handling of MaxFileSizeMB=0); otherwise
// an entry whose decompressed payload exceeds maxBytes is rejected so a
// malicious archive can't fill the disk via compression ratio.
func copyZipEntry(dst io.Writer, f *zip.File, maxBytes int64) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	var src io.Reader = rc
	if maxBytes > 0 {
		src = io.LimitReader(rc, maxBytes+1)
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return err
	}
	if maxBytes > 0 && n > maxBytes {
		return fmt.Errorf("archive entry %q exceeds per-file limit of %d bytes", f.Name, maxBytes)
	}
	return nil
}

func copyZipFile(f *zip.File, dst string, maxBytes int64) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	return copyZipEntry(out, f, maxBytes)
}

func extractZipEntryToTemp(f *zip.File, dataDir string, maxBytes int64) (string, error) {
	tmp, err := os.CreateTemp(dataDir, "merge-inner-*")
	if err != nil {
		return "", err
	}
	copyErr := copyZipEntry(tmp, f, maxBytes)
	_ = tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmp.Name())
		return "", copyErr
	}
	return tmp.Name(), nil
}
