package web

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/plugins"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
	webFS "github.com/monbooru/monbooru/web"
)

// groupOrdered buckets items by key in first-appearance order. skip drops
// an item entirely (nil keeps all); newGroup builds a bucket from its first
// item; add appends the item to its bucket.
func groupOrdered[T, G any](items []T, skip func(T) bool, key func(T) string, newGroup func(T) *G, add func(*G, T)) []G {
	order := []string{}
	groups := map[string]*G{}
	for _, t := range items {
		if skip != nil && skip(t) {
			continue
		}
		k := key(t)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
			groups[k] = newGroup(t)
		}
		add(groups[k], t)
	}
	out := make([]G, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out
}

// tagGroup is used by the groupByCategory template function.
type tagGroup struct {
	Name  string
	Color string
	Tags  []models.Tag
}

// Server holds all shared state for the HTTP server.
type Server struct {
	cfg        *config.Config
	configPath string
	cfgMu      sync.RWMutex // guards cfg reads/writes and config.Save calls
	jobs       *jobs.Manager
	pairs      *pairStore
	sessions   *SessionStore
	loginRL    *loginRateLimiter
	csrfSecret []byte // per-instance HMAC key for CSRF tokens
	tmpl       *template.Template
	staticFS   fs.FS
	done       chan struct{} // closed on Close() to stop background goroutines

	// schedReload wakes runScheduler so a Settings → Schedule edit takes
	// effect on the next select tick instead of waiting out the current
	// sleep. Buffered cap 1 with non-blocking sends so concurrent saves
	// coalesce into one reload.
	schedReload chan struct{}

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
	// schedLookupInfo is what the last run's lookup phases found, one line
	// per phase and gallery; the run-level info above cannot carry it.
	schedLookupInfo []string
	// schedGalleryOffset is where the next run starts in the gallery list,
	// so a budget too small for the first gallery cannot starve the rest.
	schedGalleryOffset int

	// monloaderStatusMu guards the cached footer-light probe result. base()
	// seeds every page's initial render from it so the light shows its last
	// known state at once instead of flickering back to "checking" (and
	// re-probing monloader) on every navigation; the poll refreshes it at
	// most once per monloaderStatusTTL.
	monloaderStatusMu      sync.Mutex
	monloaderConn          string
	monloaderVersion       string
	monloaderPTR           bool
	monloaderPTRSyncing    bool
	monloaderContrib       bool
	monloaderContribBanned bool
	monloaderContribFailed int
	monloaderCheckedAt     time.Time

	// themeWarnMu guards themeWarned, the last unresolvable server.theme
	// value reported. Theme resolution runs on every render, so the warning
	// is deduped rather than repeated per page.
	themeWarnMu sync.Mutex
	themeWarned string

	// pluginSupervisor owns the child process of every dropped plugin the
	// operator enabled. The supervision state lives there rather than on
	// Server so launch, restart backoff and stop-grace are exercisable
	// without a whole HTTP server.
	pluginSupervisor *plugins.Supervisor

	// pluginProbeMu guards pluginProbes, one cached /health result per paired
	// plugin. Button rendering gates on it, so a peer that went away stops
	// offering surfaces that would only fail.
	pluginProbeMu sync.Mutex
	pluginProbes  map[string]pluginProbe

	// fetchStatusMu guards fetchStatus, the last-known outcome of each image's
	// source metadata fetch. monloader runs the fetch asynchronously and calls
	// the enrich endpoint back; the detail page polls for the outcome so the
	// tags show up (or the failure surfaces) without a manual reload.
	fetchStatusMu sync.Mutex
	fetchStatus   map[string]fetchStatusEntry
}

// NewServer creates the HTTP server with all routes wired. One *db.DB is
// opened per configured gallery.
func NewServer(cfg *config.Config, configPath string, jobManager *jobs.Manager) (*Server, error) {
	sessions := NewSessionStore()

	// Parse all templates
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(webFS.FS, "templates/*.html", "templates/partials/*.html")
	if err != nil {
		return nil, err
	}

	staticFS, err := fs.Sub(webFS.FS, "static")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:          cfg,
		configPath:   configPath,
		jobs:         jobManager,
		pairs:        newPairStore(),
		sessions:     sessions,
		loginRL:      newLoginRateLimiter(),
		csrfSecret:   mustRandBytes(32),
		tmpl:         tmpl,
		staticFS:     staticFS,
		done:         make(chan struct{}),
		schedReload:  make(chan struct{}, 1),
		contexts:     map[string]*galleryCtx{},
		activeName:   cfg.DefaultGallery,
		fetchStatus:  map[string]fetchStatusEntry{},
		pluginProbes: map[string]pluginProbe{},
	}
	s.pluginSupervisor = plugins.New(s.pluginCallbackURL, s.done)

	applyRelationsConfig(cfg.Relations)

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

	// Validate the operator-supplied custom CSS path lives in a directory
	// monbooru already trusts (config dir, /config, or /data). Any other
	// path could leak the file's contents at /custom.css to LAN viewers
	// when the operator misconfigures the value (typo: /etc/passwd) - a
	// footgun the threat model treats as in-scope.
	if cfg.Server.CustomCSS != "" {
		if !customCSSPathAllowed(cfg.Server.CustomCSS, configPath) {
			logx.Warnf("server.custom_css %q lives outside the trusted dirs (configdir, /config, /data); the link is suppressed", cfg.Server.CustomCSS)
			s.cfg.Server.CustomCSS = ""
		}
	}
	if cfg.Server.BooruLogo != "" {
		if !customCSSPathAllowed(cfg.Server.BooruLogo, configPath) {
			logx.Warnf("server.logo %q lives outside the trusted dirs (configdir, /config, /data); the override is suppressed", cfg.Server.BooruLogo)
			s.cfg.Server.BooruLogo = ""
		}
	}
	if cfg.Server.ThemeColor != "" && !tags.IsValidCategoryColor(cfg.Server.ThemeColor) {
		logx.Warnf("server.theme_color %q is not a #rgb / #rrggbb colour; the bundled palette is used", cfg.Server.ThemeColor)
		s.cfg.Server.ThemeColor = ""
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

	go s.runPluginProbes()
	s.seedThemesDir()
	s.ensurePluginsDir()
	s.startManagedPlugins()

	return s, nil
}

// idleIndexReleaseAfter is how long a gallery's phash and counted-tag
// indexes may sit unread before the reclaim loop drops them. Long
// enough that a browse session pausing doesn't pay the rebuild, short
// enough that a finished one gives the memory back.
const idleIndexReleaseAfter = 30 * time.Minute

// runMemoryReclaim wakes every 5 minutes and, when no job is active,
// drops each gallery's idle in-memory indexes, shrinks its SQLite page
// cache, returns the Go heap, and tears down the cached auto-tagger
// session set if it has been idle for tagger.idle_release_after_minutes.
func (s *Server) runMemoryReclaim() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if s.jobs.IsRunning() {
				continue
			}
			ctxs := s.allContexts()
			for _, cx := range ctxs {
				dropped := counts.ReleaseIdleCountedTags(cx.DB, idleIndexReleaseAfter)
				if cx.bkTree != nil && cx.bkTree.ReleaseIdle(idleIndexReleaseAfter) {
					dropped = true
				}
				if dropped {
					logx.Debugf("memory reclaim %q: dropped idle indexes", cx.Name)
				}
				if err := cx.DB.ShrinkMemory(context.Background()); err != nil {
					logx.Warnf("memory reclaim %q: %v", cx.Name, err)
				}
			}
			search.AdjacencyCacheSweep()
			s.pruneFetchStatus()
			s.reconcileAllLookups()
			debug.FreeOSMemory()
			s.cfgMu.Lock()
			mins := s.cfg.Tagger.IdleReleaseAfterMinutes
			s.cfgMu.Unlock()
			if mins > 0 {
				before := readVmRSS()
				if tagger.ReleaseIdle(time.Duration(mins) * time.Minute) {
					after := readVmRSS()
					if before > 0 && after > 0 && before > after {
						logx.Infof("memory reclaim: released idle auto-tagger session (-%s)", humanBytesFmt(int64(before-after)))
					} else {
						logx.Infof("memory reclaim: released idle auto-tagger session")
					}
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
	switch path {
	case "/internal/gallery/switch", "/custom.css", "/custom.logo", "/theme.css", "/theme.logo", "/manifest.json":
		return true
	}
	if strings.HasPrefix(path, "/i/") {
		// /i/{sha} can switch the active gallery (write lock) when the image
		// lives in another gallery, so it must not run under the request-held
		// read lock.
		return true
	}
	if path == "/internal/monloader-status" || path == "/internal/monloader/disconnect" || path == "/internal/monloader/reconnect" ||
		path == "/internal/plugin/relay" || strings.HasPrefix(path, "/settings/plugins/") ||
		strings.HasPrefix(path, pluginMountPrefix) {
		// These poll / probe a peer over HTTP and touch no gallery context;
		// holding the read lock across the outbound call would stall a switch.
		// The plugin mount is the longest of them: a peer's page is served for
		// as long as the peer takes to serve it.
		return true
	}
	if (strings.HasPrefix(path, "/images/") || strings.HasPrefix(path, "/tags/")) &&
		(strings.HasSuffix(path, "/sources/fetch") || strings.HasSuffix(path, "/lookup") ||
			strings.HasSuffix(path, "/replace") ||
			strings.HasSuffix(path, "/ptr-contrib-panel") || strings.HasSuffix(path, "/ptr-contrib-dialog") ||
			strings.HasSuffix(path, "/ptr-contrib") || strings.HasSuffix(path, "/ptr-lookup-preview") ||
			strings.HasSuffix(path, "/ptr-lookup-search")) {
		// These proxy to monloader over HTTP; holding the request read lock
		// across the outbound call would stall a gallery switch. The handlers
		// read their rows under their own short locks.
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
// source-label, and visible-count caches. The first user request then hits
// warm caches instead of paying a cold aggregation scan against every
// visible image - on libraries with tens of thousands of images that walk
// was the dominant contributor to first-sidebar latency.
func (s *Server) StartWatchers() {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	for _, cx := range s.contexts {
		cx.startWatcher(s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB, s.ingestNaming(cx.Name), s.jobs)
		cx.startMangaReclaim()
		go cx.warmCaches()
	}
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))))
	mux.HandleFunc("GET /custom.css", s.serveCustomCSS)
	mux.HandleFunc("GET /custom.logo", s.serveCustomLogo)
	mux.HandleFunc("GET /theme.css", s.serveThemeCSS)
	mux.HandleFunc("GET /theme.logo", s.serveThemeLogo)
	mux.HandleFunc("GET /manifest.json", s.manifestHandler)
	mux.HandleFunc("GET /thumbnails/{gallery}/{file}", s.serveThumbnail)
	// Fallback icon for tabs with no <link rel="icon"> (a raw image opened
	// in a new tab). Route through the override so server.logo applies;
	// non-permanent since that target can change.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.booruFaviconURL(), http.StatusFound)
	})

	// Health check (unauthenticated)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": Version})
	})

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("POST /logout", s.logoutPost)

	mux.HandleFunc("POST /upload", s.uploadPost)

	// Root only; `GET /` below is the catch-all for unmatched paths. The
	// `/{$}` pattern wins over `/` for the exact root.
	mux.HandleFunc("GET /{$}", s.galleryHandler)
	mux.HandleFunc("GET /", s.notFoundHandler)

	mux.HandleFunc("GET /i/{sha}", s.imageByHashHandler)
	mux.HandleFunc("GET /images/{id}", s.detailHandler)
	mux.HandleFunc("GET /images/{id}/related", s.relatedImagesHandler)
	mux.HandleFunc("GET /images/{id}/file", s.serveImageFile)
	mux.HandleFunc("GET /images/{id}/view", s.serveImageView)
	mux.HandleFunc("GET /images/{id}/page/{n}", s.serveMangaPage)
	mux.HandleFunc("GET /images/{id}/page/{n}/thumb", s.serveMangaPageThumb)
	mux.HandleFunc("POST /images/{id}/page/{n}/extract", s.extractMangaPage)
	mux.HandleFunc("POST /images/{id}/generate-collection", s.generateMangaCollection)
	mux.HandleFunc("GET /images/{id}/read", s.readerHandler)
	mux.HandleFunc("GET /images/{id}/pages", s.pagesGridHandler)
	mux.HandleFunc("POST /images/{id}/tags", s.addTagToImage)
	mux.HandleFunc("DELETE /images/{id}/tags", s.removeAllTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/user-tags", s.removeUserTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/auto-tags", s.removeAutoTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/source-tags", s.removeSourceTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/stale-tags", s.removeStaleTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/category-tags", s.removeCategoryTagsFromImageHandler)
	mux.HandleFunc("DELETE /images/{id}/source-contribution", s.dropSourceContributionHandler)
	mux.HandleFunc("DELETE /images/{id}/tags/{tagID}", s.removeTagFromImage)
	mux.HandleFunc("POST /images/{id}/favorite", s.toggleFavorite)
	mux.HandleFunc("POST /images/{id}/inbox", s.toggleInbox)
	mux.HandleFunc("DELETE /images/{id}", s.deleteImage)
	mux.HandleFunc("POST /images/{id}/canonical-path", s.promoteCanonical)
	mux.HandleFunc("POST /images/{id}/sources/set", s.setSource)
	mux.HandleFunc("POST /images/{id}/sources/remove", s.removeSource)
	mux.HandleFunc("POST /images/{id}/sources/primary", s.makeSourcePrimary)
	mux.HandleFunc("POST /images/{id}/sources/keep", s.keepLocalFile)
	mux.HandleFunc("POST /images/{id}/annotations/set", s.setAnnotation)
	mux.HandleFunc("POST /images/{id}/annotations/remove", s.removeAnnotation)
	mux.HandleFunc("POST /images/{id}/markup/preview", s.previewMarkup)
	mux.HandleFunc("POST /images/{id}/sources/fetch", s.fetchSource)
	mux.HandleFunc("POST /images/{id}/lookup", s.lookupImage)
	mux.HandleFunc("POST /images/{id}/replace", s.replaceImage)
	mux.HandleFunc("GET /images/{id}/ptr-contrib-panel", s.ptrContribPanel)
	mux.HandleFunc("GET /images/{id}/ptr-contrib-dialog", s.ptrContribDialog)
	mux.HandleFunc("POST /images/{id}/ptr-contrib", s.ptrContribSend)
	mux.HandleFunc("POST /images/{id}/note", s.setNote)
	mux.HandleFunc("POST /internal/batch-lookup/count", s.batchLookupCount)
	mux.HandleFunc("POST /images/{id}/scheduled-lookup", s.scheduledLookupPost)
	mux.HandleFunc("POST /images/{id}/scheduled-lookup/reset", s.scheduledLookupResetPost)
	mux.HandleFunc("POST /images/{id}/commentary/set", s.setSourceCommentary)
	mux.HandleFunc("POST /images/{id}/commentary/remove", s.removeSourceCommentary)
	mux.HandleFunc("POST /images/{id}/original/set", s.setSourceOriginal)
	mux.HandleFunc("POST /images/{id}/original/remove", s.removeSourceOriginal)
	mux.HandleFunc("POST /images/{id}/collections/set", s.setCollection)
	mux.HandleFunc("POST /images/{id}/collections/remove", s.removeCollection)
	mux.HandleFunc("POST /images/{id}/move", s.moveImage)
	mux.HandleFunc("POST /images/{id}/rename", s.renameImage)
	mux.HandleFunc("POST /images/{id}/transfer", s.transferImage)
	mux.HandleFunc("DELETE /images/{id}/aliases/{pathID}", s.deleteAlias)

	mux.HandleFunc("GET /tags", s.tagsHandler)
	mux.HandleFunc("GET /tags/{id}", s.tagDetailHandler)
	mux.HandleFunc("GET /tags/{id}/usage", s.tagUsagePanelHandler)
	mux.HandleFunc("POST /tags/batch-category", s.batchTagCategoryPost)
	mux.HandleFunc("POST /tags/batch-alias", s.batchTagAliasPost)
	mux.HandleFunc("POST /tags/merge-folded", s.batchMergeFoldedPost)
	mux.HandleFunc("POST /tags/batch-imply", s.batchTagImplyPost)
	mux.HandleFunc("POST /tags/new", s.createTagPost)
	mux.HandleFunc("POST /tags/aliases", s.createAliasPost)
	mux.HandleFunc("POST /tags/{id}/rename", s.renameTagPost)
	mux.HandleFunc("DELETE /tags/{id}", s.deleteTagHandler)
	mux.HandleFunc("PATCH /tags/{id}/category", s.changeTagCategory)
	mux.HandleFunc("GET /tags/{id}/implications", s.implicationsDialogHandler)
	mux.HandleFunc("POST /tags/{id}/implications", s.addImplicationPost)
	mux.HandleFunc("POST /tags/{id}/implied-by", s.addImpliedByPost)
	mux.HandleFunc("POST /tags/{id}/aliases", s.addTagAliasPost)
	// The group deletes carry the /group suffix because a bare
	// `DELETE /tags/{id}/aliases` overlaps `DELETE /tags/categories/{id}`
	// with neither pattern more specific, which the mux refuses.
	mux.HandleFunc("DELETE /tags/{id}/implications/group", s.removeImplicationsDelete)
	mux.HandleFunc("DELETE /tags/{id}/implied-by/group", s.removeImpliedByDelete)
	mux.HandleFunc("DELETE /tags/{id}/aliases/group", s.removeTagAliasesDelete)
	mux.HandleFunc("DELETE /tags/{id}/implications/{impliedID}", s.removeImplicationDelete)
	mux.HandleFunc("POST /tags/categories", s.createCategoryPost)
	mux.HandleFunc("POST /tags/categories/{id}/rename", s.renameCategoryPost)
	mux.HandleFunc("DELETE /tags/categories/{id}", s.deleteCategoryDelete)
	mux.HandleFunc("GET /tags/categories/{id}/count", s.categoryCountHandler)

	mux.HandleFunc("GET /collections", s.collectionsHandler)
	mux.HandleFunc("POST /collections/rename", s.renameCollectionPost)
	mux.HandleFunc("POST /collections/dissolve", s.dissolveCollectionPost)
	mux.HandleFunc("POST /collections/find-relations", s.collectionFindRelationsPost)
	mux.HandleFunc("GET /collections/order", s.collectionOrderDialog)
	mux.HandleFunc("POST /collections/order", s.reorderCollectionPost)
	mux.HandleFunc("POST /collections/generate-cbz", s.generateCollectionCBZ)

	mux.HandleFunc("GET /categories", s.categoriesHandler)

	mux.HandleFunc("GET /settings", s.settingsHandler)
	mux.HandleFunc("POST /settings/general", s.settingsGeneralPost)
	mux.HandleFunc("POST /settings/monloader", s.settingsMonloaderPost)
	mux.HandleFunc("POST /settings/tagger", s.settingsTaggerPost)
	mux.HandleFunc("POST /settings/auth/password", s.settingsPasswordPost)
	mux.HandleFunc("POST /settings/auth/remove-password", s.settingsRemovePasswordPost)
	mux.HandleFunc("POST /settings/auth/tokens", s.settingsTokenCreate)
	mux.HandleFunc("DELETE /settings/auth/tokens/{id}", s.settingsTokenRevoke)
	mux.HandleFunc("GET /settings/auth/tokens/{id}/privileges", s.settingsTokenPrivilegesGet)
	mux.HandleFunc("POST /settings/auth/tokens/{id}/privileges", s.settingsTokenPrivilegesPost)
	mux.HandleFunc("PATCH /settings/categories/{id}", s.updateCategoryPatch)
	mux.HandleFunc("POST /settings/schedule", s.settingsSchedulePost)
	mux.HandleFunc("POST /settings/maintenance/prune-missing", s.pruneMissingImagesPost)
	mux.HandleFunc("POST /settings/maintenance/prune-orphaned-thumbnails", s.pruneOrphanedThumbnailsPost)
	mux.HandleFunc("POST /settings/maintenance/recalc-tags", s.recalcTagsPost)
	mux.HandleFunc("POST /settings/maintenance/tag-conflicts", s.tagCategoryConflictsPost)
	mux.HandleFunc("POST /settings/maintenance/find-folded-duplicates", s.findFoldedDuplicatesPost)
	mux.HandleFunc("POST /settings/maintenance/lookup-due", s.lookupDuePost)
	// Relocated to /relations/file-duplicates/* in v1.8; old routes
	// stay alive as 301 redirects for one release so bookmarks survive.
	mux.HandleFunc("GET /settings/maintenance/duplicates-list", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/relations/file-duplicates/list", http.StatusMovedPermanently)
	})
	mux.HandleFunc("POST /settings/maintenance/remove-duplicates", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/relations/file-duplicates/remove", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /relations/file-duplicates/list", s.duplicatesListHandler)
	mux.HandleFunc("POST /relations/file-duplicates/remove", s.removeDuplicatesPost)
	mux.HandleFunc("POST /relations/file-duplicates/promote", s.promoteAliasPathPost)
	mux.HandleFunc("GET /relations/duplicates/sha256", s.sha256WalkerPage)
	mux.HandleFunc("POST /relations/duplicates/sha256/remove-one", s.sha256WalkerRemoveOnePost)
	mux.HandleFunc("GET /relations/duplicates/marked", s.markedWalkerPage)
	mux.HandleFunc("POST /relations/duplicates/marked/delete-one", s.markedWalkerDeleteOnePost)
	mux.HandleFunc("POST /relations/duplicates/marked/delete-all", s.markedWalkerDeleteAllPost)
	mux.HandleFunc("GET /relations", s.relationsPage)
	mux.HandleFunc("GET /relations/browse", s.browseRelationsPage)
	mux.HandleFunc("GET /relations/browse-groups", s.browseGroupsRedirect)
	mux.HandleFunc("GET /relations/session", s.sessionPage)
	mux.HandleFunc("POST /relations/session/decide", s.sessionDecidePost)
	mux.HandleFunc("POST /relations/dup-group/{id}/copy-tags", s.copyTagsToOriginalPost)
	mux.HandleFunc("GET /relations/dup-group/{id}/copy-tags/preview", s.copyTagsToOriginalPreview)
	mux.HandleFunc("POST /settings/relations", s.settingsRelationsPost)
	mux.HandleFunc("POST /settings/maintenance/re-extract-metadata", s.reExtractMetadataPost)
	mux.HandleFunc("POST /settings/maintenance/rebuild-thumbnails", s.rebuildThumbnailsPost)
	mux.HandleFunc("POST /settings/maintenance/compute-hashes", s.computeHashesPost)
	mux.HandleFunc("POST /relations/find-pairs", s.findRelationPairsPost)
	mux.HandleFunc("POST /relations/reset-skipped", s.resetSkippedPost)
	mux.HandleFunc("POST /relations/phash/{id}/recompute", s.recomputePhashPost)
	mux.HandleFunc("POST /relations/add", s.addRelationPost)
	mux.HandleFunc("POST /relations/remove", s.removeRelationPost)
	mux.HandleFunc("POST /relations/reverse", s.reverseRelationPost)
	mux.HandleFunc("POST /relations/browse-groups/merge", s.mergeGroupsPost)
	mux.HandleFunc("POST /relations/browse-groups/dissolve", s.dissolveGroupsPost)
	mux.HandleFunc("GET /internal/images/{id}/md5", s.md5CellGet)
	mux.HandleFunc("GET /internal/images/{id}/related-entries", s.relatedEntriesGet)
	mux.HandleFunc("GET /internal/images/{id}/fetch-status", s.fetchStatusHandler)
	mux.HandleFunc("GET /images/{id}/relations", s.imageRelationsPage)
	mux.HandleFunc("POST /settings/maintenance/vacuum-db", s.vacuumDBPost)
	mux.HandleFunc("POST /settings/maintenance/free-memory", s.freeMemoryPost)
	mux.HandleFunc("POST /settings/tagger/{name}/enable", s.settingsTaggerEnablePost)
	mux.HandleFunc("POST /settings/tagger/{name}/disable", s.settingsTaggerDisablePost)
	mux.HandleFunc("POST /settings/tagger/{name}/delete", s.settingsTaggerDeletePost)
	mux.HandleFunc("GET /settings/tagger/{name}/config", s.settingsTaggerConfigGet)
	mux.HandleFunc("POST /settings/tagger/{name}/config", s.settingsTaggerConfigPost)
	mux.HandleFunc("GET /settings/tagger/{name}/labels", s.settingsTaggerLabelsGet)
	mux.HandleFunc("POST /settings/tagger/{name}/mapping", s.settingsTaggerMappingPost)
	mux.HandleFunc("POST /settings/tagger/{name}/reset", s.settingsTaggerResetPost)

	// Saved searches are managed from the sidebar (no dedicated search page).
	mux.HandleFunc("POST /search/saved", s.createSavedSearch)
	mux.HandleFunc("DELETE /search/saved/{id}", s.deleteSavedSearch)

	mux.HandleFunc("GET /internal/job/status", s.jobStatusHandler)
	mux.HandleFunc("GET /internal/monloader-status", s.monloaderStatusHandler)
	mux.HandleFunc("POST /internal/job/dismiss", s.jobDismissPost)
	mux.HandleFunc("POST /internal/job/cancel", s.jobCancelPost)
	mux.HandleFunc("POST /internal/sync", s.syncTrigger)
	mux.HandleFunc("POST /internal/autotag", s.autotagTrigger)
	mux.HandleFunc("POST /internal/batch-delete", s.batchDelete)
	mux.HandleFunc("POST /internal/batch-move", s.batchMove)
	mux.HandleFunc("POST /internal/batch-rename", s.batchRename)
	mux.HandleFunc("POST /internal/batch-transfer", s.batchTransfer)
	mux.HandleFunc("POST /internal/batch-tag", s.batchTag)
	mux.HandleFunc("POST /internal/batch-strip", s.batchStrip)
	mux.HandleFunc("POST /internal/batch-inbox", s.batchInbox)
	mux.HandleFunc("POST /internal/batch-favorite", s.batchFavorite)
	mux.HandleFunc("POST /internal/batch-collection", s.batchCollection)
	mux.HandleFunc("POST /internal/batch-lookup", s.batchLookup)
	mux.HandleFunc("POST /internal/delete-search", s.deleteSearchPost)
	mux.HandleFunc("POST /tags/delete-search", s.deleteTagsSearchPost)
	mux.HandleFunc("POST /tags/ptr-lookup-search", s.ptrLookupSearchPost)
	mux.HandleFunc("GET /tags/{id}/ptr-contrib-panel", s.tagPtrContribPanel)
	mux.HandleFunc("GET /tags/{id}/ptr-contrib-dialog", s.tagPtrContribDialog)
	mux.HandleFunc("POST /tags/{id}/ptr-contrib", s.tagPtrContribSend)
	mux.HandleFunc("GET /tags/{id}/ptr-lookup-dialog", s.tagPtrLookupDialog)
	mux.HandleFunc("GET /tags/{id}/ptr-lookup-preview", s.tagPtrLookupPreview)
	mux.HandleFunc("GET /tags/{id}/ptr-lookup-search", s.tagPtrLookupSearch)
	mux.HandleFunc("POST /tags/{id}/ptr-lookup", s.ptrLookupTagPost)
	mux.HandleFunc("POST /internal/delete-folder", s.deleteFolderPost)
	mux.HandleFunc("GET /internal/tags/suggest", s.tagSuggest)
	mux.HandleFunc("GET /internal/search/suggest", s.searchSuggest)
	mux.HandleFunc("GET /internal/folders/suggest", s.foldersSuggest)
	mux.HandleFunc("GET /internal/collection/suggest", s.collectionSuggest)
	mux.HandleFunc("GET /internal/source/suggest", s.sourceSuggest)
	mux.HandleFunc("GET /internal/name/preview", s.namePreview)
	mux.HandleFunc("GET /internal/sidebar", s.gallerySidebar)
	mux.HandleFunc("GET /internal/sidebar-browse", s.sidebarBrowse)
	mux.HandleFunc("POST /internal/rating-ceiling", s.ratingCeilingPost)
	mux.HandleFunc("POST /images/{id}/autotag", s.autotagImage)
	mux.HandleFunc("GET /images/{id}/tags", s.getImageTagsHandler)

	mux.HandleFunc("POST /internal/gallery/switch", s.gallerySwitchHandler)
	mux.HandleFunc("POST /settings/galleries", s.settingsGalleriesPost)
	mux.HandleFunc("POST /settings/galleries/{name}/rename", s.settingsGalleryRenamePost)
	mux.HandleFunc("POST /settings/galleries/{name}/delete", s.settingsGalleryDeletePost)
	mux.HandleFunc("POST /settings/galleries/{name}/default", s.settingsGalleryDefaultPost)
	mux.HandleFunc("GET /settings/galleries/{name}/export", s.settingsGalleryExport)
	mux.HandleFunc("POST /settings/galleries/{name}/import", s.settingsGalleryImport)

	mux.HandleFunc("POST /api/v1/pair/request", s.pairRequest)
	mux.HandleFunc("GET /api/v1/pair/status", s.pairStatus)
	mux.HandleFunc("POST /api/v1/pair/remove", s.pairTeardown)
	mux.HandleFunc("GET /internal/plugins/pairing", s.pluginPairingFragment)
	mux.HandleFunc("POST /settings/plugins/pair/{id}/approve", s.pluginPairApprove)
	mux.HandleFunc("POST /settings/plugins/pair/{id}/deny", s.pluginPairDeny)
	mux.HandleFunc("POST /settings/plugins/{name}/remove", s.pluginPairRemove)
	mux.HandleFunc("POST /settings/plugins/{name}/pause", s.pluginPause)
	mux.HandleFunc("POST /settings/plugins/{name}/start", s.pluginStart)
	mux.HandleFunc("POST /settings/plugins/{name}/stop", s.pluginStop)
	mux.HandleFunc("POST /settings/plugins/theme", s.settingsThemePost)
	mux.HandleFunc("POST /internal/monloader/disconnect", s.monloaderLightDisconnect)
	mux.HandleFunc("POST /internal/monloader/reconnect", s.monloaderLightReconnect)
	mux.HandleFunc("POST /internal/plugin/relay", s.pluginRelay)
	// What a page needs: its own GETs (HEAD rides along) and its form posts.
	mux.HandleFunc("GET "+pluginMountPrefix+"{name}/", s.pluginMount)
	mux.HandleFunc("POST "+pluginMountPrefix+"{name}/", s.pluginMount)

	api.New(s.cfg, &s.cfgMu, s.jobs, s.apiResolver, Version).Mount(mux)

	// Middleware order, outermost first: logging, context (RLock), session, CSRF.
	var h http.Handler = mux
	h = s.CSRFMiddleware(h)
	h = s.SessionMiddleware(h)
	h = s.ContextMiddleware(h)
	h = loggingMiddleware(h)

	return h
}

// allContexts snapshots every open gallery under the context lock. The
// copy is the point: the callers walk it doing DB work, and holding the
// lock across that would block a gallery switch for the length of the
// walk.
func (s *Server) allContexts() []*galleryCtx {
	s.ctxMu.RLock()
	defer s.ctxMu.RUnlock()
	out := make([]*galleryCtx, 0, len(s.contexts))
	for _, cx := range s.contexts {
		out = append(out, cx)
	}
	return out
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
		Handle:           cx.Handle,
		RelationsSvc:     cx.RelationsSvc,
		InvalidateCaches: cx.InvalidateCaches,
		RecordFetch:      func(id int64, state, msg string) { s.recordFetchStatus(cx.Name, id, state, msg) },
		VisibleCount:     cx.VisibleCount,
		TagCount:         cx.TagCount,
	}, true
}

// isNoisyPath reports paths that are requested constantly (polling, static
// assets, thumbnails, health probes). They log at debug so the default info
// level stays readable.
func isNoisyPath(path string) bool {
	switch path {
	case "/internal/job/status", "/internal/monloader-status", "/health":
		return true
	}
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/thumbnails/")
}

// requestStartKey carries the wall-clock time at which the outermost
// middleware first saw the request. base() reads it back so the footer's
// "page loaded in N ms" reflects everything between the request hitting
// our handler chain and the layout footer rendering, not just the
// handler's tail end.
type requestStartKey struct{}

func requestStartFromContext(ctx context.Context) time.Time {
	if t, ok := ctx.Value(requestStartKey{}).(time.Time); ok {
		return t
	}
	return time.Time{}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), requestStartKey{}, time.Now()))
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
var RepoURL = "https://github.com/monbooru/monbooru"

// DocURL is the online documentation URL, set at build time via -ldflags from
// DOC.md.
var DocURL = "https://monbooru.github.io/mondocs/index.html"

// Variant identifies the build flavour (e.g. "cuda") and is injected at
// build time via -ldflags from the CUDA Dockerfile. Empty for the default
// CPU build; rendered in parentheses in the footer when non-empty.
var Variant = ""

// ratingLevel is one cell in the footer rating selector. Value is the
// underlying tag name (used as the cookie value and the AST key); Label
// is the user-facing text - "general" renders as "sfw" so the toggle
// doesn't lead with the implicit-default name.
type ratingLevel struct {
	Value string
	Label string
}

// ratingFooterLevels is the fixed left-to-right footer order, low to
// high. Active is "explicit" when the cookie is unset (no ceiling).
var ratingFooterLevels = []ratingLevel{
	{Value: "general", Label: "sfw"},
	{Value: "sensitive", Label: "sensitive"},
	{Value: "questionable", Label: "questionable"},
	{Value: "explicit", Label: "explicit"},
}

// baseData is common template data present on every page.
type baseData struct {
	Title       string
	ActiveNav   string
	CSRFToken   string
	AuthEnabled bool
	Degraded    bool
	Version     string
	RepoURL     string
	DocURL      string
	Variant     string
	CustomCSS   bool
	// Theme is true while an operator-installed theme resolves, gating the
	// /theme.css link between the bundled sheet and the operator's own.
	Theme bool
	// SidebarCollapsed hides the sidebar column on the layout's first paint.
	// Rendered server-side so a navigation doesn't flash the column in and
	// back out once main.js runs.
	SidebarCollapsed bool
	// BooruName is the operator's brand override (or "Monbooru" by
	// default). Rendered into every page <title>, the topbar wordmark,
	// and the login screen so a deployment that wants a different name
	// only edits monbooru.toml.
	BooruName string
	// BooruLogo is the resolved URL for the topbar logo: "/custom.logo"
	// when server.logo is set, the active theme's "/theme.logo" when it
	// ships one, the bundled logo.png otherwise.
	// BooruFavicon is the same for the favicon <link>, minus the theme
	// rung, falling back to the bundled favicon.png. A configured
	// server.logo drives both; a theme only moves the topbar.
	BooruLogo    string
	BooruFavicon string
	// MonloaderURL is the browser-facing monloader base for the footer
	// "connected to monloader" link, trailing slash trimmed; falls back to
	// the api url when unset, so only both being unset drops the link.
	MonloaderURL  string
	ActiveGallery string
	Galleries     []config.Gallery
	// Counts surfaced on the footer status bar. Populated per-request;
	// zero when the active gallery is missing or a query failed.
	VisibleCount     int
	InboxCount       int
	TagCount         int
	CollectionsCount int
	// InboxNavActive marks the top-nav "Inbox" entry as the active
	// page when the current URL's `q` parameter positively asserts
	// inbox:true at the top level. Same parser-based gate the inline
	// upload drop zone uses.
	InboxNavActive bool
	// HiddenByCeiling drives the "N hidden images in the current search"
	// footer cell. Only the gallery handler populates it; on every other
	// page the field stays at 0 and the cell renders empty.
	HiddenByCeiling int
	// Rating ceiling state for the footer selector. ActiveRating is the
	// effective level - "explicit" when no cookie is set.
	RatingLevels []ratingLevel
	ActiveRating string
	// RequestStart is the wall-clock time captured by loggingMiddleware
	// when the request first entered our handler chain. The footer
	// renders time.Since(RequestStart) so the indicator covers all
	// middleware + handler work + template execution, not just the
	// tail-end after base() runs.
	RequestStart time.Time
	// MonloaderPaired gates the footer "connected to monloader" light:
	// it renders (and starts polling) only while a monloader pairing exists.
	MonloaderPaired bool
	// MonloaderUsable gates the monloader-backed actions (online lookup, find
	// tags, source refetch): paired and the link is neither paused nor a probe
	// found it unreachable / rejecting.
	MonloaderUsable bool
	// MonloaderConn / MonloaderVersion seed the light on the initial render;
	// the poller swaps in live values. They live here so the partial resolves
	// on every page struct, not just the poll handler's map.
	MonloaderConn    string
	MonloaderVersion string
	// MonloaderPTR gates the PTR-backed lookup controls: true when the last
	// cached probe saw monloader report its PTR index enabled and caught up,
	// the only state it answers a read in. Stale reads are fine - monloader
	// answers 409 and the UI degrades in place.
	MonloaderPTR bool
	// MonloaderContrib gates every PTR contribution surface: true when the
	// last cached probe saw monloader report a usable personal account
	// (contrib.account && !contrib.banned). An absent contrib field on an
	// older monloader reads as false, so contribution UI never renders
	// against a monloader that can't serve it. Stale reads degrade in
	// place on the 409, like the lookup gating.
	MonloaderContrib bool
	// MonloaderPTRSyncing caveats the lookup backend dialog while the PTR
	// index is still building: it answers on partial data by design.
	MonloaderPTRSyncing bool
	// MonloaderPTRPresent gates the PTR-backed surfaces' render, where
	// MonloaderPTR gates whether they are live. A paused or unreachable
	// link reports no PTR either way, so the flag has to say "and we
	// cannot currently tell" or the surfaces vanish on a pause.
	MonloaderPTRPresent bool
}

// sidebarCookieName records the collapsed sidebar. The topbar toggle in
// main.js writes it; only the layout reads it.
const sidebarCookieName = "monbooru_sidebar"

// sidebarCollapsed reports whether the operator hid the sidebar. Anything
// other than the one stored value reads as shown, so a stale or
// hand-edited cookie can't leave the column missing with no way back.
func sidebarCollapsed(r *http.Request) bool {
	c, err := r.Cookie(sidebarCookieName)
	return err == nil && c.Value == "collapsed"
}

func (s *Server) base(r *http.Request, nav, title string) baseData {
	sessID := sessionFromContext(r.Context())
	cx := s.contexts[s.activeName] // ctxMu RLocked by ContextMiddleware
	degraded := false
	visible, inbox, tagCount, collectionsCount := 0, 0, 0, 0
	if cx != nil {
		degraded = cx.Degraded
		visible, _ = cx.VisibleCount()
		// Inbox count is ceiling-aware here because every surface that
		// renders it (top-nav "Inbox (N)" link, inline drop zone, search
		// suggestions) promises the post-click match count.
		inbox, _ = cx.InboxCountUnder(resolveCeiling(r, cx))
		tagCount, _ = cx.TagCount()
		collectionsCount, _ = cx.CollectionsCount()
	}
	inboxNavActive := false
	if expr, parseErr := search.Parse(r.URL.Query().Get("q")); parseErr == nil {
		inboxNavActive = inboxFilterActive(expr)
	}
	galleries := s.galleries()
	active := readRatingCookie(r)
	active = cmp.Or(active, "explicit")
	conn, connVer, ptrReady, ptrSyncing, ptrContrib := s.monloaderStatusSeed()
	if s.monloaderPaused() {
		// A paused link renders as paused everywhere and hides the
		// PTR-gated surfaces, regardless of the last probe's cache.
		conn, connVer, ptrReady, ptrSyncing, ptrContrib = "paused", "", false, false, false
	}
	paired := s.pairedWith("monloader")
	monloaderUsable := s.monloaderUsable()
	themeSheet := s.activeTheme().Path != ""
	return baseData{
		Title:               title,
		ActiveNav:           nav,
		CSRFToken:           s.csrfToken(sessID),
		AuthEnabled:         s.authEnabled(),
		Degraded:            degraded,
		Version:             Version,
		RepoURL:             RepoURL,
		DocURL:              DocURL,
		Variant:             Variant,
		CustomCSS:           s.customCSSPath() != "",
		Theme:               themeSheet,
		SidebarCollapsed:    sidebarCollapsed(r),
		BooruName:           s.booruName(),
		BooruLogo:           s.booruLogoURL(),
		BooruFavicon:        s.booruFaviconURL(),
		MonloaderURL:        s.monloaderWebBase(),
		MonloaderPaired:     paired,
		MonloaderUsable:     monloaderUsable,
		MonloaderConn:       conn,
		MonloaderVersion:    connVer,
		MonloaderPTR:        ptrReady,
		MonloaderPTRSyncing: ptrSyncing,
		MonloaderPTRPresent: paired && (ptrReady || ptrSyncing || !monloaderUsable),
		MonloaderContrib:    ptrContrib,
		ActiveGallery:       s.activeName,
		Galleries:           galleries,
		VisibleCount:        visible,
		InboxCount:          inbox,
		TagCount:            tagCount,
		CollectionsCount:    collectionsCount,
		InboxNavActive:      inboxNavActive,
		RatingLevels:        ratingFooterLevels,
		ActiveRating:        active,
		RequestStart:        requestStartFromContext(r.Context()),
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
	// absent for recently-ingested animated files; static thumbs are absent
	// when generation failed on an undecodable file. Respond 204 so the img
	// tag's onerror still fires but the console doesn't log a 404 per card.
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// SQLite reuses the highest deleted INTEGER PRIMARY KEY id, so a
	// deleted image's URL can be reborn the next ingest with brand-new
	// thumbnail bytes at the same path. Without revalidation the browser
	// keeps serving the prior bytes from cache (heuristic freshness on a
	// bare Last-Modified). The ETag includes the file mtime so a rewrite
	// invalidates the cached response; same trick serveImageFile uses.
	setGalleryScopedCache(w, r.PathValue("gallery"), file, fullPath)
	http.ServeFile(w, r, fullPath)
}

// serveConfiguredFile serves an operator-supplied file (the
// server.custom_css stylesheet, the server.logo logo/favicon). An empty
// config 404s so the layout's gated <link> and the bundled-asset
// fallbacks degrade cleanly when the knob is not set. Path scope is
// enforced at config load (see customCSSPathAllowed) so any leak vector
// is closed before this handler ever runs. The cache tag revalidates
// against the file mtime so an edited file is picked up at once; a bare
// Last-Modified would go heuristically stale until the operator
// disabled the browser cache.
func (s *Server) serveConfiguredFile(w http.ResponseWriter, r *http.Request, path, kind string) {
	if path == "" {
		http.NotFound(w, r)
		return
	}
	setGalleryScopedCache(w, "custom", kind, path)
	http.ServeFile(w, r, path)
}

func (s *Server) serveCustomCSS(w http.ResponseWriter, r *http.Request) {
	s.serveConfiguredFile(w, r, s.customCSSPath(), "css")
}

func (s *Server) serveCustomLogo(w http.ResponseWriter, r *http.Request) {
	s.serveConfiguredFile(w, r, s.customLogoPath(), "logo")
}

// booruName resolves server.name with a "Monbooru" fallback so every
// title-suffix callsite reads a single source of truth instead of
// repeating the default.
func (s *Server) booruName() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if name := s.cfg.Server.BooruName; name != "" {
		return name
	}
	return "Monbooru"
}

// modelPath reads paths.model_path under the config lock. Fixed after
// boot, but every tagger handler reaches for it and one lock discipline
// beats nine open-coded reads.
func (s *Server) modelPath() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Paths.ModelPath
}

// The settings handlers rewrite these fields at runtime under the write
// lock, so every serving path reads them under the read lock - through one
// of the accessors below, or through a caller that takes several at once
// (ingestNaming, receivedNaming). A string field is two words, and a torn
// read of one yields a slice header that never existed.

func (s *Server) authEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.EnablePassword
}

func (s *Server) passwordHash() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.PasswordHash
}

func (s *Server) sessionLifetimeDays() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.SessionLifetimeDays
}

func (s *Server) pageSize() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.UI.PageSize
}

func (s *Server) thumbnailFit() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.UI.ThumbnailFit
}

func (s *Server) maxFileSizeMB() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Gallery.MaxFileSizeMB
}

// watcherSettings pairs the two knobs every startWatcher call passes, so
// a save landing between them cannot start a watcher on half of one.
func (s *Server) watcherSettings() (bool, int) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB
}

func (s *Server) customCSSPath() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Server.CustomCSS
}

func (s *Server) customLogoPath() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Server.BooruLogo
}

func (s *Server) themeColor() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Server.ThemeColor
}

func (s *Server) defaultGallery() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.DefaultGallery
}

func (s *Server) derivePaths(name string) (string, string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.DerivePaths(name)
}

func (s *Server) executionProvider() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Tagger.ExecutionProvider
}

// cfgSnapshot copies the config for the tagger package, which takes the
// whole struct and reads it for the length of a job. The copy is shallow:
// its slices are the live headers, read under the lock, and the settings
// writers replace those slices rather than growing them in place.
func (s *Server) cfgSnapshot() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	c := *s.cfg
	return &c
}

// galleries copies cfg.Galleries under the lock its mutators take. A
// slice-header read torn against a reallocating append pairs the old
// array pointer with the new length, so the copy walks off the end of
// the old backing array and hands garbage strings to a template.
func (s *Server) galleries() []config.Gallery {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	out := make([]config.Gallery, len(s.cfg.Galleries))
	copy(out, s.cfg.Galleries)
	return out
}

// customLogoURL is the operator's server.logo override as a URL, empty
// when the knob is unset. It outranks the active theme on every branded
// surface: a config the operator wrote by hand beats one a theme folder
// brought in.
func (s *Server) customLogoURL() string {
	if s.customLogoPath() != "" {
		return "/custom.logo"
	}
	return ""
}

// booruLogoURL points the topbar logo at server.logo, else the active
// theme's logo.png, else the bundled asset.
func (s *Server) booruLogoURL() string {
	if url := s.customLogoURL(); url != "" {
		return url
	}
	if s.activeTheme().Logo != "" {
		return "/theme.logo"
	}
	return "/static/logo.png"
}

// booruFaviconURL is the same for the favicon (and, through it, the
// manifest icon), except that a theme does not reach it: a theme's
// logo.png is drawn for the topbar, and at 16px in a tab it reduces to
// mush. Only server.logo - which the operator picked knowing both
// surfaces use it - replaces the bundled icon.
func (s *Server) booruFaviconURL() string {
	if url := s.customLogoURL(); url != "" {
		return url
	}
	return "/static/favicon.png"
}

// monloaderWebBase is the browser-facing monloader base for the footer
// "connected to monloader" link: the configured web url when set, else the api url.
func (s *Server) monloaderWebBase() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	base := s.cfg.Server.MonloaderURL
	base = cmp.Or(base, s.cfg.Monloader.APIURL)
	return strings.TrimRight(base, "/")
}

// uppercasePercentEscapes rewrites every %XX hex pair in s to use
// uppercase hex while leaving all other characters untouched. Used to
// align url.QueryEscape's lowercase output with the browser address
// bar's RFC 3986 normalization so the same logical query doesn't show
// up twice in autocomplete history.
func uppercasePercentEscapes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			b.WriteByte('%')
			b.WriteByte(toUpperHex(s[i+1]))
			b.WriteByte(toUpperHex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func toUpperHex(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 'A'
	}
	return c
}

// customCSSPathAllowed gates the operator-supplied path to a small set
// of trusted root directories. The intent is to catch a misconfigured
// CustomCSS like "/etc/passwd" before /custom.css can leak it; legit
// uses (a CSS file alongside the config or under /config or /data) all
// pass without further setup. EvalSymlinks runs before the containment
// check so a symlink under a trusted root that points at a file
// outside (e.g. /config/style.css → /etc/passwd) fails the gate.
func customCSSPathAllowed(cssPath, configPath string) bool {
	if cssPath == "" {
		return true
	}
	abs, err := filepath.Abs(cssPath)
	if err != nil {
		return false
	}
	// EvalSymlinks fails when the target doesn't exist yet; fall back
	// to the cleaned absolute path in that case so operator misspellings
	// still fail the file-serve below (rather than appearing to pass
	// the gate because the symlink check errored).
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = resolved
	}
	roots := []string{"/config", "/data"}
	if configPath != "" {
		if cfgAbs, err := filepath.Abs(filepath.Dir(configPath)); err == nil {
			roots = append(roots, cfgAbs)
		}
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		// Evaluate symlinks on the root too so a containerised /config
		// that resolves to /var/lib/monbooru/config still matches the
		// resolved file path.
		if resolved, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
			root = resolved
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

// resolveMangaImage looks up a manga row's canonical_path. Returns
// (path, true) when the row is a cbz; (_, false) for non-manga ids and
// missing rows. Callers respond 404 on the false return.
func (s *Server) resolveMangaImage(idStr string) (string, bool) {
	cx := s.Active()
	if cx == nil {
		return "", false
	}
	var canonPath, fileType string
	if err := cx.DB.Read.QueryRow(
		`SELECT canonical_path, file_type FROM images WHERE id = ?`, idStr,
	).Scan(&canonPath, &fileType); err != nil {
		return "", false
	}
	if fileType != "cbz" {
		return "", false
	}
	// Refuse a canonical_path that drifted outside the gallery root before
	// the archive extractor opens it, mirroring serveImageFile.
	if !gallery.ResolvedInside(cx.GalleryPath, canonPath) {
		return "", false
	}
	return canonPath, true
}

// serveMangaPage serves the n-th page of a manga (1-based) from the
// per-image cache, extracting on miss. Cache-Control fixes browser
// behavior under prefetch / back-button so the same page isn't refetched
// constantly during reader navigation.
func (s *Server) serveMangaPage(w http.ResponseWriter, r *http.Request) {
	s.serveMangaPagePath(w, r, gallery.EnsureMangaPage, "")
}

// serveMangaPageThumb serves the n-th page's thumbnail (300px-longest-
// side JPEG) used by the pages-grid view. Same lazy-extract +
// idle-evict path as serveMangaPage.
func (s *Server) serveMangaPageThumb(w http.ResponseWriter, r *http.Request) {
	s.serveMangaPagePath(w, r, gallery.EnsureMangaPageThumb, "-thumb")
}

// serveMangaPagePath is the shared body behind serveMangaPage and
// serveMangaPageThumb. cacheSuffix keeps the thumb-vs-bytes cache keys
// disjoint so a gallery switch invalidates each independently.
func (s *Server) serveMangaPagePath(
	w http.ResponseWriter, r *http.Request,
	ensure func(thumbnailsPath, canonPath string, imageID int64, n int) (string, error),
	cacheSuffix string,
) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	canonPath, ok := s.resolveMangaImage(idStr)
	if !ok {
		http.NotFound(w, r)
		return
	}
	page, err := ensure(s.thumbnailsPath(), canonPath, id, n)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Scope the cached bytes to the active gallery so a gallery switch
	// invalidates them; see serveImageFile for the same trick.
	setGalleryScopedCache(w, s.activeName, fmt.Sprintf("%s-%d%s", idStr, n, cacheSuffix), page)
	http.ServeFile(w, r, page)
}

// serveImageFile serves the raw image/video file.
func (s *Server) serveImageFile(w http.ResponseWriter, r *http.Request) {
	s.serveImageBytes(w, r, false)
}

// serveImageView is what every viewer in the UI points at: the original
// bytes, or - for a still past the display ceiling, which no browser will
// decode - the bounded rendition instead. One endpoint rather than a
// template branch so the lightbox and the relations surfaces, which build
// their src in JS and hold no dimensions, get the same answer. The download
// link and the <video> src stay on /file: those are the bytes themselves.
func (s *Server) serveImageView(w http.ResponseWriter, r *http.Request) {
	s.serveImageBytes(w, r, true)
}

func (s *Server) serveImageBytes(w http.ResponseWriter, r *http.Request, scaled bool) {
	idStr := r.PathValue("id")
	cx := s.Active()
	if cx == nil {
		http.NotFound(w, r)
		return
	}
	var id int64
	var canonPath, fileType string
	var width, height sql.NullInt64
	if err := cx.DB.Read.QueryRow(
		`SELECT id, canonical_path, file_type, width, height FROM images WHERE id = ?`, idStr,
	).Scan(&id, &canonPath, &fileType, &width, &height); err != nil {
		http.NotFound(w, r)
		return
	}
	// Ensure resolved path is within the gallery directory to prevent serving
	// arbitrary files. Use filepath.Rel so a sibling directory that shares a
	// literal prefix with the gallery root (e.g. `/data/gallery_backup` vs
	// `/data/gallery`) is correctly rejected.
	if !gallery.ResolvedInside(cx.GalleryPath, canonPath) {
		http.NotFound(w, r)
		return
	}
	// /images/{id}/file is the same URL across galleries, so the
	// browser's cache key alone can't tell them apart - switching
	// galleries used to keep showing the prior gallery's id=N bytes
	// until a hard reload. Set an ETag that names the active gallery
	// so the conditional check (http.serveContent uses If-None-Match)
	// invalidates on a gallery switch even when mtimes happen to
	// match. no-cache forces revalidation on every visit so the
	// matching gallery still hits 304.
	if scaled && !gallery.IsVideoType(fileType) && fileType != "cbz" &&
		gallery.NeedsViewRendition(int(width.Int64), int(height.Int64)) {
		rendition, err := gallery.EnsureViewRendition(canonPath, cx.ThumbnailsPath, id)
		if err != nil {
			logx.Warnf("view rendition for image %s: %v", idStr, err)
		} else {
			setGalleryScopedCache(w, s.activeName, idStr+"-view", rendition)
			w.Header().Set("Content-Type", "image/jpeg")
			http.ServeFile(w, r, rendition)
			return
		}
	}
	setGalleryScopedCache(w, s.activeName, idStr, canonPath)
	if ct := gallery.MIMEForFileType(fileType); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition", gallery.ContentDispositionFor(canonPath))
	http.ServeFile(w, r, canonPath)
}

// setGalleryScopedCache writes an ETag that includes the gallery name
// so a browser's cached copy from a different gallery's id=N is
// invalidated on the next conditional request. Falls back silently if
// the file can't be stat'd. The mtime is nanoseconds: a plugin turning
// an image twice inside one second writes two different files, and at
// whole-second resolution the second one rides the first one's tag and
// the browser keeps painting the bytes it already has.
func setGalleryScopedCache(w http.ResponseWriter, gallery, id, path string) {
	w.Header().Set("Cache-Control", "private, no-cache")
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%s-%d"`, gallery, id, info.ModTime().UnixNano()))
}

// Close stops background goroutines and closes every gallery's database.
func (s *Server) Close() {
	select {
	case <-s.done:
		// already closed
	default:
		close(s.done)
	}
	s.pluginSupervisor.StopAll()
	tagger.ReleaseAll()
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	for _, cx := range s.contexts {
		cx.close()
	}
}

// withConfig mutates the in-memory config under the write lock and persists it
// atomically, so a read-modify-write on a config slice can't lose a concurrent
// settings change. A non-nil fn error aborts the save.
func (s *Server) withConfig(fn func(*config.Config) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := fn(s.cfg); err != nil {
		return err
	}
	if err := config.Save(s.cfg, s.configPath); err != nil {
		logx.Errorf("config save: %v", err)
		return err
	}
	return nil
}

// saveConfig persists the config as it stands, for callers that have
// already made their edit. Returns any error so they can surface the
// failure instead of leaving the in-memory cfg out of sync with disk.
func (s *Server) saveConfig() error {
	return s.withConfig(func(*config.Config) error { return nil })
}
