package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
)

// serveImageFile handles GET /api/v1/images/{id}/file: the original
// bytes. These routes exist so a Bearer-token client can reach the
// media the image object references - the web /thumbnails and
// /images/{id}/file routes sit behind the session middleware and
// redirect a token-only caller to /login. Path containment mirrors the
// web handler so a canonical_path that somehow escapes the gallery root
// is refused.
func (h *Handler) serveImageFile(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	canonPath, fileType, ok := containedCanonical(w, g, id)
	if !ok {
		return
	}
	if ct := gallery.MIMEForFileType(fileType); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition", gallery.ContentDispositionFor(canonPath))
	http.ServeFile(w, r, canonPath)
}

// containedCanonical resolves the row's canonical_path and file_type,
// answering 404 when the row is missing or its path escapes the gallery
// root.
func containedCanonical(w http.ResponseWriter, g Gallery, id int64) (canonPath, fileType string, ok bool) {
	if err := g.DB.Read.QueryRow(
		`SELECT canonical_path, file_type FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &fileType); err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return "", "", false
	}
	if !gallery.ResolvedInside(g.GalleryPath, canonPath) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return "", "", false
	}
	return canonPath, fileType, true
}

// serveThumbnail handles GET /api/v1/images/{id}/thumbnail: the static
// {id}.jpg preview generated at ingest. Returns 404 when the row or the
// thumbnail file is absent (e.g. a video ingested before ffmpeg was
// available, awaiting a rebuild-thumbnails pass).
func (h *Handler) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	thumbPath := filepath.Join(g.ThumbnailsPath, strconv.FormatInt(id, 10)+".jpg")
	if _, err := os.Stat(thumbPath); err != nil {
		apiError(w, http.StatusNotFound, "not_found", "thumbnail not found")
		return
	}
	http.ServeFile(w, r, thumbPath)
}

func (h *Handler) serveMangaPage(w http.ResponseWriter, r *http.Request) {
	h.serveMangaPagePath(w, r, gallery.EnsureMangaPage)
}

func (h *Handler) serveMangaPageThumb(w http.ResponseWriter, r *http.Request) {
	h.serveMangaPagePath(w, r, gallery.EnsureMangaPageThumb)
}

// serveMangaPagePath is the shared body behind the per-page byte and
// thumbnail routes. ensure lazily extracts the page from the archive on
// a cache miss, identical to the web reader's path. Non-cbz rows 404.
func (h *Handler) serveMangaPagePath(
	w http.ResponseWriter, r *http.Request,
	ensure func(thumbnailsPath, canonPath string, imageID int64, n int) (string, error),
) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid page number")
		return
	}
	canonPath, fileType, ok := containedCanonical(w, g, id)
	if !ok {
		return
	}
	if fileType != models.FileTypeCBZ {
		apiError(w, http.StatusNotFound, "not_found", "image is not a manga archive")
		return
	}
	page, err := ensure(g.ThumbnailsPath, canonPath, id, n)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "page not found")
		return
	}
	http.ServeFile(w, r, page)
}
