// Package api implements the /api/v1/ REST handlers for monbooru.
package api

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tags"
)

// Gallery is what API handlers need to act on a single gallery.
// InvalidateCaches is called after every image add/delete; may be nil.
type Gallery struct {
	Name             string
	GalleryPath      string
	ThumbnailsPath   string
	DB               *db.DB
	TagSvc           *tags.Service
	RelationsSvc     *relations.Service
	InvalidateCaches func()
	// RecordFetch reports a source metadata-fetch outcome for an image so the
	// detail page's poll can reflect it. state is "ok" on a successful enrich,
	// "pending" while in flight, or a terminal failure code (a hash "mismatch",
	// an enrich "error", or a monloader queue code like "unsupported_url" for a
	// fetch that failed before it could enrich). May be nil (the test harness
	// wires no web layer).
	RecordFetch func(imageID int64, state, message string)
	// VisibleCount / TagCount return the cached non-missing-image and
	// non-alias-tag counts (the same values the Settings page shows). May
	// be nil; listGalleries falls back to a direct query then.
	VisibleCount func() (int, error)
	TagCount     func() (int, error)
}

// invalidate runs the gallery's cache-invalidation hook when one is
// wired (it is nil in the test harness).
func (g Gallery) invalidate() {
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}
}

// recordFetch reports a source-fetch outcome to the web layer when one is
// wired (nil in the test harness).
func (g Gallery) recordFetch(imageID int64, state, message string) {
	if g.RecordFetch != nil {
		g.RecordFetch(imageID, state, message)
	}
}

// relationsOnDelete returns the OnImageDeleteTx callback for the given
// service, or nil when the gallery has no relations service wired (the
// test harness). gallery.DeleteImage treats a nil callback as a no-op.
func relationsOnDelete(svc *relations.Service) func(*sql.Tx, int64) error {
	if svc == nil {
		return nil
	}
	return svc.OnImageDeleteTx
}

// ResolverFunc resolves a gallery by name. Empty name = active gallery.
type ResolverFunc func(name string) (Gallery, bool)

// Handler is the root handler for all /api/v1/ routes.
type Handler struct {
	cfg      *config.Config
	cfgMu    *sync.RWMutex // guards cfg.Auth.Tokens, mutated at runtime by the web layer
	jobs     *jobs.Manager
	resolver ResolverFunc
	version  string
}

// New creates a new API handler. version is surfaced on the /api/v1/ root so
// clients (e.g. monloader) can read the server version without scraping HTML.
// cfgMu is the web layer's config lock; token reads take it in shared mode.
func New(cfg *config.Config, cfgMu *sync.RWMutex, jobManager *jobs.Manager, resolver ResolverFunc, version string) *Handler {
	return &Handler{cfg: cfg, cfgMu: cfgMu, jobs: jobManager, resolver: resolver, version: version}
}

// uploadDestination reads the two settings a received file is filed by.
// They are strings the settings page rewrites at runtime, so the read
// takes the web layer's lock.
func (h *Handler) uploadDestination() (folder, name string) {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.cfg.Gallery.DefaultUploadFolder, h.cfg.Gallery.DefaultUploadName
}

// resolveGallery picks the target gallery from ?gallery=... (preferred)
// or the X-Monbooru-Gallery header; empty falls back to the active one.
func (h *Handler) resolveGallery(w http.ResponseWriter, r *http.Request) (Gallery, bool) {
	name := strings.TrimSpace(r.URL.Query().Get("gallery"))
	name = cmp.Or(name, strings.TrimSpace(r.Header.Get("X-Monbooru-Gallery")))
	g, ok := h.resolver(name)
	if !ok {
		if name == "" {
			apiError(w, http.StatusServiceUnavailable, "api_disabled", "no active gallery")
		} else {
			apiError(w, http.StatusBadRequest, "invalid_gallery", "unknown gallery: "+name)
		}
		return Gallery{}, false
	}
	return g, true
}

// decodeJSON decodes the request body into dst, answering the shared
// invalid-JSON 400 on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return false
	}
	return true
}

// galleryAndID runs the shared gallery-then-{id} preamble of the id-bearing
// handlers, keeping the gallery-selector error first.
func (h *Handler) galleryAndID(w http.ResponseWriter, r *http.Request) (Gallery, int64, bool) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return Gallery{}, 0, false
	}
	id, ok := apiPathInt64(w, r, "id")
	if !ok {
		return Gallery{}, 0, false
	}
	return g, id, true
}

// Mount registers every API route on mux under /api/v1/.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/images", h.auth(h.createImage))
	mux.HandleFunc("GET /api/v1/images/search", h.auth(h.searchImages))
	mux.HandleFunc("GET /api/v1/images/{id}", h.auth(h.getImage))
	mux.HandleFunc("PATCH /api/v1/images/{id}", h.auth(h.patchImage))
	mux.HandleFunc("DELETE /api/v1/images/{id}", h.auth(h.deleteImage))
	mux.HandleFunc("GET /api/v1/images/{id}/tags", h.auth(h.listImageTags))
	mux.HandleFunc("POST /api/v1/images/{id}/tags", h.auth(h.addImageTags))
	mux.HandleFunc("DELETE /api/v1/images/{id}/tags", h.auth(h.removeImageTags))
	mux.HandleFunc("POST /api/v1/images/{id}/enrich", h.auth(h.enrichImage))
	mux.HandleFunc("POST /api/v1/images/{id}/fetch-status", h.auth(h.fetchStatusReport))

	mux.HandleFunc("GET /api/v1/images/{id}/file", h.auth(h.serveImageFile))
	mux.HandleFunc("POST /api/v1/images/{id}/file", h.auth(h.replaceImageFile))
	mux.HandleFunc("GET /api/v1/images/{id}/thumbnail", h.auth(h.serveThumbnail))
	mux.HandleFunc("GET /api/v1/images/{id}/page/{n}", h.auth(h.serveMangaPage))
	mux.HandleFunc("GET /api/v1/images/{id}/page/{n}/thumb", h.auth(h.serveMangaPageThumb))

	mux.HandleFunc("GET /api/v1/tags", h.auth(h.listTags))
	mux.HandleFunc("POST /api/v1/tags", h.auth(h.createTag))
	mux.HandleFunc("POST /api/v1/tags/aliases", h.auth(h.createAlias))
	mux.HandleFunc("POST /api/v1/tags/merge", h.auth(h.mergeTags))
	mux.HandleFunc("PATCH /api/v1/tags/{id}", h.auth(h.patchTag))
	mux.HandleFunc("DELETE /api/v1/tags/{id}", h.auth(h.deleteTag))
	mux.HandleFunc("GET /api/v1/tags/{id}/implications", h.auth(h.listImplications))
	mux.HandleFunc("POST /api/v1/tags/{id}/implications", h.auth(h.addImplication))
	mux.HandleFunc("DELETE /api/v1/tags/{id}/implications/{impliedID}", h.auth(h.removeImplication))

	mux.HandleFunc("GET /api/v1/categories", h.auth(h.listCategories))
	mux.HandleFunc("POST /api/v1/categories", h.auth(h.createCategory))
	mux.HandleFunc("PATCH /api/v1/categories/{id}", h.auth(h.patchCategory))
	mux.HandleFunc("DELETE /api/v1/categories/{id}", h.auth(h.deleteCategory))

	mux.HandleFunc("GET /api/v1/galleries", h.auth(h.listGalleries))

	mux.HandleFunc("GET /api/v1/images/{id}/relations", h.auth(h.relationsForImage))
	mux.HandleFunc("POST /api/v1/relations", h.auth(h.addRelation))
	mux.HandleFunc("DELETE /api/v1/relations", h.auth(h.removeRelation))

	mux.HandleFunc("GET /api/v1/openapi.json", h.openAPIJSON)
	mux.HandleFunc("GET /api/v1/docs", h.openAPIDocs)

	mux.HandleFunc("GET /api/v1/", h.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/" {
			apiError(w, http.StatusNotFound, "not_found", "endpoint not found: "+r.URL.Path)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"api":     "monbooru",
			"version": h.version,
			"docs":    "/api/v1/docs",
			"openapi": "/api/v1/openapi.json",
		})
	}))
}

// auth wraps a handler with bearer-token authentication, per-token scope
// enforcement, and the configured-base-URL CORS check.
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Browsers always send Origin without a trailing slash; an
		// operator's base_url written as "http://host/" would otherwise
		// reject every CORS request with no obvious diagnostic.
		baseURL := strings.TrimRight(h.cfg.Server.BaseURL, "/")
		if origin != "" && baseURL != "" {
			if origin != baseURL {
				apiError(w, http.StatusForbidden, "forbidden", "CORS: origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", baseURL)
		}

		h.cfgMu.RLock()
		if len(h.cfg.Auth.Tokens) == 0 {
			h.cfgMu.RUnlock()
			apiError(w, http.StatusServiceUnavailable, "api_disabled",
				"API is disabled: generate an API token in Settings to enable it")
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			h.cfgMu.RUnlock()
			apiError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid authorization header")
			return
		}
		tok := h.cfg.FindTokenByHash(config.HashToken(auth[len(prefix):]))
		if tok == nil {
			h.cfgMu.RUnlock()
			apiError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		scope := scopeForMethod(r.Method)
		hasScope := tok.HasScope(scope)
		h.cfgMu.RUnlock()
		if !hasScope {
			apiError(w, http.StatusForbidden, "insufficient_scope", "token lacks the "+scope+" scope")
			return
		}

		next(w, r)
	}
}

// scopeForMethod maps an HTTP method to the privilege a token must hold:
// writes for POST/PATCH/PUT, deletes for DELETE, reads for the rest.
func scopeForMethod(method string) string {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return config.ScopeWrite
	case http.MethodDelete:
		return config.ScopeDelete
	default:
		return config.ScopeRead
	}
}

// apiPathInt64 parses a numeric path segment, writing an
// invalid_request apiError on failure. The bool reports whether the
// caller can keep going.
func apiPathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid "+name)
		return 0, false
	}
	return v, true
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

// serverError writes the standard internal_error envelope when err is
// non-nil, reporting whether it did; callers use it as a return guard.
func serverError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
	return true
}

// badRequest is serverError's invalid_request twin.
func badRequest(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writePage emits the paginated envelope documented by paginatedSchema:
// {page, limit, total, results}.
func writePage(w http.ResponseWriter, page, limit, total int, results any) {
	writeJSON(w, http.StatusOK, map[string]any{
		"page":    page,
		"limit":   limit,
		"total":   total,
		"results": results,
	})
}

// parsePage reads page + limit from the query string and clamps limit
// to maxLimit. `page_size` is accepted as a synonym for `limit` so a
// caller using the more common page_size convention isn't silently
// clamped to defaultLimit.
func parsePage(r *http.Request, defaultLimit, maxLimit int) (offset, limit int) {
	page := 1
	limit = defaultLimit
	q := r.URL.Query()
	if p := q.Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	l := q.Get("limit")
	l = cmp.Or(l, q.Get("page_size"))
	if l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = min(n, maxLimit)
		}
	}
	return (page - 1) * limit, limit
}
