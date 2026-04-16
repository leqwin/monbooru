// Package api implements the /api/v1/ REST handlers for monbooru.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/tags"
)

// Handler is the root handler for all /api/v1/ routes.
type Handler struct {
	cfg    *config.Config
	db     *db.DB
	tagSvc *tags.Service
	jobs   *jobs.Manager
}

// New creates a new API handler.
func New(cfg *config.Config, database *db.DB, jobManager *jobs.Manager) *Handler {
	return &Handler{
		cfg:    cfg,
		db:     database,
		tagSvc: tags.New(database),
		jobs:   jobManager,
	}
}

// Mount registers all API routes on mux under /api/v1/.
func (h *Handler) Mount(mux *http.ServeMux) {
	// Images
	mux.HandleFunc("POST /api/v1/images", h.auth(h.createImage))
	mux.HandleFunc("GET /api/v1/images/search", h.auth(h.searchImages))
	mux.HandleFunc("GET /api/v1/images/{id}", h.auth(h.getImage))
	mux.HandleFunc("DELETE /api/v1/images/{id}", h.auth(h.deleteImage))
	mux.HandleFunc("POST /api/v1/images/{id}/tags", h.auth(h.addImageTags))
	mux.HandleFunc("DELETE /api/v1/images/{id}/tags", h.auth(h.removeImageTags))

	// Tags
	mux.HandleFunc("GET /api/v1/tags", h.auth(h.listTags))

	// OpenAPI
	mux.HandleFunc("GET /api/v1/openapi.json", h.openAPIJSON)
	mux.HandleFunc("GET /api/v1/docs", h.openAPIDocs)

	// Root: return API info
	mux.HandleFunc("GET /api/v1/", h.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/" {
			apiError(w, http.StatusNotFound, "not_found", "endpoint not found: "+r.URL.Path)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"api":     "monbooru",
			"docs":    "/api/v1/docs",
			"openapi": "/api/v1/openapi.json",
		})
	}))
}

// auth wraps a handler with optional bearer token authentication.
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// CORS: only allow requests from the configured base URL.
		origin := r.Header.Get("Origin")
		if origin != "" && h.cfg.Server.BaseURL != "" {
			if origin != h.cfg.Server.BaseURL {
				apiError(w, http.StatusForbidden, "forbidden", "CORS: origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", h.cfg.Server.BaseURL)
		}

		// Bearer token auth. The API is disabled until an API token is generated
		// from the Settings page; an empty token means no one can use the API.
		if h.cfg.Auth.APIToken == "" {
			apiError(w, http.StatusServiceUnavailable, "api_disabled",
				"API is disabled: generate an API token in Settings to enable it")
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			apiError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid authorization header")
			return
		}
		token := auth[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.cfg.Auth.APIToken)) != 1 {
			apiError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}

		next(w, r)
	}
}

// apiError writes a JSON error response.
func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

// writeJSON encodes v as JSON and writes it with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// parsePage parses page and limit from query params.
func parsePage(r *http.Request, defaultLimit, maxLimit int) (offset, limit int) {
	page := 1
	limit = defaultLimit
	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := parseInt(p); err == nil && n > 0 {
			page = n
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := parseInt(l); err == nil && n > 0 {
			if n > maxLimit {
				n = maxLimit
			}
			limit = n
		}
	}
	return (page - 1) * limit, limit
}

func parseInt(s string) (int, error) {
	return strconv.Atoi(s)
}
