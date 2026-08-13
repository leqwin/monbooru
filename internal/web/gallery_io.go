package web

import (
	"archive/zip"
	"cmp"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

// safeArchiveDest joins a relative archive path under root and returns the
// resolved absolute destination, rejecting paths that escape root through
// `..` segments or absolute roots. Routes through gallery.PathInside so a
// legal name like "foo..bar.ext" isn't caught by a `..` substring check.
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

// galleryExportVersion is the full-export JSON document's `"version"`
// field. v2 carries manga_metadata + images.page_count + images.series;
// v3 adds image_collections, image_sources, image_annotations and
// images.note; v4 adds image_annotations.manual (operator-drawn boxes);
// v5 adds the relation tables (duplicate / alternate / version / derivative /
// not-related); v6 adds image_sources.similarity; v7 adds
// image_sources.original; v8 adds images.original_source. Older imports down to
// galleryExportMinSupported still round-trip (the new columns default, the
// new tables stay empty or, for image_collections, derive from
// images.series).
const galleryExportVersion = 8

// galleryExportMinSupported is the oldest full-export version this
// server reads; anything below is rejected.
const galleryExportMinSupported = 1

// lightManifestVersion is the on-disk version of the light tags.json
// manifest. The light format carries only sha256 / path / tags per
// row, so it doesn't track full-export schema bumps.
const lightManifestVersion = 1

// decodeGalleryExport reads a full-export JSON document and rejects any
// version outside the supported range. The caller owns opening/closing
// the reader.
func decodeGalleryExport(r io.Reader) (galleryExport, error) {
	var exp galleryExport
	if err := json.NewDecoder(r).Decode(&exp); err != nil {
		return exp, fmt.Errorf("decode export: %w", err)
	}
	if exp.Version < galleryExportMinSupported || exp.Version > galleryExportVersion {
		return exp, fmt.Errorf("unsupported export version %d (supported: %d..%d)", exp.Version, galleryExportMinSupported, galleryExportVersion)
	}
	return exp, nil
}

// galleryExport is the root JSON document. Field order mirrors schema.sql so a
// human opening the file reads the schema top-down.
type galleryExport struct {
	Version          int                  `json:"version"`
	GalleryName      string               `json:"gallery_name"`
	GalleryPath      string               `json:"gallery_path"`
	TagCategories    []tagCategoryRow     `json:"tag_categories"`
	Tags             []tagRow             `json:"tags"`
	TagImplications  []tagImplicationRow  `json:"tag_implications"`
	Images           []imageRow           `json:"images"`
	ImageCollections []imageCollectionRow `json:"image_collections,omitempty"`
	ImageSources     []imageSourceRow     `json:"image_sources,omitempty"`
	ImageAnnotations []imageAnnotationRow `json:"image_annotations,omitempty"`
	ImagePaths       []imagePathRow       `json:"image_paths"`
	ImageTags        []imageTagRow        `json:"image_tags"`
	SDMetadata       []sdMetadataRow      `json:"sd_metadata"`
	ComfyUIMetadata  []comfyMetadataRow   `json:"comfyui_metadata"`
	MangaMetadata    []mangaMetadataRow   `json:"manga_metadata,omitempty"`
	DupGroups        []dupGroupRow        `json:"dup_groups,omitempty"`
	DupGroupMembers  []dupGroupMemberRow  `json:"dup_group_members,omitempty"`
	AltGroups        []altGroupRow        `json:"alt_groups,omitempty"`
	AltGroupMembers  []altGroupMemberRow  `json:"alt_group_members,omitempty"`
	VersionEdges     []versionEdgeRow     `json:"version_edges,omitempty"`
	DerivativeEdges  []derivativeEdgeRow  `json:"derivative_edges,omitempty"`
	NotRelatedPairs  []notRelatedPairRow  `json:"not_related_pairs,omitempty"`
	SavedSearches    []savedSearchRow     `json:"saved_searches"`
}

type tagCategoryRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	IsBuiltin int    `json:"is_builtin"`
}

type tagRow struct {
	ID             int64          `json:"id"`
	Name           string         `json:"name"`
	CategoryID     int64          `json:"category_id"`
	UsageCount     int            `json:"usage_count"`
	IsAlias        int            `json:"is_alias"`
	CanonicalTagID sql.NullInt64  `json:"canonical_tag_id"`
	CreatedAt      string         `json:"created_at"`
	Origin         string         `json:"origin"`
	LastUsedAt     sql.NullString `json:"last_used_at"`
}

type imageRow struct {
	ID              int64           `json:"id"`
	SHA256          string          `json:"sha256"`
	CanonicalPath   string          `json:"canonical_path"`
	FolderPath      string          `json:"folder_path"`
	FileType        string          `json:"file_type"`
	Width           sql.NullInt64   `json:"width"`
	Height          sql.NullInt64   `json:"height"`
	FileSize        int64           `json:"file_size"`
	IsMissing       int             `json:"is_missing"`
	IsFavorited     int             `json:"is_favorited"`
	IsInbox         int             `json:"is_inbox"`
	AutoTaggedAt    sql.NullString  `json:"auto_tagged_at"`
	SourceType      string          `json:"source_type"`
	Origin          string          `json:"origin"`
	Source          string          `json:"source"`
	URL             string          `json:"url"`
	PageCount       sql.NullInt64   `json:"page_count,omitempty"`
	DurationSeconds sql.NullFloat64 `json:"duration_seconds,omitempty"`
	Series          string          `json:"collection,omitempty"`
	SeriesOrder     sql.NullInt64   `json:"collection_order,omitempty"`
	Note            string          `json:"note,omitempty"`
	OriginalSource  string          `json:"original_source,omitempty"`
	IngestedAt      string          `json:"ingested_at"`
}

type imageCollectionRow struct {
	ImageID  int64         `json:"image_id"`
	Name     string        `json:"name"`
	Position sql.NullInt64 `json:"position"`
}

type imageSourceRow struct {
	ImageID    int64   `json:"image_id"`
	Site       string  `json:"site"`
	PostID     string  `json:"post_id"`
	URL        string  `json:"url"`
	MD5        string  `json:"md5"`
	Commentary string  `json:"commentary"`
	Original   string  `json:"original,omitempty"`
	Similarity float64 `json:"similarity,omitempty"`
	FetchedAt  string  `json:"fetched_at"`
}

type imageAnnotationRow struct {
	ImageID   int64  `json:"image_id"`
	Site      string `json:"site"`
	PostID    string `json:"post_id"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	Body      string `json:"body"`
	Manual    int    `json:"manual,omitempty"`
	FetchedAt string `json:"fetched_at"`
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
	IsImplied  int             `json:"is_implied,omitempty"`
	Confidence sql.NullFloat64 `json:"confidence"`
	TaggerName sql.NullString  `json:"tagger_name"`
	CreatedAt  string          `json:"created_at"`
}

type tagImplicationRow struct {
	ParentTagID  int64  `json:"parent_tag_id"`
	ImpliedTagID int64  `json:"implied_tag_id"`
	CreatedAt    string `json:"created_at"`
	Origin       string `json:"origin"`
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

type mangaMetadataRow struct {
	ImageID         int64           `json:"image_id"`
	Title           sql.NullString  `json:"title"`
	Series          sql.NullString  `json:"series"`
	Number          sql.NullString  `json:"number"`
	Volume          sql.NullString  `json:"volume"`
	Count           sql.NullInt64   `json:"count"`
	Summary         sql.NullString  `json:"summary"`
	Notes           sql.NullString  `json:"notes"`
	Year            sql.NullInt64   `json:"year"`
	Month           sql.NullInt64   `json:"month"`
	Day             sql.NullInt64   `json:"day"`
	Writer          sql.NullString  `json:"writer"`
	Penciller       sql.NullString  `json:"penciller"`
	Inker           sql.NullString  `json:"inker"`
	Colorist        sql.NullString  `json:"colorist"`
	Letterer        sql.NullString  `json:"letterer"`
	CoverArtist     sql.NullString  `json:"cover_artist"`
	Editor          sql.NullString  `json:"editor"`
	Publisher       sql.NullString  `json:"publisher"`
	Imprint         sql.NullString  `json:"imprint"`
	Genre           sql.NullString  `json:"genre"`
	Web             sql.NullString  `json:"web"`
	LanguageISO     sql.NullString  `json:"language_iso"`
	Format          sql.NullString  `json:"format"`
	Manga           sql.NullString  `json:"manga"`
	AgeRating       sql.NullString  `json:"age_rating"`
	CommunityRating sql.NullFloat64 `json:"community_rating"`
	XMLPageCount    sql.NullInt64   `json:"xml_page_count"`
	RawXML          sql.NullString  `json:"raw_xml"`
}

type savedSearchRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Query     string `json:"query"`
	Sort      string `json:"sort"`
	Order     string `json:"sort_order"`
	Seed      string `json:"seed"`
	CreatedAt string `json:"created_at"`
}

type dupGroupRow struct {
	ID              int64  `json:"id"`
	OriginalImageID int64  `json:"original_image_id"`
	CreatedAt       string `json:"created_at"`
}

type dupGroupMemberRow struct {
	ImageID   int64  `json:"image_id"`
	GroupID   int64  `json:"group_id"`
	CreatedAt string `json:"created_at"`
}

type altGroupRow struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
}

type altGroupMemberRow struct {
	ImageID   int64  `json:"image_id"`
	GroupID   int64  `json:"group_id"`
	CreatedAt string `json:"created_at"`
}

type versionEdgeRow struct {
	ChildImageID  int64  `json:"child_image_id"`
	ParentImageID int64  `json:"parent_image_id"`
	CreatedAt     string `json:"created_at"`
}

type derivativeEdgeRow struct {
	DerivativeImageID int64  `json:"derivative_image_id"`
	SourceImageID     int64  `json:"source_image_id"`
	CreatedAt         string `json:"created_at"`
}

type notRelatedPairRow struct {
	AImageID  int64  `json:"a_image_id"`
	BImageID  int64  `json:"b_image_id"`
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
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := cx.DB.Write.Exec("VACUUM INTO ?", tmpPath); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
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

	streamRows(bw, "tag_categories", cx.DB,
		`SELECT id, name, color, is_builtin FROM tag_categories ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r tagCategoryRow
			err := rows.Scan(&r.ID, &r.Name, &r.Color, &r.IsBuiltin)
			return r, err
		})
	streamRows(bw, "tags", cx.DB,
		`SELECT id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at, origin, last_used_at FROM tags ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r tagRow
			err := rows.Scan(&r.ID, &r.Name, &r.CategoryID, &r.UsageCount, &r.IsAlias, &r.CanonicalTagID, &r.CreatedAt, &r.Origin, &r.LastUsedAt)
			return r, err
		})
	streamRows(bw, "tag_implications", cx.DB,
		`SELECT parent_tag_id, implied_tag_id, created_at, origin FROM tag_implications ORDER BY parent_tag_id, implied_tag_id`,
		func(rows *sql.Rows) (any, error) {
			var r tagImplicationRow
			err := rows.Scan(&r.ParentTagID, &r.ImpliedTagID, &r.CreatedAt, &r.Origin)
			return r, err
		})
	streamRows(bw, "images", cx.DB,
		`SELECT id, sha256, canonical_path, folder_path, file_type, width, height,
		        file_size, is_missing, is_favorited, is_inbox, auto_tagged_at, source_type, origin, source, url, page_count, duration_seconds, series, series_order, note, original_source, ingested_at
		 FROM images ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r imageRow
			err := rows.Scan(&r.ID, &r.SHA256, &r.CanonicalPath, &r.FolderPath, &r.FileType,
				&r.Width, &r.Height, &r.FileSize, &r.IsMissing, &r.IsFavorited, &r.IsInbox,
				&r.AutoTaggedAt, &r.SourceType, &r.Origin, &r.Source, &r.URL, &r.PageCount, &r.DurationSeconds, &r.Series, &r.SeriesOrder, &r.Note, &r.OriginalSource, &r.IngestedAt)
			return r, err
		})
	streamRows(bw, "image_collections", cx.DB,
		`SELECT image_id, name, position FROM image_collections ORDER BY image_id, name`,
		func(rows *sql.Rows) (any, error) {
			var r imageCollectionRow
			err := rows.Scan(&r.ImageID, &r.Name, &r.Position)
			return r, err
		})
	streamRows(bw, "image_sources", cx.DB,
		`SELECT image_id, site, post_id, url, md5, commentary, original, similarity, fetched_at FROM image_sources ORDER BY rowid`,
		func(rows *sql.Rows) (any, error) {
			var r imageSourceRow
			err := rows.Scan(&r.ImageID, &r.Site, &r.PostID, &r.URL, &r.MD5, &r.Commentary, &r.Original, &r.Similarity, &r.FetchedAt)
			return r, err
		})
	streamRows(bw, "image_annotations", cx.DB,
		`SELECT image_id, site, post_id, x, y, w, h, body, manual, fetched_at FROM image_annotations ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r imageAnnotationRow
			err := rows.Scan(&r.ImageID, &r.Site, &r.PostID, &r.X, &r.Y, &r.W, &r.H, &r.Body, &r.Manual, &r.FetchedAt)
			return r, err
		})
	streamRows(bw, "image_paths", cx.DB,
		`SELECT id, image_id, path, is_canonical FROM image_paths ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r imagePathRow
			err := rows.Scan(&r.ID, &r.ImageID, &r.Path, &r.IsCanonical)
			return r, err
		})
	streamRows(bw, "image_tags", cx.DB,
		`SELECT image_id, tag_id, is_auto, is_implied, confidence, tagger_name, created_at FROM image_tags`,
		func(rows *sql.Rows) (any, error) {
			var r imageTagRow
			err := rows.Scan(&r.ImageID, &r.TagID, &r.IsAuto, &r.IsImplied, &r.Confidence, &r.TaggerName, &r.CreatedAt)
			return r, err
		})
	streamRows(bw, "sd_metadata", cx.DB,
		`SELECT image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash FROM sd_metadata`,
		func(rows *sql.Rows) (any, error) {
			var r sdMetadataRow
			err := rows.Scan(&r.ImageID, &r.Prompt, &r.NegativePrompt, &r.Model, &r.Seed,
				&r.Sampler, &r.Steps, &r.CFGScale, &r.RawParams, &r.GenerationHash)
			return r, err
		})
	streamRows(bw, "comfyui_metadata", cx.DB,
		`SELECT image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash FROM comfyui_metadata`,
		func(rows *sql.Rows) (any, error) {
			var r comfyMetadataRow
			err := rows.Scan(&r.ImageID, &r.Prompt, &r.ModelCheckpoint, &r.Seed,
				&r.Sampler, &r.Steps, &r.CFGScale, &r.RawWorkflow, &r.GenerationHash)
			return r, err
		})
	streamRows(bw, "manga_metadata", cx.DB,
		`SELECT image_id, title, series, number, volume, count, summary, notes,
		        year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		        imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml
		 FROM manga_metadata`,
		func(rows *sql.Rows) (any, error) {
			var r mangaMetadataRow
			err := rows.Scan(&r.ImageID, &r.Title, &r.Series, &r.Number, &r.Volume, &r.Count, &r.Summary, &r.Notes,
				&r.Year, &r.Month, &r.Day, &r.Writer, &r.Penciller, &r.Inker, &r.Colorist, &r.Letterer, &r.CoverArtist,
				&r.Editor, &r.Publisher, &r.Imprint, &r.Genre, &r.Web, &r.LanguageISO, &r.Format, &r.Manga, &r.AgeRating,
				&r.CommunityRating, &r.XMLPageCount, &r.RawXML)
			return r, err
		})
	streamRows(bw, "dup_groups", cx.DB,
		`SELECT id, original_image_id, created_at FROM dup_groups ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r dupGroupRow
			err := rows.Scan(&r.ID, &r.OriginalImageID, &r.CreatedAt)
			return r, err
		})
	streamRows(bw, "dup_group_members", cx.DB,
		`SELECT image_id, group_id, created_at FROM dup_group_members ORDER BY image_id`,
		func(rows *sql.Rows) (any, error) {
			var r dupGroupMemberRow
			err := rows.Scan(&r.ImageID, &r.GroupID, &r.CreatedAt)
			return r, err
		})
	streamRows(bw, "alt_groups", cx.DB,
		`SELECT id, created_at FROM alt_groups ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r altGroupRow
			err := rows.Scan(&r.ID, &r.CreatedAt)
			return r, err
		})
	streamRows(bw, "alt_group_members", cx.DB,
		`SELECT image_id, group_id, created_at FROM alt_group_members ORDER BY image_id`,
		func(rows *sql.Rows) (any, error) {
			var r altGroupMemberRow
			err := rows.Scan(&r.ImageID, &r.GroupID, &r.CreatedAt)
			return r, err
		})
	streamRows(bw, "version_edges", cx.DB,
		`SELECT child_image_id, parent_image_id, created_at FROM version_edges ORDER BY child_image_id`,
		func(rows *sql.Rows) (any, error) {
			var r versionEdgeRow
			err := rows.Scan(&r.ChildImageID, &r.ParentImageID, &r.CreatedAt)
			return r, err
		})
	streamRows(bw, "derivative_edges", cx.DB,
		`SELECT derivative_image_id, source_image_id, created_at FROM derivative_edges ORDER BY derivative_image_id`,
		func(rows *sql.Rows) (any, error) {
			var r derivativeEdgeRow
			err := rows.Scan(&r.DerivativeImageID, &r.SourceImageID, &r.CreatedAt)
			return r, err
		})
	streamRows(bw, "not_related_pairs", cx.DB,
		`SELECT a_image_id, b_image_id, created_at FROM not_related_pairs ORDER BY a_image_id, b_image_id`,
		func(rows *sql.Rows) (any, error) {
			var r notRelatedPairRow
			err := rows.Scan(&r.AImageID, &r.BImageID, &r.CreatedAt)
			return r, err
		})
	streamRows(bw, "saved_searches", cx.DB,
		`SELECT id, name, query, sort, sort_order, seed, created_at FROM saved_searches ORDER BY id`,
		func(rows *sql.Rows) (any, error) {
			var r savedSearchRow
			err := rows.Scan(&r.ID, &r.Name, &r.Query, &r.Sort, &r.Order, &r.Seed, &r.CreatedAt)
			return r, err
		})
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
	defer func() { _ = zw.Close() }()

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

	if err := writeGalleryFilesToZip(zw, cx.GalleryPath); err != nil {
		return err
	}
	// Close writes the central directory; until it succeeds the response
	// body is not a readable archive. The deferred close stays for the
	// error paths, where its already-closed error is discarded.
	return zw.Close()
}

// writeGalleryFilesToZip walks galleryPath and appends every file under it
// as `gallery/<relative_path>` entries in zw, using zip.Store for the
// already-compressed image payloads. A missing root surfaces as an empty
// section (degraded mode) so the inner db/json still rides along for a
// headers-only restore.
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
		defer func() { _ = f.Close() }()
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
	s.cfgMu.RLock()
	isDefault := name == s.cfg.DefaultGallery
	s.cfgMu.RUnlock()
	if isDefault {
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
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, upload); err != nil {
		_ = tmp.Close()
		s.ctxMu.Unlock()
		return fmt.Errorf("buffer upload: %w", err)
	}
	_ = tmp.Close()

	// Close the target DB and stop its watcher before touching on-disk state.
	cx.close()

	applyErr := applyImport(format, tmpPath, dbPath, thumbsPath, galleryPath, s.maxFileSizeMB())

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
	watch, maxMB := s.watcherSettings()
	newCx.startWatcher(watch, maxMB, s.jobs)
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
// early-return error paths are linear. maxFileSizeMB caps each archive
// entry's decompressed size; <= 0 disables the cap.
func applyImport(format, tmpPath, dbPath, thumbsPath, galleryPath string, maxFileSizeMB int) error {
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
		return replaceFromArchive(tmpPath, dbPath, thumbsPath, galleryPath, maxFileSizeMB)
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
	defer func() { _ = f.Close() }()
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
	if err := resetDBAndThumbs(dbPath, thumbsPath); err != nil {
		return err
	}
	if err := os.Rename(srcPath, dbPath); err != nil {
		return fmt.Errorf("install db: %w", err)
	}
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("reopen installed db: %w", err)
	}
	defer func() { _ = database.Close() }()
	// Bring the imported snapshot up to current schema before any sanitisation
	// pass touches it. Today's sanitisers happen to query columns present in
	// every monbooru release; a future helper that touches a newer column
	// would otherwise hit `no such column` only on imports of older DBs.
	if err := db.Bootstrap(database); err != nil {
		return fmt.Errorf("bootstrap imported db: %w", err)
	}
	if err := sanitizeImportedCategoryColors(database); err != nil {
		return fmt.Errorf("sanitize colors: %w", err)
	}
	if err := sanitizeImportedAliasChains(database); err != nil {
		return fmt.Errorf("sanitize alias chains: %w", err)
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
			_ = rows.Close()
			return err
		}
		if !tags.IsValidCategoryColor(r.color) {
			bad = append(bad, r)
		}
	}
	_ = rows.Close()
	for _, r := range bad {
		safe := tags.SafeCategoryColor(r.color)
		logx.Warnf("import: replaced invalid color %q on tag_category id=%d with %s", r.color, r.id, safe)
		if _, err := database.Write.Exec(
			`UPDATE tag_categories SET color = ? WHERE id = ?`, safe, r.id,
		); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeImportedAliasChains normalizes the canonical_tag_id graph of a
// freshly imported DB. The write paths keep it one hop deep and only on
// alias rows, but imported rows arrive verbatim, and the resolvers follow
// COALESCE(canonical_tag_id, id) exactly once with no is_alias check - a
// chained alias silently drops out of search and a pointer on a plain tag
// misdirects it. Aliases resolving through other aliases are re-pointed at
// their terminal plain tag; an alias with nowhere to land (cycle member,
// missing canonical) is promoted to a plain tag; a plain tag's stray
// pointer is cleared.
func sanitizeImportedAliasChains(database *db.DB) error {
	type node struct {
		alias     bool
		canonical int64 // 0 when NULL
	}
	rows, err := database.Read.Query(`SELECT id, is_alias, COALESCE(canonical_tag_id, 0) FROM tags`)
	if err != nil {
		return err
	}
	nodes := make(map[int64]node)
	var ids []int64
	for rows.Next() {
		var id, canonical int64
		var alias int
		if err := rows.Scan(&id, &alias, &canonical); err != nil {
			_ = rows.Close()
			return err
		}
		nodes[id] = node{alias: alias == 1, canonical: canonical}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	// Sorted walk order keeps the promoted cycle member deterministic.
	slices.Sort(ids)

	// root[id] is the plain tag id's pointer chain lands on; promoted rows
	// become their own root.
	root := make(map[int64]int64)
	promoted := make(map[int64]bool)
	for _, id := range ids {
		if n := nodes[id]; !n.alias {
			continue
		}
		if _, done := root[id]; done {
			continue
		}
		var path []int64
		onPath := make(map[int64]bool)
		cur := id
		var end int64
		for {
			n := nodes[cur]
			if !n.alias {
				end = cur
				break
			}
			if r, ok := root[cur]; ok {
				end = r
				break
			}
			if _, ok := nodes[n.canonical]; !ok || onPath[cur] {
				end = cur
				promoted[cur] = true
				break
			}
			onPath[cur] = true
			path = append(path, cur)
			cur = n.canonical
		}
		root[end] = end
		for _, p := range path {
			root[p] = end
		}
	}

	for _, id := range ids {
		n := nodes[id]
		switch {
		case !n.alias && n.canonical != 0:
			if _, err := database.Write.Exec(
				`UPDATE tags SET canonical_tag_id = NULL WHERE id = ?`, id,
			); err != nil {
				return err
			}
			logx.Warnf("import: cleared the canonical pointer on plain tag id=%d", id)
		case !n.alias:
		case promoted[id]:
			if _, err := database.Write.Exec(
				`UPDATE tags SET is_alias = 0, canonical_tag_id = NULL WHERE id = ?`, id,
			); err != nil {
				return err
			}
			logx.Warnf("import: promoted alias id=%d to a plain tag (alias cycle or missing canonical)", id)
		case root[id] != n.canonical:
			if _, err := database.Write.Exec(
				`UPDATE tags SET canonical_tag_id = ? WHERE id = ?`, root[id], id,
			); err != nil {
				return err
			}
			logx.Warnf("import: re-pointed alias id=%d through its alias chain to tag id=%d", id, root[id])
		}
	}
	return nil
}

// replaceDBFromJSON decodes the uploaded JSON document, builds a fresh DB
// from it at a temp path, and only once the load fully succeeded swaps it
// over the target - so a failed import leaves the target gallery untouched.
// Keeps primary keys from the export so image_tags still line up. Also
// rebases every canonical_path / image_paths.path onto the target
// gallery_path so a cross-root import doesn't dangle every link.
func replaceDBFromJSON(srcPath, dbPath, thumbsPath, galleryPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open json: %w", err)
	}
	defer func() { _ = f.Close() }()
	exp, err := decodeGalleryExport(f)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dbPath), "import-*.db")
	if err != nil {
		return fmt.Errorf("create temp db: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() {
		for _, p := range []string{tmpPath, tmpPath + "-wal", tmpPath + "-shm"} {
			_ = os.Remove(p)
		}
	}()
	database, err := db.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open new db: %w", err)
	}
	loadErr := func() error {
		if err := db.Bootstrap(database); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		if err := loadExportIntoDB(database, exp); err != nil {
			return err
		}
		if err := sanitizeImportedAliasChains(database); err != nil {
			return fmt.Errorf("sanitize alias chains: %w", err)
		}
		return rebaseImagePaths(database, galleryPath)
	}()
	if loadErr == nil {
		// Fold the WAL into the main file so the rename below moves every
		// committed page.
		if _, err := database.Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			loadErr = fmt.Errorf("checkpoint: %w", err)
		}
	}
	if err := database.Close(); err != nil && loadErr == nil {
		loadErr = fmt.Errorf("close new db: %w", err)
	}
	if loadErr != nil {
		return loadErr
	}

	if err := resetDBAndThumbs(dbPath, thumbsPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dbPath); err != nil {
		return fmt.Errorf("install db: %w", err)
	}
	return nil
}

// replaceFromArchive opens the uploaded ZIP, extracts the inner DB or JSON
// via the matching replaceDBFrom* helper, and when `gallery/` entries are
// present wipes the source folder and extracts them into it. maxFileSizeMB
// caps each entry's decompressed size; <= 0 disables the cap.
func replaceFromArchive(srcPath, dbPath, thumbsPath, galleryPath string, maxFileSizeMB int) error {
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

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
			return replaceFromCompatArchive(zr.File, format, dbPath, thumbsPath, galleryPath, maxFileSizeMB)
		}
		return fmt.Errorf("archive missing monbooru.db, monbooru.json, or tags.json")
	}
	// A light archive ships only tags.json + gallery/; route to the light
	// replacer which bootstraps a fresh db and ingests each image. A full
	// archive takes priority when both a monbooru.{db,json} and a tags.json
	// are present - that combination is unusual but the full payload wins.
	if innerDB == nil && innerJSON == nil {
		return replaceFromLightArchive(innerLight, galleryFiles, dbPath, thumbsPath, galleryPath, maxFileSizeMB)
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
	defer func() { _ = os.Remove(innerTmpPath) }()

	var innerFile *zip.File
	var applyInner func(string, string, string, string) error
	if innerDB != nil {
		innerFile = innerDB
		applyInner = replaceDBFromFile
	} else {
		innerFile = innerJSON
		applyInner = replaceDBFromJSON
	}
	if err := copyZipEntry(innerTmp, innerFile, maxBytes); err != nil {
		_ = innerTmp.Close()
		return err
	}
	_ = innerTmp.Close()
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
		if err := copyZipFile(f, dst, maxBytes); err != nil {
			return err
		}
	}

	// applyInner reconciled is_missing before these files existed on disk;
	// redo it now that the archive's images are extracted.
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("reopen db for reconcile: %w", err)
	}
	defer func() { _ = database.Close() }()
	return reconcileMissingFiles(database, galleryPath)
}

// storedBasename is filepath.Base for a path read back out of a
// database or an export. Those carry the separators of the machine
// that wrote them, which filepath.Base on another OS would not
// recognise as separators at all.
func storedBasename(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
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
	defer func() { _ = tx.Rollback() }()

	// Ahead of the scan, and ahead of any canonical_path rewrite: the
	// statement keys off the separators in canonical_path to prove the
	// row came from Windows, and the rebase below erases that evidence.
	if _, err := tx.Exec(db.NormalizeWindowsFolderPathSQL); err != nil {
		return fmt.Errorf("normalize folder paths: %w", err)
	}

	imgs, err := loadImagePathRows(tx)
	if err != nil {
		return fmt.Errorf("scan images for rebase: %w", err)
	}

	// Infer the source-root from the first canonical row: each row
	// stores canonical_path = <sourceRoot>/<folder_path>/<basename>, so
	// stripping the trailing folder+basename leaves the root the export
	// came from. Used below to rebase alias paths from their stored
	// folder rather than the canonical's folder - operator-maintained
	// aliases in non-canonical folders survive the rebase intact.
	sourceRoot := ""
	for _, r := range imgs {
		if r.canonical == "" {
			continue
		}
		suffix := filepath.Join(r.folder, storedBasename(r.canonical))
		if r.folder == "" {
			suffix = storedBasename(r.canonical)
		}
		if strings.HasSuffix(r.canonical, string(filepath.Separator)+suffix) {
			sourceRoot = strings.TrimSuffix(r.canonical, string(filepath.Separator)+suffix)
			break
		}
		if r.canonical == suffix {
			sourceRoot = ""
			break
		}
	}

	// image_paths has its own absolute column; rebuild it from the row's
	// image_id by looking up the matching image's folder_path + basename.
	// Image_paths include the canonical (is_canonical=1) and any aliases.
	// We rebuild each one by computing newPath the same way.
	for _, r := range imgs {
		newCanonical := filepath.Join(root, r.folder, storedBasename(r.canonical))
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
		`SELECT ip.id, ip.image_id, ip.path, ip.is_canonical, i.folder_path
		 FROM image_paths ip
		 JOIN images i ON i.id = ip.image_id`,
	)
	if err != nil {
		return fmt.Errorf("scan image_paths for rebase: %w", err)
	}
	type pathRow struct {
		id, imageID int64
		path        string
		isCanonical bool
		folder      string
	}
	var paths []pathRow
	for pathRows.Next() {
		var r pathRow
		var isCanon int
		if err := pathRows.Scan(&r.id, &r.imageID, &r.path, &isCanon, &r.folder); err != nil {
			_ = pathRows.Close()
			return err
		}
		r.isCanonical = isCanon == 1
		paths = append(paths, r)
	}
	if err := pathRows.Err(); err != nil {
		_ = pathRows.Close()
		return err
	}
	_ = pathRows.Close()

	for _, p := range paths {
		// Canonical rows always rebase to the row's stored folder_path so
		// the images.canonical_path computed above agrees. Alias rows
		// preserve their original folder by stripping the source root
		// from their stored path; this keeps an alias that lived at
		// /old/photos/cat.jpg landing at /new/photos/cat.jpg instead of
		// being collapsed into the canonical's folder (which would also
		// risk a UNIQUE-on-path collision with the canonical row).
		var newPath string
		if p.isCanonical || sourceRoot == "" {
			newPath = filepath.Join(root, p.folder, storedBasename(p.path))
		} else if rel := strings.TrimPrefix(p.path, sourceRoot+string(filepath.Separator)); rel != p.path {
			newPath = filepath.Join(root, rel)
		} else {
			// Alias path was outside the inferred source root (operator
			// hand-edit). Fall back to the canonical-folder shape rather
			// than leaving the alias dangling at the foreign absolute.
			newPath = filepath.Join(root, p.folder, storedBasename(p.path))
		}
		if newPath == p.path {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET path = ? WHERE id = ?`, newPath, p.id,
		); err != nil {
			return fmt.Errorf("update image_path %d: %w", p.id, err)
		}
	}

	// After rebasing, reconcile is_missing against the target root in both
	// directions: flag a row whose canonical file is absent (so missing:true
	// surfaces it instead of a healthy-looking row that 404s on click), and
	// clear a row that was exported missing but exists here (otherwise it
	// stays hidden until a manual Sync the import flow never queues). Mirrors
	// what Sync does for vanished/reappeared files.
	if err := reconcileMissingFlags(tx, root, imgs); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return recalcImportedTagCounts(database)
}

// recalcImportedTagCounts rebases usage_count on what the reconcile just
// decided is visible. The export carries the counts verbatim, so an
// import whose files are not at the target path leaves every tag
// claiming usages the gallery cannot show - "1 match" over "No images
// found" until the operator finds Maintenance -> Recalculate.
func recalcImportedTagCounts(database *db.DB) error {
	if _, err := tags.RecalcDBCount(database); err != nil {
		return fmt.Errorf("recalculate tag counts: %w", err)
	}
	return nil
}

// imgPathRow is the stored path triple the rebase and reconcile passes walk.
type imgPathRow struct {
	id        int64
	folder    string
	canonical string
}

// loadImagePathRows reads every image's stored paths. The rebase passes its
// transaction, since it rewrites what it reads; reconcile passes the pool.
func loadImagePathRows(q db.Querier) ([]imgPathRow, error) {
	return db.QueryAll(q, func(rows *sql.Rows) (imgPathRow, error) {
		var r imgPathRow
		err := rows.Scan(&r.id, &r.folder, &r.canonical)
		return r, err
	}, `SELECT id, folder_path, canonical_path FROM images`)
}

// reconcileMissingFlags re-derives is_missing from what is on disk under
// root, in both directions: a row whose canonical file is absent is flagged
// so missing:true surfaces it instead of a healthy-looking row that 404s on
// click, and a row exported missing but present here is cleared.
func reconcileMissingFlags(x db.Execer, root string, imgs []imgPathRow) error {
	for _, r := range imgs {
		flag := fileMissingFlag(root, r.folder, r.canonical)
		if _, err := x.Exec(`UPDATE images SET is_missing = ? WHERE id = ?`, flag, r.id); err != nil {
			return fmt.Errorf("reconcile is_missing for image %d: %w", r.id, err)
		}
	}
	return nil
}

// fileMissingFlag reports 1 when the image's canonical file is absent from
// galleryRoot and 0 when it is present, matching what Sync records.
func fileMissingFlag(galleryRoot, folder, canonical string) int {
	if _, err := os.Stat(filepath.Join(galleryRoot, folder, storedBasename(canonical))); err == nil {
		return 0
	}
	return 1
}

// reconcileMissingFiles re-runs the is_missing reconcile against galleryPath.
// The archive import installs the DB before extracting its bundled images, so
// this second pass runs once the files are actually on disk.
func reconcileMissingFiles(database *db.DB, galleryPath string) error {
	root := strings.TrimRight(galleryPath, "/")
	imgs, err := loadImagePathRows(database.Read)
	if err != nil {
		return fmt.Errorf("scan images for reconcile: %w", err)
	}
	if err := reconcileMissingFlags(database.Write, root, imgs); err != nil {
		return err
	}
	return recalcImportedTagCounts(database)
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
	defer func() { _ = database.Close() }()
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
	defer func() { _ = tx.Rollback() }()

	// Defer FK checks until COMMIT so alias rows can reference canonical
	// tags that haven't been inserted yet (tags are emitted in ID order,
	// and an alias created before its canonical legitimately has a lower
	// id). The pragma is scoped to the current transaction.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer fk: %w", err)
	}

	// Wipe every table the export populates so seeded rows from
	// db.Bootstrap (built-in categories, canonical rating tags, anything
	// future seeds add) can't collide with imported ids. Order respects
	// FK dependencies and `defer_foreign_keys = ON` smooths over the
	// rest until commit.
	for _, stmt := range []string{
		`DELETE FROM image_tags`,
		`DELETE FROM image_tag_sources`,
		`DELETE FROM tag_implications`,
		`DELETE FROM image_paths`,
		`DELETE FROM image_collections`,
		`DELETE FROM image_sources`,
		`DELETE FROM image_annotations`,
		`DELETE FROM sd_metadata`,
		`DELETE FROM comfyui_metadata`,
		`DELETE FROM manga_metadata`,
		`DELETE FROM saved_searches`,
		`DELETE FROM dup_group_members`,
		`DELETE FROM dup_groups`,
		`DELETE FROM alt_group_members`,
		`DELETE FROM alt_groups`,
		`DELETE FROM version_edges`,
		`DELETE FROM derivative_edges`,
		`DELETE FROM not_related_pairs`,
		`DELETE FROM images`,
		`DELETE FROM tags`,
		`DELETE FROM tag_categories`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("reset table: %w", err)
		}
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
		if _, err := tx.Exec(
			`INSERT INTO tags (id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at, origin, last_used_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Name, r.CategoryID, r.UsageCount, r.IsAlias, r.CanonicalTagID, r.CreatedAt, r.Origin, r.LastUsedAt,
		); err != nil {
			return fmt.Errorf("insert tag %d: %w", r.ID, err)
		}
	}
	for _, r := range exp.TagImplications {
		if _, err := tx.Exec(
			`INSERT INTO tag_implications (parent_tag_id, implied_tag_id, created_at, origin) VALUES (?, ?, ?, ?)`,
			r.ParentTagID, r.ImpliedTagID, r.CreatedAt, r.Origin,
		); err != nil {
			return fmt.Errorf("insert tag_implication (%d→%d): %w", r.ParentTagID, r.ImpliedTagID, err)
		}
	}
	for _, r := range exp.Images {
		if _, err := tx.Exec(
			`INSERT INTO images (id, sha256, canonical_path, folder_path, file_type, width, height,
			                    file_size, is_missing, is_favorited, is_inbox, auto_tagged_at, source_type, origin, source, url, page_count, duration_seconds, series, series_order, note, original_source, ingested_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.SHA256, r.CanonicalPath, r.FolderPath, r.FileType, r.Width, r.Height,
			r.FileSize, r.IsMissing, r.IsFavorited, r.IsInbox, r.AutoTaggedAt, r.SourceType, r.Origin, r.Source, r.URL, r.PageCount, r.DurationSeconds, r.Series, r.SeriesOrder, r.Note, r.OriginalSource, r.IngestedAt,
		); err != nil {
			return fmt.Errorf("insert image %d: %w", r.ID, err)
		}
	}
	if exp.Version < 3 {
		// Pre-v3 exports carry no image_collections table; the memberships
		// derive from each image's home label mirror.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_collections (image_id, name, position)
			 SELECT id, series, series_order FROM images WHERE series != ''`,
		); err != nil {
			return fmt.Errorf("seed image_collections: %w", err)
		}
	}
	for _, r := range exp.ImageCollections {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_collections (image_id, name, position) VALUES (?, ?, ?)`,
			r.ImageID, r.Name, r.Position,
		); err != nil {
			return fmt.Errorf("insert image_collection (%d,%q): %w", r.ImageID, r.Name, err)
		}
	}
	for _, r := range exp.ImageSources {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_sources (image_id, site, post_id, url, md5, commentary, original, similarity, fetched_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, r.Site, r.PostID, r.URL, r.MD5, r.Commentary, r.Original, r.Similarity, r.FetchedAt,
		); err != nil {
			return fmt.Errorf("insert image_source (%d,%q): %w", r.ImageID, r.Site, err)
		}
	}
	for _, r := range exp.ImageAnnotations {
		if _, err := tx.Exec(
			`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body, manual, fetched_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, r.Site, r.PostID, r.X, r.Y, r.W, r.H, r.Body, r.Manual, r.FetchedAt,
		); err != nil {
			return fmt.Errorf("insert image_annotation (image %d): %w", r.ImageID, err)
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
			`INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, r.TagID, r.IsAuto, r.IsImplied, conf, tname, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert image_tag (%d,%d): %w", r.ImageID, r.TagID, err)
		}
	}
	// The export format doesn't carry the source ledger; derive it the
	// way the upgrade backfill does, so an imported library answers
	// per-tag provenance the same as one that migrated in place.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source, created_at)
		 SELECT image_id, tag_id, COALESCE(NULLIF(tagger_name, ''), 'user'), created_at
		 FROM image_tags WHERE is_implied = 0`,
	); err != nil {
		return fmt.Errorf("backfill image_tag_sources: %w", err)
	}
	for _, r := range exp.SDMetadata {
		if _, err := tx.Exec(
			`INSERT INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, r.Prompt, r.NegativePrompt, r.Model,
			r.Seed, r.Sampler, r.Steps,
			r.CFGScale, r.RawParams, r.GenerationHash,
		); err != nil {
			return fmt.Errorf("insert sd_metadata %d: %w", r.ImageID, err)
		}
	}
	for _, r := range exp.ComfyUIMetadata {
		if _, err := tx.Exec(
			`INSERT INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, r.Prompt, r.ModelCheckpoint,
			r.Seed, r.Sampler, r.Steps,
			r.CFGScale, r.RawWorkflow, r.GenerationHash,
		); err != nil {
			return fmt.Errorf("insert comfyui_metadata %d: %w", r.ImageID, err)
		}
	}
	for _, r := range exp.MangaMetadata {
		if _, err := tx.Exec(
			`INSERT INTO manga_metadata (image_id, title, series, number, volume, count, summary, notes,
			     year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
			     imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImageID, r.Title, r.Series, r.Number, r.Volume,
			r.Count, r.Summary, r.Notes,
			r.Year, r.Month, r.Day,
			r.Writer, r.Penciller, r.Inker, r.Colorist,
			r.Letterer, r.CoverArtist, r.Editor, r.Publisher,
			r.Imprint, r.Genre, r.Web, r.LanguageISO,
			r.Format, r.Manga, r.AgeRating,
			r.CommunityRating, r.XMLPageCount, r.RawXML,
		); err != nil {
			return fmt.Errorf("insert manga_metadata %d: %w", r.ImageID, err)
		}
	}
	for _, r := range exp.DupGroups {
		if _, err := tx.Exec(
			`INSERT INTO dup_groups (id, original_image_id, created_at) VALUES (?, ?, ?)`,
			r.ID, r.OriginalImageID, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert dup_group %d: %w", r.ID, err)
		}
	}
	for _, r := range exp.DupGroupMembers {
		if _, err := tx.Exec(
			`INSERT INTO dup_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			r.ImageID, r.GroupID, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert dup_group_member %d: %w", r.ImageID, err)
		}
	}
	for _, r := range exp.AltGroups {
		if _, err := tx.Exec(
			`INSERT INTO alt_groups (id, created_at) VALUES (?, ?)`,
			r.ID, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert alt_group %d: %w", r.ID, err)
		}
	}
	for _, r := range exp.AltGroupMembers {
		if _, err := tx.Exec(
			`INSERT INTO alt_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			r.ImageID, r.GroupID, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert alt_group_member %d: %w", r.ImageID, err)
		}
	}
	for _, r := range exp.VersionEdges {
		if _, err := tx.Exec(
			`INSERT INTO version_edges (child_image_id, parent_image_id, created_at) VALUES (?, ?, ?)`,
			r.ChildImageID, r.ParentImageID, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert version_edge %d: %w", r.ChildImageID, err)
		}
	}
	for _, r := range exp.DerivativeEdges {
		if _, err := tx.Exec(
			`INSERT INTO derivative_edges (derivative_image_id, source_image_id, created_at) VALUES (?, ?, ?)`,
			r.DerivativeImageID, r.SourceImageID, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert derivative_edge %d: %w", r.DerivativeImageID, err)
		}
	}
	for _, r := range exp.NotRelatedPairs {
		if _, err := tx.Exec(
			`INSERT INTO not_related_pairs (a_image_id, b_image_id, created_at) VALUES (?, ?, ?)`,
			r.AImageID, r.BImageID, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert not_related_pair (%d,%d): %w", r.AImageID, r.BImageID, err)
		}
	}
	for _, r := range exp.SavedSearches {
		if _, err := tx.Exec(
			`INSERT INTO saved_searches (id, name, query, sort, sort_order, seed, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Name, r.Query, r.Sort, r.Order, r.Seed, r.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert saved_search %d: %w", r.ID, err)
		}
	}
	return tx.Commit()
}

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
func streamRows(j *jsonWriter, key string, database *db.DB, query string, scan func(*sql.Rows) (any, error)) {
	if j.err != nil {
		return
	}
	j.arrayStart(key)
	first := true
	rows, err := database.Read.Query(query)
	if err != nil {
		j.arrayEnd()
		j.err = err
		return
	}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			_ = rows.Close()
			j.arrayEnd()
			j.err = err
			return
		}
		j.arrayItem(&first, v)
		if j.err != nil {
			_ = rows.Close()
			j.arrayEnd()
			return
		}
	}
	if err := rows.Err(); err != nil && j.err == nil {
		j.err = err
	}
	_ = rows.Close()
	j.arrayEnd()
}

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
// Expects a multipart form with `mode`, `confirm_name` (replace only),
// and `file`. The handler reads parts in order with MultipartReader so
// the type-to-confirm gate runs before the (possibly multi-GB) file
// part is consumed; this requires the dialog template to put mode and
// confirm_name fields ahead of the file input. CSRF is validated by
// the middleware off the X-CSRF-Token header so it never triggers
// implicit form parsing on this route.
func (s *Server) settingsGalleryImport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	const maxImport = 16 << 30 // 16 GiB cap; protects against runaway uploads on a LAN setup.
	r.Body = http.MaxBytesReader(w, r.Body, maxImport)

	mr, err := r.MultipartReader()
	if err != nil {
		writeInlineFlash(w, "err", "expected multipart/form-data")
		return
	}

	const maxFieldBytes = 1 << 20 // 1 MiB per form field; values are short
	fields := map[string]string{}
	var filePart *multipart.Part
	var fileFilename string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeInlineFlash(w, "err", "malformed upload")
			return
		}
		if part.FileName() == "" {
			body, readErr := io.ReadAll(io.LimitReader(part, maxFieldBytes))
			_ = part.Close()
			if readErr != nil {
				writeInlineFlash(w, "err", "malformed upload")
				return
			}
			fields[part.FormName()] = strings.TrimSpace(string(body))
			continue
		}
		filePart = part
		fileFilename = part.FileName()
		break
	}
	if filePart == nil {
		writeInlineFlash(w, "err", "missing file")
		return
	}
	defer func() { _ = filePart.Close() }()

	mode := fields["mode"]
	mode = cmp.Or(mode, "replace")
	if mode != "replace" && mode != "merge" {
		writeInlineFlash(w, "err", "mode must be replace or merge")
		return
	}
	if mode == "replace" {
		if fields["confirm_name"] != name {
			writeInlineFlash(w, "err", "type-to-confirm name does not match")
			return
		}
	}
	format := formatFromExt(fileFilename)
	if format == "" {
		writeInlineFlash(w, "err", "file must be .db, .json, or .zip")
		return
	}

	if mode == "merge" {
		if err := s.MergeGallery(name, format, filePart); err != nil {
			writeInlineFlash(w, "err", err.Error())
			return
		}
		// Mirror the replace path (ImportGallery → SwitchGallery): a merge
		// brings new images into the target gallery, so the user expects to
		// land on it. No-op if the target is already active.
		if err := s.SwitchGallery(name); err != nil {
			logx.Infof("gallery %q: post-merge switch skipped: %v", name, err)
		}
		writeInlineFlash(w, "ok", "Gallery "+name+" merged.")
		return
	}
	if err := s.ImportGallery(name, format, filePart); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	// Write the success flash into #flash-galleries; the dialog's
	// after-request hook detects the flash-ok, closes the modal, and
	// triggers a reload so the newly-active gallery badge shows.
	writeInlineFlash(w, "ok", "Gallery "+name+" imported. Rebuilding thumbnails in the background.")
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
