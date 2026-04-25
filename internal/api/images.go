package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tagger"
)

// imageResponse is the JSON representation of an image.
type imageResponse struct {
	ID            int64          `json:"id"`
	SHA256        string         `json:"sha256"`
	CanonicalPath string         `json:"canonical_path"`
	Aliases       []string       `json:"aliases"`
	FileType      string         `json:"file_type"`
	Width         *int           `json:"width"`
	Height        *int           `json:"height"`
	FileSize      int64          `json:"file_size"`
	IsFavorited   bool           `json:"is_favorited"`
	IsMissing     bool           `json:"is_missing"`
	AutoTaggedAt  *time.Time     `json:"auto_tagged_at"`
	SourceType    string         `json:"source_type"`
	Origin        string         `json:"origin"`
	IngestedAt    time.Time      `json:"ingested_at"`
	ThumbnailURL  string         `json:"thumbnail_url"`
	Tags          []imageTagJSON `json:"tags"`
}

type imageTagJSON struct {
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	IsAuto     bool     `json:"is_auto"`
	Confidence *float64 `json:"confidence"`
	TaggerName *string  `json:"tagger_name"`
}

// buildImageResponse fetches an image plus its tags and assembles the
// JSON response struct.
func (h *Handler) buildImageResponse(g Gallery, imageID int64) (*imageResponse, error) {
	var img models.Image
	var isMissing, isFavorited int
	var autoTaggedAt *string
	var ingestedAt string

	err := g.DB.Read.QueryRow(`
		SELECT id, sha256, canonical_path, file_type, width, height, file_size,
		       is_missing, is_favorited, auto_tagged_at, source_type, origin, ingested_at
		FROM images WHERE id = ?`, imageID,
	).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FileType, &img.Width, &img.Height,
		&img.FileSize, &isMissing, &isFavorited, &autoTaggedAt, &img.SourceType, &img.Origin, &ingestedAt)
	if err != nil {
		return nil, err
	}
	img.IsMissing = isMissing == 1
	img.IsFavorited = isFavorited == 1
	img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	if autoTaggedAt != nil {
		t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
		img.AutoTaggedAt = &t
	}

	// Close the alias rows immediately rather than deferring, so the
	// read connection is freed before the tag query.
	aliases := []string{}
	aliasRows, err := g.DB.Read.Query(`SELECT path FROM image_paths WHERE image_id = ? AND is_canonical = 0`, imageID)
	if err != nil {
		logx.Warnf("buildImageResponse aliases: %v", err)
	} else {
		for aliasRows.Next() {
			var p string
			if err := aliasRows.Scan(&p); err == nil {
				aliases = append(aliases, p)
			}
		}
		aliasRows.Close()
	}

	tags := []imageTagJSON{}
	tagRows, err := g.DB.Read.Query(`
		SELECT t.name, tc.name, it.is_auto, it.confidence, it.tagger_name
		FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE it.image_id = ?
		ORDER BY tc.name, t.name`, imageID)
	if err != nil {
		logx.Warnf("buildImageResponse tags: %v", err)
	} else {
		defer tagRows.Close()
		for tagRows.Next() {
			var tj imageTagJSON
			var tn *string
			if err := tagRows.Scan(&tj.Name, &tj.Category, &tj.IsAuto, &tj.Confidence, &tn); err == nil {
				tj.TaggerName = tn
				tags = append(tags, tj)
			}
		}
	}

	resp := &imageResponse{
		ID:            img.ID,
		SHA256:        img.SHA256,
		CanonicalPath: img.CanonicalPath,
		Aliases:       aliases,
		FileType:      img.FileType,
		Width:         img.Width,
		Height:        img.Height,
		FileSize:      img.FileSize,
		IsFavorited:   img.IsFavorited,
		IsMissing:     img.IsMissing,
		AutoTaggedAt:  img.AutoTaggedAt,
		SourceType:    img.SourceType,
		Origin:        img.Origin,
		IngestedAt:    img.IngestedAt,
		ThumbnailURL:  "/thumbnails/" + g.Name + "/" + strconv.FormatInt(imageID, 10) + ".jpg",
		Tags:          tags,
	}
	return resp, nil
}

func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// createImage handles POST /api/v1/images. Accepts either multipart
// (with `file`, `tags`, `folder`, `autotag`, `tagger_name`, `source`)
// or JSON (with `path`, `tags`, `folder`, `autotag`, `tagger_name`,
// `source`). In JSON mode `folder` only applies to relative paths;
// absolute paths are used verbatim.
func (h *Handler) createImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	ct := r.Header.Get("Content-Type")

	var (
		imgPath        string
		initialTags    []string
		folder         string
		autotag        bool
		taggerName     string
		tagSource      string // caller-supplied source; stored on image.origin and inherited by initial tags
		uploadedToDisk bool   // true when we wrote the file ourselves (multipart)
	)

	if isMultipart(ct) {
		maxBytes := int64(h.cfg.Gallery.MaxFileSizeMB) * 1024 * 1024
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+4096)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			apiError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds max size")
			return
		}
		file, fh, err := r.FormFile("file")
		if err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "missing file field")
			return
		}
		defer file.Close()

		folder = strings.TrimSpace(r.FormValue("folder"))
		autotag = isTrue(r.FormValue("autotag"))
		taggerName = strings.TrimSpace(r.FormValue("tagger_name"))
		tagSource = strings.TrimSpace(r.FormValue("source"))

		destDir, destErr := gallery.ResolveSubdir(g.GalleryPath, folder)
		if destErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
			return
		}
		if err := os.MkdirAll(destDir, 0755); err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to create folder: "+err.Error())
			return
		}

		// Write directly to the final destination so the watcher sees
		// the real filename rather than a temp one (which would get
		// marked missing as soon as we renamed it).
		dstPath := gallery.UniqueDestPath(destDir, fh.Filename)
		dst, err := os.Create(dstPath)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to create destination file")
			return
		}
		if _, err := io.Copy(dst, file); err != nil {
			dst.Close()
			os.Remove(dstPath)
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to save upload")
			return
		}
		dst.Close()

		if tagsJSON := r.FormValue("tags"); tagsJSON != "" {
			json.Unmarshal([]byte(tagsJSON), &initialTags)
		}
		imgPath = dstPath
		uploadedToDisk = true
	} else {
		var body struct {
			Path       string   `json:"path"`
			Tags       []string `json:"tags"`
			Folder     string   `json:"folder"`
			Autotag    bool     `json:"autotag"`
			TaggerName string   `json:"tagger_name"`
			Source     string   `json:"source"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}
		if body.Path == "" {
			apiError(w, http.StatusBadRequest, "invalid_request", "path is required")
			return
		}
		imgPath = body.Path
		initialTags = body.Tags
		folder = strings.TrimSpace(body.Folder)
		autotag = body.Autotag
		taggerName = strings.TrimSpace(body.TaggerName)
		tagSource = strings.TrimSpace(body.Source)

		// Relative path + folder: resolve under <gallery>/<folder>/<path>.
		// Absolute paths go through unchanged.
		if folder != "" && !filepath.IsAbs(imgPath) {
			destDir, destErr := gallery.ResolveSubdir(g.GalleryPath, folder)
			if destErr != nil {
				apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
				return
			}
			imgPath = filepath.Join(destDir, imgPath)
		}
	}

	// Enforce gallery.max_file_size_mb for both modes. Multipart also
	// has MaxBytesReader; this mainly guards the JSON path-reference
	// mode where the caller supplies an absolute path.
	if maxMB := h.cfg.Gallery.MaxFileSizeMB; maxMB > 0 {
		if info, err := os.Stat(imgPath); err == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				if uploadedToDisk {
					os.Remove(imgPath)
				}
				apiError(w, http.StatusRequestEntityTooLarge, "file_too_large",
					fmt.Sprintf("file exceeds max size (%d MB)", maxMB))
				return
			}
		}
	}

	fileType, ftErr := gallery.DetectFileType(imgPath)
	if ftErr != nil {
		if uploadedToDisk {
			os.Remove(imgPath)
		}
		apiError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}

	// Caller-supplied source wins; otherwise multipart defaults to
	// "upload" and JSON path-reference defaults to "ingest".
	origin := tagSource
	if origin == "" {
		if uploadedToDisk {
			origin = models.OriginUpload
		} else {
			origin = models.OriginIngest
		}
	}

	img, isDuplicate, err := gallery.Ingest(g.DB, g.GalleryPath, g.ThumbnailsPath, imgPath, fileType, origin)
	if err != nil {
		if uploadedToDisk {
			os.Remove(imgPath)
		}
		logx.Warnf("api createImage ingest: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if isDuplicate {
		// Ingest already recorded the new file as an alias path; leave
		// it on disk so the alias remains valid.
		apiError(w, http.StatusConflict, "conflict", "image with this SHA-256 already exists")
		return
	}
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}

	// Each initial tag is either a plain name (general category) or
	// "category:name". Failures are collected as warnings rather than
	// aborting the whole request.
	var tagWarnings []string
	for _, tagName := range initialTags {
		catID, bareName, err := h.resolveCategoryTag(g, tagName)
		if err != nil {
			tagWarnings = append(tagWarnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tag, err := g.TagSvc.GetOrCreateTag(bareName, catID)
		if err != nil {
			tagWarnings = append(tagWarnings, "tag "+tagName+": "+err.Error())
			continue
		}
		if err := g.TagSvc.AddTagToImageFromTagger(img.ID, tag.ID, false, nil, tagSource); err != nil {
			tagWarnings = append(tagWarnings, "tag "+tagName+": "+err.Error())
		}
	}

	var autotagNote string
	if autotag {
		if !tagger.IsAvailable(h.cfg) {
			autotagNote = "autotag skipped: tagger not available"
		} else {
			selected, selErr := h.selectedTaggers(taggerName)
			if selErr != nil {
				autotagNote = "autotag skipped: " + selErr.Error()
			} else if err := h.jobs.Start("autotag"); err != nil {
				autotagNote = "autotag skipped: a job is already running"
			} else {
				imgID := img.ID
				database := g.DB
				go func() {
					if err := tagger.RunWithTaggers(h.jobs.Context(), database, h.cfg, []int64{imgID}, selected, h.jobs, h.cfg.Tagger.UseCUDA); err != nil {
						h.jobs.Fail(err.Error())
						return
					}
					h.jobs.Complete(fmt.Sprintf("auto-tagged image #%d", imgID))
				}()
				autotagNote = "autotag job started"
			}
		}
	}

	resp, err := h.buildImageResponse(g, img.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
		return
	}

	// Wrap the response when we have side-channel info to attach.
	if len(tagWarnings) > 0 || autotagNote != "" {
		envelope := map[string]any{"image": resp}
		if len(tagWarnings) > 0 {
			envelope["tag_warnings"] = tagWarnings
		}
		if autotagNote != "" {
			envelope["autotag"] = autotagNote
		}
		writeJSON(w, http.StatusCreated, envelope)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// selectedTaggers resolves a caller-supplied tagger_name to a concrete
// list of taggers; empty name means every enabled+available tagger.
func (h *Handler) selectedTaggers(name string) ([]tagger.TaggerStatus, error) {
	enabled := tagger.EnabledTaggers(h.cfg)
	if name == "" {
		return enabled, nil
	}
	for _, t := range enabled {
		if t.Name == name {
			return []tagger.TaggerStatus{t}, nil
		}
	}
	return nil, fmt.Errorf("tagger %q is not enabled or available", name)
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (h *Handler) deleteImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}

	result, err := gallery.DeleteImage(g.DB, g.ThumbnailsPath, id, g.TagSvc.RemoveAllTagsFromImage)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}

	if r.URL.Query().Get("delete_empty_folder") == "true" && !result.IsMissing && result.FolderPath != "" {
		fullFolderPath := filepath.Join(g.GalleryPath, result.FolderPath)
		entries, readErr := os.ReadDir(fullFolderPath)
		if readErr == nil && len(entries) == 0 {
			if removeErr := os.Remove(fullFolderPath); removeErr != nil {
				logx.Warnf("api deleteImage: failed to remove empty folder %q: %v", fullFolderPath, removeErr)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"folder_deleted": true,
				"folder":         result.FolderPath,
			})
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) searchImages(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	queryStr := q.Get("q")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "newest"
	}
	orderStr := q.Get("order")
	if orderStr == "" {
		orderStr = "desc"
	}

	offset, limit := parsePage(r, h.cfg.UI.PageSize, 200)
	pageNum := offset/limit + 1

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		logx.Warnf("searchImages parse: %v", parseErr)
	}
	sq := search.Query{
		Expr:  expr,
		Sort:  sortStr,
		Order: orderStr,
		Page:  pageNum,
		Limit: limit,
	}

	result, err := search.Execute(g.DB, sq)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	images := make([]imageResponse, 0, len(result.Results))
	for _, img := range result.Results {
		images = append(images, imageResponse{
			ID:            img.ID,
			SHA256:        img.SHA256,
			CanonicalPath: img.CanonicalPath,
			Aliases:       []string{},
			FileType:      img.FileType,
			Width:         img.Width,
			Height:        img.Height,
			FileSize:      img.FileSize,
			IsFavorited:   img.IsFavorited,
			IsMissing:     img.IsMissing,
			AutoTaggedAt:  img.AutoTaggedAt,
			SourceType:    img.SourceType,
			Origin:        img.Origin,
			IngestedAt:    img.IngestedAt,
			ThumbnailURL:  "/thumbnails/" + g.Name + "/" + strconv.FormatInt(img.ID, 10) + ".jpg",
			Tags:          []imageTagJSON{},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"page":    result.Page,
		"limit":   result.Limit,
		"total":   result.Total,
		"results": images,
	})
}

// addImageTags handles POST /api/v1/images/:id/tags. Each entry can
// be a plain name (general category) or "category:name", matching the
// web UI's tag input.
func (h *Handler) addImageTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}

	var body struct {
		Tags   []string `json:"tags"`
		Source string   `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	source := strings.TrimSpace(body.Source)

	for _, tagName := range body.Tags {
		catID, bareName, err := h.resolveCategoryTag(g, tagName)
		if err != nil {
			logx.Warnf("api addImageTags resolve %q: %v", tagName, err)
			continue
		}
		tag, err := g.TagSvc.GetOrCreateTag(bareName, catID)
		if err != nil {
			logx.Warnf("api addImageTags GetOrCreate: %v", err)
			continue
		}
		if err := g.TagSvc.AddTagToImageFromTagger(id, tag.ID, false, nil, source); err != nil {
			logx.Warnf("api addImageTags AddTag: %v", err)
		}
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp.Tags)
}

// resolveCategoryTag splits "artist:foo" into (artist_id, "foo") when
// "artist" names a real category, otherwise returns (general_id, input)
// so colon-bearing tag names like "nier:automata" or ":3" round-trip
// without a warning.
func (h *Handler) resolveCategoryTag(g Gallery, input string) (int64, string, error) {
	input = strings.TrimSpace(input)
	catName := "general"
	tagName := input
	if idx := strings.Index(input, ":"); idx > 0 {
		var catID int64
		if err := g.DB.Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, input[:idx],
		).Scan(&catID); err == nil {
			return catID, input[idx+1:], nil
		}
	}
	var catID int64
	if err := g.DB.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = ?`, catName,
	).Scan(&catID); err != nil {
		return 0, "", fmt.Errorf("unknown category %q", catName)
	}
	return catID, tagName, nil
}

// removeImageTags handles DELETE /api/v1/images/:id/tags. Each entry
// is plain (any single match) or "category:name" (exact category). A
// plain name matching more than one category on the image returns 409
// so the caller can disambiguate.
func (h *Handler) removeImageTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	for _, tagName := range body.Tags {
		tagID, err := h.resolveImageTagID(g, id, tagName)
		if err != nil {
			apiError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if tagID == 0 {
			// Tag not on this image; silently ignored per the docs.
			continue
		}
		g.TagSvc.RemoveTagFromImage(id, tagID)
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp.Tags)
}

// resolveImageTagID returns the tag_id attached to imageID that matches
// tagName. A "category:name" input targets that exact category when
// the prefix is a real category; otherwise the whole string is matched
// as a literal tag name. A real-category prefix that misses on the
// image falls through to the literal-name branch so an oddly-stored
// general tag like "artist:foo" is still removable. A plain name is
// accepted only when it resolves to exactly one tag on the image.
// (0, nil) means the tag isn't present.
func (h *Handler) resolveImageTagID(g Gallery, imageID int64, tagName string) (int64, error) {
	tagName = strings.TrimSpace(tagName)
	if idx := strings.Index(tagName, ":"); idx > 0 {
		catName := tagName[:idx]
		var catID int64
		if g.DB.Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, catName,
		).Scan(&catID) == nil {
			bareName := tagName[idx+1:]
			var tagID int64
			if err := g.DB.Read.QueryRow(
				`SELECT t.id FROM image_tags it
				 JOIN tags t             ON t.id  = it.tag_id
				 JOIN tag_categories tc  ON tc.id = t.category_id
				 WHERE it.image_id = ? AND t.name = ? AND tc.name = ?`,
				imageID, bareName, catName,
			).Scan(&tagID); err == nil {
				return tagID, nil
			}
			// Category-qualified miss: fall through.
		}
	}

	rows, err := g.DB.Read.Query(
		`SELECT t.id FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = ?`,
		imageID, tagName,
	)
	if err != nil {
		return 0, fmt.Errorf("tag lookup failed: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 0:
		return 0, nil
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("tag %q exists on this image in multiple categories; use category:name", tagName)
	}
}

func isMultipart(ct string) bool {
	return strings.HasPrefix(ct, "multipart/form-data")
}
