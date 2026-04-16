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
	ID           int64          `json:"id"`
	SHA256       string         `json:"sha256"`
	CanonicalPath string         `json:"canonical_path"`
	Aliases      []string       `json:"aliases"`
	FileType     string         `json:"file_type"`
	Width        *int           `json:"width"`
	Height       *int           `json:"height"`
	FileSize     int64          `json:"file_size"`
	IsFavorited  bool           `json:"is_favorited"`
	IsMissing    bool           `json:"is_missing"`
	AutoTaggedAt *time.Time     `json:"auto_tagged_at"`
	SourceType   string         `json:"source_type"`
	IngestedAt   time.Time      `json:"ingested_at"`
	ThumbnailURL string         `json:"thumbnail_url"`
	Tags         []imageTagJSON `json:"tags"`
}

type imageTagJSON struct {
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	IsAuto     bool     `json:"is_auto"`
	Confidence *float64 `json:"confidence"`
	TaggerName *string  `json:"tagger_name"`
}

// buildImageResponse fetches an image and its tags and returns the response struct.
func (h *Handler) buildImageResponse(imageID int64) (*imageResponse, error) {
	var img models.Image
	var isMissing, isFavorited int
	var autoTaggedAt *string
	var ingestedAt string

	err := h.db.Read.QueryRow(`
		SELECT id, sha256, canonical_path, file_type, width, height, file_size,
		       is_missing, is_favorited, auto_tagged_at, source_type, ingested_at
		FROM images WHERE id = ?`, imageID,
	).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FileType, &img.Width, &img.Height,
		&img.FileSize, &isMissing, &isFavorited, &autoTaggedAt, &img.SourceType, &ingestedAt)
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

	// Fetch aliases. Close rows immediately after iterating instead of deferring,
	// so the read connection is released before the subsequent tag query.
	aliases := []string{}
	aliasRows, err := h.db.Read.Query(`SELECT path FROM image_paths WHERE image_id = ? AND is_canonical = 0`, imageID)
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
	tagRows, err := h.db.Read.Query(`
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
		IngestedAt:    img.IngestedAt,
		ThumbnailURL:  "/thumbnails/" + strconv.FormatInt(imageID, 10) + ".jpg",
		Tags:          tags,
	}
	return resp, nil
}

// getImage returns metadata for a single image.
func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}

	resp, err := h.buildImageResponse(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// createImage handles POST /api/v1/images.
//
// Multipart form fields:
//   - file          (required): the uploaded file
//   - tags          (optional): JSON-encoded array of tag names
//   - folder        (optional): destination subfolder under gallery_path;
//                               missing parents are created
//   - autotag       (optional): "true"/"1" kicks off an auto-tag job on
//                               the newly-ingested image
//   - tagger_name   (optional): when set with autotag, restricts the job
//                               to that single enabled auto-tagger
//
// JSON mode accepts the same folder/autotag/tagger_name fields alongside
// the existing path/tags fields. In JSON mode the folder only takes
// effect for relative paths (absolute paths are used verbatim).
func (h *Handler) createImage(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")

	var (
		imgPath        string
		initialTags    []string
		folder         string
		autotag        bool
		taggerName     string
		uploadedToDisk bool // true when we wrote the file ourselves (multipart)
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

		destDir, destErr := gallery.ResolveSubdir(h.cfg.Paths.GalleryPath, folder)
		if destErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
			return
		}
		if err := os.MkdirAll(destDir, 0755); err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to create folder: "+err.Error())
			return
		}

		// Write the upload to its final destination using the original
		// filename. A temp filename would be picked up by the watcher and
		// then marked missing as soon as the request handler removed it.
		// Auto-suffix on collision (mirrors the web upload flow).
		dstPath := filepath.Join(destDir, fh.Filename)
		if _, err := os.Stat(dstPath); err == nil {
			base := strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename))
			ext := filepath.Ext(fh.Filename)
			for i := 1; ; i++ {
				dstPath = filepath.Join(destDir, fmt.Sprintf("%s_%d%s", base, i, ext))
				if _, err := os.Stat(dstPath); os.IsNotExist(err) {
					break
				}
			}
		}

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

		// When the caller supplies a relative path plus a folder, resolve the
		// file under <gallery>/<folder>/<path>. Absolute paths are used as-is.
		if folder != "" && !filepath.IsAbs(imgPath) {
			destDir, destErr := gallery.ResolveSubdir(h.cfg.Paths.GalleryPath, folder)
			if destErr != nil {
				apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
				return
			}
			imgPath = filepath.Join(destDir, imgPath)
		}
	}

	// Enforce gallery.max_file_size_mb for both upload modes. The multipart
	// path also passes through MaxBytesReader, so this mainly guards the
	// JSON path-reference mode where the caller supplies an absolute path.
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

	// Determine file type via extension + magic-byte detection.
	fileType, ftErr := gallery.DetectFileType(imgPath)
	if ftErr != nil {
		if uploadedToDisk {
			os.Remove(imgPath)
		}
		apiError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}

	img, isDuplicate, err := gallery.Ingest(h.db, h.cfg, imgPath, fileType)
	if err != nil {
		if uploadedToDisk {
			os.Remove(imgPath)
		}
		logx.Warnf("api createImage ingest: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if isDuplicate {
		// Ingest already recorded the new file as an alias path; leave it on
		// disk so the alias remains valid.
		apiError(w, http.StatusConflict, "conflict", "image with this SHA-256 already exists")
		return
	}

	// Apply initial tags, collecting any warnings. Each tag may be a plain
	// name (added to the general category) or `category:name` which adds it
	// to the named category, matching the web UI's parser.
	var tagWarnings []string
	for _, tagName := range initialTags {
		catID, bareName, err := h.resolveCategoryTag(tagName)
		if err != nil {
			tagWarnings = append(tagWarnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tag, err := h.tagSvc.GetOrCreateTag(bareName, catID)
		if err != nil {
			tagWarnings = append(tagWarnings, "tag "+tagName+": "+err.Error())
			continue
		}
		if err := h.tagSvc.AddTagToImage(img.ID, tag.ID, false, nil); err != nil {
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
				go func() {
					if err := tagger.RunWithTaggers(h.jobs.Context(), h.db, h.cfg, []int64{imgID}, selected, h.jobs, h.cfg.Tagger.UseCUDA); err != nil {
						h.jobs.Fail(err.Error())
						return
					}
					h.jobs.Complete(fmt.Sprintf("auto-tagged image #%d", imgID))
				}()
				autotagNote = "autotag job started"
			}
		}
	}

	resp, err := h.buildImageResponse(img.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
		return
	}

	// Wrap the response when there is any side-channel info to report.
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

// selectedTaggers resolves a caller-supplied tagger_name to the concrete
// list of enabled+available taggers to run; empty name means all of them.
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

// deleteImage handles DELETE /api/v1/images/:id.
func (h *Handler) deleteImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}

	result, err := gallery.DeleteImage(h.db, h.cfg, id, h.tagSvc.RemoveAllTagsFromImage)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	if r.URL.Query().Get("delete_empty_folder") == "true" && !result.IsMissing && result.FolderPath != "" {
		fullFolderPath := filepath.Join(h.cfg.Paths.GalleryPath, result.FolderPath)
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

// searchImages handles GET /api/v1/images/search.
func (h *Handler) searchImages(w http.ResponseWriter, r *http.Request) {
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

	result, err := search.Execute(h.db, sq)
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
			IngestedAt:    img.IngestedAt,
			ThumbnailURL:  "/thumbnails/" + strconv.FormatInt(img.ID, 10) + ".jpg",
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

// addImageTags handles POST /api/v1/images/:id/tags.
//
// Each tag in the body may be a plain name (added to the general category)
// or `category:name` which targets the named category — mirroring the web
// UI's tag input so API callers can add artist/character/etc. tags without
// pre-creating them from the UI.
func (h *Handler) addImageTags(w http.ResponseWriter, r *http.Request) {
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
		catID, bareName, err := h.resolveCategoryTag(tagName)
		if err != nil {
			logx.Warnf("api addImageTags resolve %q: %v", tagName, err)
			continue
		}
		tag, err := h.tagSvc.GetOrCreateTag(bareName, catID)
		if err != nil {
			logx.Warnf("api addImageTags GetOrCreate: %v", err)
			continue
		}
		if err := h.tagSvc.AddTagToImage(id, tag.ID, false, nil); err != nil {
			logx.Warnf("api addImageTags AddTag: %v", err)
		}
	}

	resp, err := h.buildImageResponse(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp.Tags)
}

// resolveCategoryTag splits an input like "artist:foo" into (artist_id, "foo"),
// or returns (general_id, "tagname") for a bare name. Unknown category names
// are reported as an error so the caller can surface a tag_warnings entry.
func (h *Handler) resolveCategoryTag(input string) (int64, string, error) {
	input = strings.TrimSpace(input)
	catName := "general"
	tagName := input
	if idx := strings.Index(input, ":"); idx > 0 {
		catName = input[:idx]
		tagName = input[idx+1:]
	}
	var catID int64
	if err := h.db.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = ?`, catName,
	).Scan(&catID); err != nil {
		return 0, "", fmt.Errorf("unknown category %q", catName)
	}
	return catID, tagName, nil
}

// removeImageTags handles DELETE /api/v1/images/:id/tags.
//
// Each tag name may be plain (matches the general category) or `category:name`
// (matches that specific category). A plain name that exists on the image in
// more than one category is ambiguous and returns a 409 so the caller can
// disambiguate — otherwise the handler would silently remove an arbitrary row.
func (h *Handler) removeImageTags(w http.ResponseWriter, r *http.Request) {
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
		tagID, err := h.resolveImageTagID(id, tagName)
		if err != nil {
			apiError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if tagID == 0 {
			continue // tag not on this image; silently ignored (matches docs)
		}
		h.tagSvc.RemoveTagFromImage(id, tagID)
	}

	resp, err := h.buildImageResponse(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp.Tags)
}

// resolveImageTagID finds the single tag_id attached to imageID that matches
// tagName. A `category:name` input targets that category exactly; a plain
// name is only accepted when it resolves to exactly one tag on the image.
// Returns (0, nil) when the tag is not present on the image.
func (h *Handler) resolveImageTagID(imageID int64, tagName string) (int64, error) {
	tagName = strings.TrimSpace(tagName)
	if idx := strings.Index(tagName, ":"); idx > 0 {
		catName := tagName[:idx]
		bareName := tagName[idx+1:]
		var tagID int64
		err := h.db.Read.QueryRow(
			`SELECT t.id FROM image_tags it
			 JOIN tags t             ON t.id  = it.tag_id
			 JOIN tag_categories tc  ON tc.id = t.category_id
			 WHERE it.image_id = ? AND t.name = ? AND tc.name = ?`,
			imageID, bareName, catName,
		).Scan(&tagID)
		if err != nil {
			return 0, nil
		}
		return tagID, nil
	}

	rows, err := h.db.Read.Query(
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
