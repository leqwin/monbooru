package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/leqwin/monbooru/internal/api"
	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/tags"
	webFS "github.com/leqwin/monbooru/web"
)

// tagGroup is used by the groupByCategory template function.
type tagGroup struct {
	Name  string
	Color string
	Tags  []models.Tag
}

// imageTagGroup is used by the groupByImageTags template function.
type imageTagGroup struct {
	Name  string
	Color string
	Tags  []models.ImageTag
}

// imageTagSourceGroup is used by the groupByImageSource template function
// which splits the detail-page tag list into subsections by origin:
// manual ("user") and one group per distinct auto-tagger name.
type imageTagSourceGroup struct {
	Source string // "user" or tagger name
	Title  string
	Tags   []models.ImageTag
}

// Server holds all shared state for the HTTP server.
type Server struct {
	cfg        *config.Config
	configPath string
	cfgMu      sync.Mutex // protects cfg writes and config.Save calls
	db         *db.DB
	jobs       *jobs.Manager
	tagSvc     *tags.Service
	sessions   *SessionStore
	loginRL    *loginRateLimiter
	csrfSecret []byte // per-instance HMAC key for CSRF tokens
	tmpl       *template.Template
	staticFS   fs.FS
	degraded   bool // true when gallery_path is unreadable
	done       chan struct{} // closed on Close() to stop background goroutines
}

// NewServer creates the HTTP server with all routes wired.
func NewServer(cfg *config.Config, configPath string, database *db.DB, jobManager *jobs.Manager, degraded bool) (*Server, error) {
	sessions := NewSessionStore()
	tagSvc := tags.New(database)

	// Parse all templates
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"seq": func(start, end int) []int {
			r := make([]int, 0, end-start+1)
			for i := start; i <= end; i++ {
				r = append(r, i)
			}
			return r
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"mul": func(a, b int) int { return a * b },
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				k, _ := pairs[i].(string)
				m[k] = pairs[i+1]
			}
			return m
		},
		"groupByCategory": func(tagList []models.Tag) []tagGroup {
			order := []string{}
			groups := map[string]*tagGroup{}
			for _, t := range tagList {
				key := t.CategoryName
				if _, ok := groups[key]; !ok {
					order = append(order, key)
					groups[key] = &tagGroup{Name: t.CategoryName, Color: t.CategoryColor}
				}
				groups[key].Tags = append(groups[key].Tags, t)
			}
			out := make([]tagGroup, 0, len(order))
			for _, k := range order {
				out = append(out, *groups[k])
			}
			return out
		},
		"folderCount": func(nodes []gallery.FolderNode) int {
			total := 0
			for _, n := range nodes {
				total += n.Count
			}
			return total
		},
		"deref":    func(p *int) int { if p == nil { return 0 }; return *p },
		"deref64":  func(p *int64) int64 { if p == nil { return 0 }; return *p },
		"deref64f": func(p *float64) float64 { if p == nil { return 0 }; return *p },
		"groupByImageSource": func(tagList []models.ImageTag) []imageTagSourceGroup {
			var userTags []models.ImageTag
			byTagger := map[string]*imageTagSourceGroup{}
			var order []string
			for _, t := range tagList {
				if !t.IsAuto {
					userTags = append(userTags, t)
					continue
				}
				key := t.TaggerName
				if key == "" {
					key = "auto-tagger"
				}
				if _, ok := byTagger[key]; !ok {
					order = append(order, key)
					byTagger[key] = &imageTagSourceGroup{
						Source: key,
						Title:  "Tags added by the " + key + " auto-tagger",
					}
				}
				byTagger[key].Tags = append(byTagger[key].Tags, t)
			}
			out := []imageTagSourceGroup{}
			if len(userTags) > 0 {
				out = append(out, imageTagSourceGroup{
					Source: "user",
					Title:  "Tags added by the user",
					Tags:   userTags,
				})
			}
			for _, k := range order {
				out = append(out, *byTagger[k])
			}
			return out
		},
		"groupByImageTags": func(tagList []models.ImageTag) []imageTagGroup {
			order := []string{}
			groups := map[string]*imageTagGroup{}
			for _, t := range tagList {
				key := t.Category
				if _, ok := groups[key]; !ok {
					order = append(order, key)
					groups[key] = &imageTagGroup{Name: t.Category, Color: t.Color}
				}
				groups[key].Tags = append(groups[key].Tags, t)
			}
			out := make([]imageTagGroup, 0, len(order))
			for _, k := range order {
				out = append(out, *groups[k])
			}
			return out
		},
		"humanBytes": func(b int64) string {
			const unit = 1024
			if b < unit {
				return fmt.Sprintf("%d B", b)
			}
			div, exp := int64(unit), 0
			for n := b / unit; n >= unit; n /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
		},
		"isLongValue": func(s string) bool {
			return len(s) > 200 || strings.ContainsAny(s, "\n\r")
		},
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			r := []rune(s)
			if len(r) <= n {
				return s
			}
			return string(r[:n])
		},
	}).ParseFS(webFS.FS, "templates/*.html", "templates/partials/*.html")
	if err != nil {
		return nil, err
	}

	staticFS, err := fs.Sub(webFS.FS, "static")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:        cfg,
		configPath: configPath,
		db:         database,
		jobs:       jobManager,
		tagSvc:     tagSvc,
		sessions:   sessions,
		loginRL:    newLoginRateLimiter(),
		csrfSecret: mustRandBytes(32),
		tmpl:       tmpl,
		staticFS:   staticFS,
		degraded:   degraded,
		done:       make(chan struct{}),
	}

	// Periodically sweep expired sessions and login rate-limiter entries.
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sessions.SweepExpired()
				s.loginRL.sweep()
			case <-s.done:
				return
			}
		}
	}()

	return s, nil
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))))
	mux.HandleFunc("GET /thumbnails/{file}", s.serveThumbnail)

	// Health check (unauthenticated)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": Version})
	})

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("POST /logout", s.logoutPost)

	mux.HandleFunc("GET /upload", s.uploadPage)
	mux.HandleFunc("POST /upload", s.uploadPost)

	// Root only — `GET /` would otherwise act as a catch-all in Go 1.22's
	// servemux, turning every mistyped URL into the home page.
	mux.HandleFunc("GET /{$}", s.galleryHandler)

	mux.HandleFunc("GET /images/{id}", s.detailHandler)
	mux.HandleFunc("GET /images/{id}/related", s.relatedImagesHandler)
	mux.HandleFunc("GET /images/{id}/file", s.serveImageFile)
	mux.HandleFunc("POST /images/{id}/tags", s.addTagToImage)
	mux.HandleFunc("DELETE /images/{id}/tags", s.removeAllTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/user-tags", s.removeUserTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/auto-tags", s.removeAutoTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/tags/{tagID}", s.removeTagFromImage)
	mux.HandleFunc("POST /images/{id}/favorite", s.toggleFavorite)
	mux.HandleFunc("DELETE /images/{id}", s.deleteImage)
	mux.HandleFunc("POST /images/{id}/canonical-path", s.promoteCanonical)
	mux.HandleFunc("DELETE /images/{id}/aliases/{pathID}", s.deleteAlias)

	mux.HandleFunc("GET /tags", s.tagsHandler)
	mux.HandleFunc("POST /tags/merge", s.mergeTagsPost)
	mux.HandleFunc("POST /tags/{id}/rename", s.renameTagPost)
	mux.HandleFunc("DELETE /tags/{id}", s.deleteTagHandler)
	mux.HandleFunc("PATCH /tags/{id}/category", s.changeTagCategory)
	mux.HandleFunc("POST /tags/categories", s.createCategoryPost)
	mux.HandleFunc("POST /tags/categories/{id}/rename", s.renameCategoryPost)
	mux.HandleFunc("DELETE /tags/categories/{id}", s.deleteCategoryDelete)
	mux.HandleFunc("GET /tags/categories/{id}/count", s.categoryCountHandler)

	mux.HandleFunc("GET /categories", s.categoriesHandler)

	mux.HandleFunc("GET /settings", s.settingsHandler)
	mux.HandleFunc("POST /settings/gallery", s.settingsGalleryPost)
	mux.HandleFunc("POST /settings/tagger", s.settingsTaggerPost)
	mux.HandleFunc("POST /settings/auth/password", s.settingsPasswordPost)
	mux.HandleFunc("POST /settings/auth/remove-password", s.settingsRemovePasswordPost)
	mux.HandleFunc("POST /settings/auth/token", s.settingsTokenPost)
	mux.HandleFunc("POST /settings/ui", s.settingsUIPost)
	mux.HandleFunc("PATCH /settings/categories/{id}", s.updateCategoryPatch)
	mux.HandleFunc("POST /settings/maintenance/prune-missing", s.pruneMissingImagesPost)
	mux.HandleFunc("POST /settings/maintenance/prune-orphaned-thumbnails", s.pruneOrphanedThumbnailsPost)
	mux.HandleFunc("POST /settings/maintenance/recalc-tags", s.recalcTagsPost)
	mux.HandleFunc("POST /settings/maintenance/merge-general-tags", s.mergeGeneralTagsPost)
	mux.HandleFunc("GET /settings/maintenance/duplicates-list", s.duplicatesListHandler)
	mux.HandleFunc("POST /settings/maintenance/remove-duplicates", s.removeDuplicatesPost)
	mux.HandleFunc("POST /settings/maintenance/re-extract-metadata", s.reExtractMetadataPost)
	mux.HandleFunc("POST /settings/maintenance/rebuild-thumbnails", s.rebuildThumbnailsPost)
	mux.HandleFunc("POST /settings/maintenance/vacuum-db", s.vacuumDBPost)
	mux.HandleFunc("POST /settings/tagger/remove-autotagged", s.removeAutotaggedPost)
	mux.HandleFunc("POST /settings/tagger/remove-user-tags", s.removeAllUserTagsPost)
	mux.HandleFunc("POST /settings/tagger/remove-all-tags", s.removeAllTagsPost)
	mux.HandleFunc("POST /settings/tagger/{name}/enable", s.settingsTaggerEnablePost)
	mux.HandleFunc("POST /settings/tagger/{name}/disable", s.settingsTaggerDisablePost)

	// Saved searches are managed from the sidebar (no dedicated search page).
	mux.HandleFunc("POST /search/saved", s.createSavedSearch)
	mux.HandleFunc("DELETE /search/saved/{id}", s.deleteSavedSearch)

	mux.HandleFunc("GET /help", s.helpHandler)

	mux.HandleFunc("GET /internal/job/status", s.jobStatusHandler)
	mux.HandleFunc("POST /internal/job/dismiss", s.jobDismissPost)
	mux.HandleFunc("POST /internal/job/cancel", s.jobCancelPost)
	mux.HandleFunc("POST /internal/sync", s.syncTrigger)
	mux.HandleFunc("POST /internal/autotag", s.autotagTrigger)
	mux.HandleFunc("POST /internal/batch-delete", s.batchDelete)
	mux.HandleFunc("POST /internal/delete-search", s.deleteSearchPost)
	mux.HandleFunc("POST /internal/delete-folder", s.deleteFolderPost)
	mux.HandleFunc("GET /internal/tags/suggest", s.tagSuggest)
	mux.HandleFunc("GET /internal/search/suggest", s.searchSuggest)
	mux.HandleFunc("GET /internal/sidebar", s.gallerySidebar)
	mux.HandleFunc("POST /images/{id}/autotag", s.autotagImage)
	mux.HandleFunc("GET /images/{id}/tags", s.getImageTagsHandler)

	api.New(s.cfg, s.db, s.jobs).Mount(mux)

	// Middleware order: logging wraps everything, session establishes identity,
	// CSRF runs innermost so it sees the session context.
	var h http.Handler = mux
	h = s.CSRFMiddleware(h)
	h = s.SessionMiddleware(h)
	h = loggingMiddleware(h)

	return h
}

// isNoisyPath reports paths that are requested constantly (polling, static
// assets, thumbnails, health probes). They log at debug so the default info
// level stays readable.
func isNoisyPath(path string) bool {
	switch path {
	case "/internal/job/status", "/health":
		return true
	}
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/thumbnails/")
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isNoisyPath(r.URL.Path) {
			logx.Debugf("%s %s", r.Method, r.URL.Path)
		} else {
			logx.Infof("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// Version is set at build time via -ldflags, or read from VERSION.md.
var Version = "dev"

// RepoURL is the canonical git repository URL, set at build time via -ldflags.
var RepoURL = "https://github.com/leqwin/monbooru"

// Variant identifies the build flavour (e.g. "cuda") and is injected at
// build time via -ldflags from the CUDA Dockerfile. Empty for the default
// CPU build; rendered in parentheses in the footer when non-empty.
var Variant = ""

// baseData is common template data present on every page.
type baseData struct {
	Title       string
	ActiveNav   string
	CSRFToken   string
	AuthEnabled bool
	Degraded    bool
	Version     string
	RepoURL     string
	Variant     string
}

func (s *Server) base(r *http.Request, nav, title string) baseData {
	sessID := sessionFromContext(r.Context())
	return baseData{
		Title:       title,
		ActiveNav:   nav,
		CSRFToken:   s.csrfToken(sessID),
		AuthEnabled: s.cfg.Auth.EnablePassword,
		Degraded:    s.degraded,
		Version:     Version,
		RepoURL:     RepoURL,
		Variant:     Variant,
	}
}

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Buffer so we can still send a clean 500 when template execution fails;
	// streaming directly into w would leak partial output and race with
	// http.Error (producing "superfluous response.WriteHeader" warnings).
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		logx.Errorf("template %q: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if _, err := buf.WriteTo(w); err != nil {
		// Client disconnected mid-write. Nothing to do but log.
		logx.Warnf("template %q write: %v", name, err)
	}
}

// thumbnailNameRe matches the two on-disk filename patterns emitted by the
// thumbnail pipeline: `{id}.jpg` for static previews and `{id}_hover.webp`
// for animated hovers. Anything else under the thumbnails directory (stray
// files, editor backups, etc.) is not served.
var thumbnailNameRe = regexp.MustCompile(`^\d+(?:_hover\.webp|\.jpg)$`)

// serveThumbnail serves a thumbnail file from the configured thumbnails directory.
func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	file := filepath.Base(r.PathValue("file"))
	if !thumbnailNameRe.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.cfg.Paths.ThumbnailsPath, file))
}

// serveImageFile serves the raw image/video file.
func (s *Server) serveImageFile(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	var canonPath string
	if err := s.db.Read.QueryRow(
		`SELECT canonical_path FROM images WHERE id = ?`, idStr,
	).Scan(&canonPath); err != nil {
		http.NotFound(w, r)
		return
	}
	// Ensure resolved path is within the gallery directory to prevent serving
	// arbitrary files. Use filepath.Rel so a sibling directory that shares a
	// literal prefix with the gallery root (e.g. `/data/gallery_backup` vs
	// `/data/gallery`) is correctly rejected.
	absPath, err := filepath.Abs(canonPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	galleryAbs, err := filepath.Abs(s.cfg.Paths.GalleryPath)
	if err != nil || !gallery.PathInside(galleryAbs, absPath) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, canonPath)
}

// Close stops background goroutines. Call on server shutdown.
func (s *Server) Close() {
	select {
	case <-s.done:
		// already closed
	default:
		close(s.done)
	}
}

// saveConfig acquires the config mutex, writes the config file, and releases.
func (s *Server) saveConfig() {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	config.Save(s.cfg, s.configPath)
}
