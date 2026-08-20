package models

import (
	"net/url"
	"time"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

const (
	FileTypeJPEG = "jpeg"
	FileTypePNG  = "png"
	FileTypeWEBP = "webp"
	FileTypeGIF  = "gif"
	FileTypeMP4  = "mp4"
	FileTypeWEBM = "webm"
	// FileTypeCBZ covers `.cbz` and `.zip` archives ingested as a single
	// manga row. Page bytes are extracted lazily into a per-image cache;
	// the cover thumbnail is page 1.
	FileTypeCBZ = "cbz"

	SourceTypeA1111   = "a1111"
	SourceTypeComfyUI = "comfyui"
	SourceTypeNone    = "none"
	// SourceTypeBoth is used when an image has both A1111 and ComfyUI metadata.
	SourceTypeBoth = "a1111,comfyui"

	// OriginIngest is recorded for files the watcher or Sync picks up from
	// disk; also the Ingest() default when no explicit origin is supplied.
	OriginIngest = "ingest"
	// OriginUpload is recorded for web-UI uploads and is the default for
	// multipart API uploads.
	OriginUpload = "upload"
	// OriginExtract is recorded for rows created by extracting a page out
	// of a cbz archive from the reader.
	OriginExtract = "extract"
	// OriginGenerate is recorded for a cbz archive monbooru itself built
	// out of a collection.
	OriginGenerate = "generate"
)

// MediaKinds are the buckets a file type falls into, the vocabulary the
// `type:` search filter takes.
var MediaKinds = []string{"image", "archive", "animated"}

// MediaKind returns the bucket a file type belongs to, or "" for none.
func MediaKind(fileType string) string {
	switch fileType {
	case FileTypeJPEG, FileTypePNG, FileTypeWEBP:
		return "image"
	case FileTypeCBZ:
		return "archive"
	case FileTypeGIF, FileTypeMP4, FileTypeWEBM:
		return "animated"
	}
	return ""
}

type Image struct {
	ID             int64
	SHA256         string
	MD5            string // digest of the same bytes; "" until computed. Distinct from ImageSource.MD5, which is a source's claim
	CanonicalPath  string
	FolderPath     string // relative dir from gallery_path root; "" = root
	FileType       string // "jpeg" | "png" | "webp" | "gif" | "mp4" | "webm" | "cbz"
	Width          *int
	Height         *int
	FileSize       int64
	IsMissing      bool
	IsFavorited    bool
	IsInbox        bool // 1 = needs triage; 0 = archived/curated
	AutoTaggedAt   *time.Time
	SourceType     string   // "a1111" | "comfyui" | "none" | "a1111,comfyui"
	Origin         string   // "ingest" | "upload" | "extract" | caller-supplied string (app name, URL...)
	Source         string   // free-form provenance label (site name, scraper, ...); operator-edited
	URL            string   // canonical web URL the image came from; http(s) only
	Note           string   // operator's freeform note; never set by an import
	OriginalSource string   // legacy image-level original source URL; read-only, no writer left
	PageCount      *int     // page entry count for cbz manga rows; NULL otherwise
	LastReadPage   *int     // 1-based reader resume page for cbz rows; NULL = unstarted or finished. Only the primary-key image load populates it
	DurationSec    *float64 // video duration in seconds; NULL for non-video rows and for videos that pre-date the column or whose probe failed
	Series         string   // operator-edited free-form series label (max 200 chars); '' when unset
	SeriesOrder    *int     // operator-edited position within Series; NULL = unspecified
	Phash          *int64   // 64-bit perceptual hash; NULL until backfilled or for rows without a decodable thumbnail
	IngestedAt     time.Time
	UploadBatch    *int64 // shared token across one web-UI upload POST; NULL for watcher/sync/API rows. Groups a drop into one inbox cluster.
}

// Collection is one membership of an image: a collection label plus an
// optional position within it. An image can carry several.
type Collection struct {
	Name  string
	Order *int // position within Name; nil = unordered
}

// ImageSource is one origin of an image: a site label plus the post it came
// from. An image can carry several; the first (lowest rowid) is the primary
// that images.source / images.url mirror.
type ImageSource struct {
	Site       string
	PostID     string // upstream post id as text; "" for a manually-added origin
	URL        string
	Commentary string  // artist commentary from this source; "" when none
	Original   string  // upstream artist source the post declared (usually a URL, newline-joined when several); "" when none
	Similarity float64 // best similarity-service score a lookup matched this origin with; 0 = exact or manual
	MD5        string  // md5 the source last claimed; "" when it never claimed one
	MD5Match   string  // last claimed-md5 vs local-file verdict: "" unknown, "match", "differ"
	// UpgradeKept is the operator's "keep my file" ruling on this origin.
	// It hides the upgrade offer until the post claims an md5 it has not
	// claimed before.
	UpgradeKept bool
	// What the post says about the file it serves. Zero / "" where the
	// source published nothing, which is most sites for most fields.
	PostWidth, PostHeight int
	PostSize              int64
	PostExt               string
}

// Annotation is one positional note box overlaid on an image, in original-image
// pixel coordinates. Either pulled from a source (the whole set a source
// contributed is replaced on a re-pull) or drawn by the operator (Manual, with
// empty Site/PostID).
type Annotation struct {
	ID     int64
	Site   string
	PostID string
	X      int
	Y      int
	W      int
	H      int
	Body   string
	Manual bool
}

// MangaMetadata mirrors sd_metadata / comfyui_metadata for the manga
// feature: parsed read-only ComicInfo.xml descriptors surfaced on the
// detail page. The authoritative page count lives on Image.PageCount;
// XMLPageCount is whatever the XML declared and is shown for
// information only.
type MangaMetadata struct {
	ImageID         int64
	Title           string
	Series          string
	Number          string
	Volume          string
	Count           *int
	Summary         string
	Notes           string
	Year            *int
	Month           *int
	Day             *int
	Writer          string
	Penciller       string
	Inker           string
	Colorist        string
	Letterer        string
	CoverArtist     string
	Editor          string
	Publisher       string
	Imprint         string
	Genre           string
	Web             string
	LanguageISO     string
	Format          string
	Manga           string // "Yes" | "YesAndRightToLeft" | "No" | "Unknown"
	AgeRating       string
	CommunityRating *float64
	XMLPageCount    *int
	RawXML          string // full XML body verbatim, capped at 64 KiB at parse time
}

type ImagePath struct {
	ID          int64
	ImageID     int64
	Path        string
	IsCanonical bool
}

type Tag struct {
	ID                     int64
	Name                   string
	CategoryID             int64
	CategoryName           string
	CategoryColor          string
	UsageCount             int
	IsAlias                bool
	CanonicalTagID         *int64
	CanonicalName          string // populated on alias rows when ListTags joins the canonical
	CanonicalCategoryName  string
	CanonicalCategoryColor string
	CreatedAt              time.Time
	Origin                 string    // creation provenance label; "" on rows predating the column
	LastUsedAt             time.Time // zero when never applied to an image
	Stale                  bool      // alias rows: the PTR's latest refresh no longer listed this spelling
	StaleUsage             int       // count of this tag's image_tags rows a source dropped (stale=1)
	FoldedInto             string    // corrected spelling on the folded-duplicates view; "" otherwise
}

type TagCategory struct {
	ID        int64
	Name      string
	Color     string
	IsBuiltin bool
}

type ImageTag struct {
	ImageID    int64
	TagID      int64
	TagName    string
	Category   string
	Color      string
	UsageCount int
	IsAuto     bool
	IsImplied  bool // row was fanned out from a parent tag's implication graph
	Confidence *float64
	TaggerName string // source auto-tagger when IsAuto; empty for manual tags
	Stale      bool   // the attributed source's latest fetch no longer carried this tag
	CreatedAt  time.Time
}

// Implication is one edge of the tag implication graph: adding ParentID
// to an image fans out an implied row for ImpliedID.
type Implication struct {
	ParentID             int64
	ImpliedID            int64
	ParentName           string
	ParentCategoryName   string
	ParentCategoryColor  string
	ImpliedName          string
	ImpliedCategoryName  string
	ImpliedCategoryColor string
	CreatedAt            time.Time
	Origin               string // creation provenance label; "" on edges predating the column
	Stale                bool   // the PTR's latest refresh no longer carried the edge
}

// SDParam is a single parsed key-value pair from A1111 generation parameters.
type SDParam struct {
	Key string
	Val string
}

type SDMetadata struct {
	ImageID        int64
	Prompt         string
	NegativePrompt string
	Model          string
	Seed           *int64
	Sampler        string
	Steps          *int
	CFGScale       *float64
	RawParams      string    // full A1111 parameter line for display
	ParsedParams   []SDParam // all key-value pairs parsed from RawParams
	GenerationHash string    // short hex digest over prompt/negative/model/sampler/steps/cfg (seed excluded)
}

type ComfyUIMetadata struct {
	ImageID         int64
	Prompt          string
	ModelCheckpoint string
	Seed            *int64
	Sampler         string
	Steps           *int
	CFGScale        *float64
	RawWorkflow     string
	GenerationHash  string // short hex digest over prompt/model/sampler/steps/cfg (seed excluded)
}

// ComfyNode represents one node from a ComfyUI workflow for structured display.
type ComfyNode struct {
	Key       string
	Title     string
	ClassType string
	Params    []ComfyNodeParam
}

// ComfyNodeParam is a single input parameter on a ComfyUI node.
type ComfyNodeParam struct {
	Name  string
	Value string
	IsRef bool // true if the value is a reference to another node
}

type SavedSearch struct {
	ID        int64
	Name      string
	Query     string
	Sort      string
	Order     string
	Seed      string
	CreatedAt time.Time
}

// HRef builds the `/?...` link a sidebar entry resolves to. Mirrors the
// gallery handler's URL contract so reopening the entry lands the user
// on the same view they saved (q + sort + order + seed).
func (s SavedSearch) HRef() string {
	out := "/?q=" + urlQueryEscape(s.Query)
	if s.Sort != "" {
		out += "&sort=" + urlQueryEscape(s.Sort)
	}
	if s.Order != "" {
		out += "&order=" + urlQueryEscape(s.Order)
	}
	if s.Seed != "" {
		out += "&seed=" + urlQueryEscape(s.Seed)
	}
	return out
}

// Background-job type identifiers. Use these constants instead of bare
// strings at every jobs.Start / StartScheduled call site so a typo
// surfaces at compile time.
const (
	JobTypeSync          = "sync"
	JobTypeAutotag       = "autotag"
	JobTypeReExtract     = "re-extract"
	JobTypeDelete        = "delete"
	JobTypeRebuildThumbs = "rebuild-thumbs"
	JobTypeMove          = "move"
	JobTypeTransfer      = "transfer"
	JobTypeTag           = "tag"
	JobTypeWatcher       = "watcher"
	JobTypePruneThumbs   = "prune-thumbs"
	JobTypeVacuum        = "vacuum"
	JobTypeFreeMemory    = "free-memory"
	JobTypeHashes        = "hashes"
	JobTypeRelations     = "relations"
	JobTypeFold          = "fold"
	JobTypeLookup        = "lookup"
)

type JobState struct {
	Running    bool
	JobType    string // one of the JobType* constants above
	Total      int
	Processed  int
	Message    string
	StartedAt  time.Time
	FinishedAt *time.Time
	Summary    string
	Error      string
	// WatcherNotices is a monotonic counter bumped on every watcher
	// ingest/remove that fires while a job is running. The client uses it
	// as a refresh signal without overwriting the running progress line.
	WatcherNotices int
}

type SearchResult struct {
	Page    int
	Limit   int
	Total   int
	Results []Image
}

// RowScanner is the Scan surface *sql.Row and *sql.Rows share, so one
// scanner serves both the single-row primary-key reads and the search
// cursor.
type RowScanner interface {
	Scan(dest ...any) error
}

// ImageRowColumns is the canonical SELECT list ScanImageRow reads, in
// the order it scans them. Callers alias the images table as `i`. The
// column order is load-bearing: the Scan is positional.
const ImageRowColumns = `i.id, i.sha256, i.md5, i.canonical_path, i.folder_path, i.file_type,
	        i.width, i.height, i.file_size, i.is_missing, i.is_favorited,
	        i.is_inbox, i.auto_tagged_at, i.source_type, i.origin, i.source, i.url, i.note, i.original_source,
	        i.page_count, i.last_read_page, i.duration_seconds, i.series, i.series_order, i.phash, i.ingested_at, i.upload_batch`

// ScanImageRow reads one row in the ImageRowColumns shape and folds the
// int-as-bool flags and RFC3339 timestamps onto the typed struct. The
// single source of truth for the image row shape.
func ScanImageRow(row RowScanner) (Image, error) {
	var img Image
	var isMissing, isFav, isInbox int
	var width, height, pageCount, lastReadPage, seriesOrder *int
	var durationSec *float64
	var autoTaggedAt *string
	var phash *int64
	var ingestedAt string
	if err := row.Scan(
		&img.ID, &img.SHA256, &img.MD5, &img.CanonicalPath, &img.FolderPath, &img.FileType,
		&width, &height, &img.FileSize, &isMissing, &isFav,
		&isInbox, &autoTaggedAt, &img.SourceType, &img.Origin, &img.Source, &img.URL, &img.Note, &img.OriginalSource,
		&pageCount, &lastReadPage, &durationSec, &img.Series, &seriesOrder, &phash, &ingestedAt, &img.UploadBatch,
	); err != nil {
		return Image{}, err
	}
	img.IsMissing = isMissing == 1
	img.IsFavorited = isFav == 1
	img.IsInbox = isInbox == 1
	img.Width = width
	img.Height = height
	img.PageCount = pageCount
	img.LastReadPage = lastReadPage
	img.DurationSec = durationSec
	img.SeriesOrder = seriesOrder
	img.Phash = phash
	if autoTaggedAt != nil {
		t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
		img.AutoTaggedAt = &t
	}
	img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	return img, nil
}
