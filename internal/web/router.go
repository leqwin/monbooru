package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leqwin/monbooru/internal/api"
	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/tagger"
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
	jobs       *jobs.Manager
	sessions   *SessionStore
	loginRL    *loginRateLimiter
	csrfSecret []byte // per-instance HMAC key for CSRF tokens
	tmpl       *template.Template
	staticFS   fs.FS
	done       chan struct{} // closed on Close() to stop background goroutines

	// ctxMu guards contexts and activeName. Read handlers take RLock via
	// ContextMiddleware; mutation handlers take the write lock.
	ctxMu      sync.RWMutex
	contexts   map[string]*galleryCtx
	activeName string

	// schedMu guards the last-schedule-run fields. Written by runScheduler,
	// read by the Schedule settings section.
	schedMu       sync.Mutex
	schedLastRun  time.Time
	schedLastDur  time.Duration
	schedLastInfo string // "OK" or a short failure summary; empty when never run
}

// NewServer creates the HTTP server with all routes wired. One *db.DB is
// opened per configured gallery.
func NewServer(cfg *config.Config, configPath string, jobManager *jobs.Manager) (*Server, error) {
	sessions := NewSessionStore()

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
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		"deref64": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"deref64f": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"groupByImageSource": func(tagList []models.ImageTag) []imageTagSourceGroup {
			// Manual tags split by source: plain UI adds (empty tagger_name)
			// land in the "user" bucket; API-supplied sources each get their
			// own "Tags added by <source>" subsection. Auto rows keep the
			// existing per-tagger grouping with the "auto-tagger" suffix.
			var userTags []models.ImageTag
			byUserSource := map[string]*imageTagSourceGroup{}
			var userSourceOrder []string
			byTagger := map[string]*imageTagSourceGroup{}
			var order []string
			for _, t := range tagList {
				if !t.IsAuto {
					if t.TaggerName == "" {
						userTags = append(userTags, t)
						continue
					}
					key := t.TaggerName
					if _, ok := byUserSource[key]; !ok {
						userSourceOrder = append(userSourceOrder, key)
						byUserSource[key] = &imageTagSourceGroup{
							Source: key,
							Title:  "Tags added by " + key,
						}
					}
					byUserSource[key].Tags = append(byUserSource[key].Tags, t)
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
			for _, k := range userSourceOrder {
				out = append(out, *byUserSource[k])
			}
			for _, k := range order {
				g := byTagger[k]
				// Auto-tagger subgroups read more naturally ordered by the
				// tagger's own confidence: the tags the model was most sure
				// of sit at the top. User tags above keep the existing
				// alphabetical-by-category-then-usage order.
				sort.SliceStable(g.Tags, func(i, j int) bool {
					ci, cj := 0.0, 0.0
					if g.Tags[i].Confidence != nil {
						ci = *g.Tags[i].Confidence
					}
					if g.Tags[j].Confidence != nil {
						cj = *g.Tags[j].Confidence
					}
					return ci > cj
				})
				out = append(out, *g)
			}
			return out
		},
		"autoConfPct": func(c *float64) string {
			if c == nil {
				return ""
			}
			return strconv.Itoa(int(*c * 100))
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
		"cancelTitle": func(jobType string) string {
			// Tooltip for the job-status × button. Only the job types that
			// observe ctx.Done() in their worker loop appear here.
			switch jobType {
			case "autotag":
				return "Stop auto-tagging"
			case "sync":
				return "Stop syncing"
			case "delete":
				return "Stop deleting"
			case "re-extract":
				return "Stop re-extraction"
			case "rebuild-thumbs":
				return "Stop thumbnail rebuild"
			case "move":
				return "Stop moving"
			case "tag":
				return "Stop tagging"
			}
			return "Stop"
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
		"schedDuration": func(d time.Duration) string {
			// Round to the nearest second for anything over 1s; keep
			// millisecond precision below so sub-second scheduler passes
			// (the typical case on an idle gallery) still render usefully.
			if d >= time.Second {
				return d.Round(time.Second).String()
			}
			return d.Round(time.Millisecond).String()
		},
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"comfyRefTarget": func(s string) string {
			// Displayed ComfyUI references start with "→ " followed by the
			// referenced node's key. Strip the arrow+space so the template
			// can build `href="#comfy-node-<key>"` for in-page navigation.
			return strings.TrimPrefix(s, "→ ")
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
		// hasFavFilter reports whether the search query contains a `fav:true`
		// token, regardless of position or surrounding tags. Drives the gallery
		// header's ★ toggle's active class so the button doesn't go inactive
		// the moment the user combines `fav:true` with any other tag.
		"hasFavFilter": func(query string) bool {
			for _, tok := range strings.Fields(query) {
				if strings.EqualFold(tok, "fav:true") {
					return true
				}
			}
			return false
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
		jobs:       jobManager,
		sessions:   sessions,
		loginRL:    newLoginRateLimiter(),
		csrfSecret: mustRandBytes(32),
		tmpl:       tmpl,
		staticFS:   staticFS,
		done:       make(chan struct{}),
		contexts:   map[string]*galleryCtx{},
		activeName: cfg.DefaultGallery,
	}

	for _, g := range cfg.Galleries {
		cx, err := openGalleryCtx(g)
		if err != nil {
			for _, done := range s.contexts {
				done.close()
			}
			return nil, err
		}
		s.contexts[g.Name] = cx
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

	go s.runMemoryReclaim()

	// Daily scheduled maintenance runs driven by cfg.Schedule.
	go s.runScheduler()

	return s, nil
}

// runMemoryReclaim wakes every 5 minutes and, when no job is active,
// shrinks each gallery's SQLite page cache, returns the Go heap, and
// tears down the cached auto-tagger session set if it has been idle
// for tagger.idle_release_after_minutes.
func (s *Server) runMemoryReclaim() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if s.jobs.IsRunning() {
				continue
			}
			s.ctxMu.RLock()
			ctxs := make([]*galleryCtx, 0, len(s.contexts))
			for _, cx := range s.contexts {
				ctxs = append(ctxs, cx)
			}
			s.ctxMu.RUnlock()
			for _, cx := range ctxs {
				if err := cx.DB.ShrinkMemory(context.Background()); err != nil {
					logx.Warnf("memory reclaim %q: %v", cx.Name, err)
				}
			}
			debug.FreeOSMemory()
			s.cfgMu.Lock()
			mins := s.cfg.Tagger.IdleReleaseAfterMinutes
			s.cfgMu.Unlock()
			if mins > 0 {
				if tagger.ReleaseIdle(time.Duration(mins) * time.Minute) {
					logx.Infof("memory reclaim: released idle auto-tagger session")
				}
			}
		case <-s.done:
			return
		}
	}
}

// Active returns the currently-active gallery context.
func (s *Server) Active() *galleryCtx {
	s.ctxMu.RLock()
	defer s.ctxMu.RUnlock()
	return s.contexts[s.activeName]
}

// Get returns the gallery context with the given name, or nil.
func (s *Server) Get(name string) *galleryCtx {
	s.ctxMu.RLock()
	defer s.ctxMu.RUnlock()
	return s.contexts[name]
}

// ContextMiddleware RLocks ctxMu for the request so a concurrent swap can't
// tear state out under it. Mutation endpoints bypass it because they take
// the write lock themselves.
func (s *Server) ContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contextMiddlewareBypass(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		s.ctxMu.RLock()
		defer s.ctxMu.RUnlock()
		next.ServeHTTP(w, r)
	})
}

func contextMiddlewareBypass(path string) bool {
	if path == "/internal/gallery/switch" {
		return true
	}
	return strings.HasPrefix(path, "/static/") ||
		strings.HasPrefix(path, "/thumbnails/") ||
		strings.HasPrefix(path, "/settings/galleries")
}

// StartWatchers starts a watcher on every configured gallery at startup. Each
// gallery owns its own watcher for the lifetime of the process so file drops
// into any gallery are picked up in real time, not just the active one.
//
// Also spawns a pre-warm goroutine per gallery that populates the FolderTree,
// SourceCounts, and VisibleCount caches. The first user request then hits
// warm caches instead of paying a cold aggregation scan against every
// visible image - on libraries with tens of thousands of images that walk
// was the dominant contributor to first-sidebar latency.
func (s *Server) StartWatchers() {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	for _, cx := range s.contexts {
		cx.startWatcher(s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB, s.jobs)
		go cx.warmCaches()
	}
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))))
	mux.HandleFunc("GET /thumbnails/{gallery}/{file}", s.serveThumbnail)

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

	// Root only; `GET /` below is the catch-all for unmatched paths. The
	// `/{$}` pattern wins over `/` for the exact root.
	mux.HandleFunc("GET /{$}", s.galleryHandler)
	mux.HandleFunc("GET /", s.notFoundHandler)

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
	mux.HandleFunc("POST /images/{id}/move", s.moveImage)
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
	mux.HandleFunc("POST /settings/general", s.settingsGeneralPost)
	mux.HandleFunc("POST /settings/tagger", s.settingsTaggerPost)
	mux.HandleFunc("POST /settings/auth/password", s.settingsPasswordPost)
	mux.HandleFunc("POST /settings/auth/remove-password", s.settingsRemovePasswordPost)
	mux.HandleFunc("POST /settings/auth/token", s.settingsTokenPost)
	mux.HandleFunc("PATCH /settings/categories/{id}", s.updateCategoryPatch)
	mux.HandleFunc("POST /settings/schedule", s.settingsSchedulePost)
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
	mux.HandleFunc("POST /settings/tagger/{name}/delete", s.settingsTaggerDeletePost)

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
	mux.HandleFunc("POST /internal/batch-move", s.batchMove)
	mux.HandleFunc("POST /internal/batch-tag", s.batchTag)
	mux.HandleFunc("POST /internal/delete-search", s.deleteSearchPost)
	mux.HandleFunc("POST /internal/delete-folder", s.deleteFolderPost)
	mux.HandleFunc("GET /internal/tags/suggest", s.tagSuggest)
	mux.HandleFunc("GET /internal/search/suggest", s.searchSuggest)
	mux.HandleFunc("GET /internal/folders/suggest", s.foldersSuggest)
	mux.HandleFunc("GET /internal/sidebar", s.gallerySidebar)
	mux.HandleFunc("GET /internal/sidebar-browse", s.sidebarBrowse)
	mux.HandleFunc("POST /images/{id}/autotag", s.autotagImage)
	mux.HandleFunc("GET /images/{id}/tags", s.getImageTagsHandler)

	mux.HandleFunc("POST /internal/gallery/switch", s.gallerySwitchHandler)
	mux.HandleFunc("POST /settings/galleries", s.settingsGalleriesPost)
	mux.HandleFunc("POST /settings/galleries/{name}/rename", s.settingsGalleryRenamePost)
	mux.HandleFunc("POST /settings/galleries/{name}/delete", s.settingsGalleryDeletePost)
	mux.HandleFunc("POST /settings/galleries/{name}/default", s.settingsGalleryDefaultPost)
	mux.HandleFunc("GET /settings/galleries/{name}/export", s.settingsGalleryExport)
	mux.HandleFunc("POST /settings/galleries/{name}/import", s.settingsGalleryImport)

	api.New(s.cfg, s.jobs, s.apiResolver).Mount(mux)

	// Middleware order, outermost first: logging, context (RLock), session, CSRF.
	var h http.Handler = mux
	h = s.CSRFMiddleware(h)
	h = s.SessionMiddleware(h)
	h = s.ContextMiddleware(h)
	h = loggingMiddleware(h)

	return h
}

// apiResolver looks up a gallery by name for the API package. Empty name
// falls back to the active gallery.
func (s *Server) apiResolver(name string) (api.Gallery, bool) {
	var cx *galleryCtx
	if name == "" {
		cx = s.Active()
	} else {
		cx = s.Get(name)
	}
	if cx == nil {
		return api.Gallery{}, false
	}
	return api.Gallery{
		Name:             cx.Name,
		GalleryPath:      cx.GalleryPath,
		ThumbnailsPath:   cx.ThumbnailsPath,
		DB:               cx.DB,
		TagSvc:           cx.TagSvc,
		InvalidateCaches: cx.InvalidateCaches,
	}, true
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
	Title         string
	ActiveNav     string
	CSRFToken     string
	AuthEnabled   bool
	Degraded      bool
	Version       string
	RepoURL       string
	Variant       string
	ActiveGallery string
	Galleries     []config.Gallery
	// Counts surfaced on the footer status bar. Populated per-request;
	// zero when the active gallery is missing or a query failed.
	VisibleCount int
	TagCount     int
	SavedCount   int
}

func (s *Server) base(r *http.Request, nav, title string) baseData {
	sessID := sessionFromContext(r.Context())
	cx := s.contexts[s.activeName] // ctxMu RLocked by ContextMiddleware
	degraded := false
	visible, tagCount, savedCount := 0, 0, 0
	if cx != nil {
		degraded = cx.Degraded
		visible, _ = cx.VisibleCount()
		tagCount, _ = cx.TagCount()
		savedCount, _ = cx.SavedCount()
	}
	// Copy the gallery list so template rendering never dereferences the map
	// under a concurrent mutation (the middleware lock is scoped to the
	// request, but the slice is cheap and small).
	galleries := make([]config.Gallery, len(s.cfg.Galleries))
	copy(galleries, s.cfg.Galleries)
	return baseData{
		Title:         title,
		ActiveNav:     nav,
		CSRFToken:     s.csrfToken(sessID),
		AuthEnabled:   s.cfg.Auth.EnablePassword,
		Degraded:      degraded,
		Version:       Version,
		RepoURL:       RepoURL,
		Variant:       Variant,
		ActiveGallery: s.activeName,
		Galleries:     galleries,
		VisibleCount:  visible,
		TagCount:      tagCount,
		SavedCount:    savedCount,
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

// serveThumbnail serves a thumbnail file from the named gallery's
// thumbnails directory. The gallery name is part of the URL so each
// gallery's thumbnails live at distinct URLs and the browser cache can't
// show a stale preview from another gallery after a switch.
func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	file := filepath.Base(r.PathValue("file"))
	if !thumbnailNameRe.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	cx := s.Get(r.PathValue("gallery"))
	if cx == nil {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(cx.ThumbnailsPath, file)
	// Hover variants are generated by ffmpeg after the static thumb and are
	// absent for recently-ingested animated files (and every static image,
	// which the grid doesn't request a hover for). Respond 204 so the img
	// tag's onerror still fires but the console doesn't log a 404 per card.
	if strings.HasSuffix(file, "_hover.webp") {
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.ServeFile(w, r, fullPath)
}

// serveImageFile serves the raw image/video file.
func (s *Server) serveImageFile(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	cx := s.Active()
	if cx == nil {
		http.NotFound(w, r)
		return
	}
	var canonPath string
	if err := cx.DB.Read.QueryRow(
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
	galleryAbs, err := filepath.Abs(cx.GalleryPath)
	if err != nil || !gallery.PathInside(galleryAbs, absPath) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, canonPath)
}

// Close stops background goroutines and closes every gallery's database.
func (s *Server) Close() {
	select {
	case <-s.done:
		// already closed
	default:
		close(s.done)
	}
	tagger.ReleaseAll()
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	for _, cx := range s.contexts {
		cx.close()
	}
}

// saveConfig acquires the config mutex, writes the config file, and returns
// any error so callers can surface the failure to the user instead of leaving
// the in-memory cfg out of sync with what's actually persisted to disk.
func (s *Server) saveConfig() error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := config.Save(s.cfg, s.configPath); err != nil {
		logx.Errorf("config save: %v", err)
		return err
	}
	return nil
}
