package models

import "time"

const (
	FileTypeJPEG = "jpeg"
	FileTypePNG  = "png"
	FileTypeWEBP = "webp"
	FileTypeGIF  = "gif"
	FileTypeMP4  = "mp4"
	FileTypeWEBM = "webm"

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
)

type Image struct {
	ID            int64
	SHA256        string
	CanonicalPath string
	FolderPath    string // relative dir from gallery_path root; "" = root
	FileType      string // "jpeg" | "png" | "webp" | "gif" | "mp4" | "webm"
	Width         *int
	Height        *int
	FileSize      int64
	IsMissing     bool
	IsFavorited   bool
	AutoTaggedAt  *time.Time
	SourceType    string // "a1111" | "comfyui" | "none" | "a1111,comfyui"
	Origin        string // "ingest" | "upload" | caller-supplied string (app name, URL…)
	IngestedAt    time.Time
}

type ImagePath struct {
	ID          int64
	ImageID     int64
	Path        string
	IsCanonical bool
}

type Tag struct {
	ID                    int64
	Name                  string
	CategoryID            int64
	CategoryName          string
	CategoryColor         string
	UsageCount            int
	IsAlias               bool
	IsAutoOnly            bool // true if all usages of this tag are auto-tagged (no manual usage)
	CanonicalTagID        *int64
	CanonicalName         string // populated on alias rows when ListTags joins the canonical
	CanonicalCategoryName string
	CanonicalCategoryColor string
	CreatedAt             time.Time
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
	Confidence *float64
	TaggerName string // source auto-tagger when IsAuto; empty for manual tags
	CreatedAt  time.Time
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
	GenerationHash string    // short hex digest over prompt/model/sampler/steps/cfg (seed excluded)
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
	CreatedAt time.Time
}

type JobState struct {
	Running    bool
	JobType    string // "sync" | "autotag"
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

