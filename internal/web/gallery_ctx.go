package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/counts"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
)

// galleryCtx holds everything per-gallery: paths, DB, tag service, degraded
// flag, and watcher bookkeeping.
type galleryCtx struct {
	// The five fields both transports describe identically. Embedded so a
	// reader still writes cx.DB / cx.GalleryPath, and so the API resolver
	// hands the same value across instead of rebuilding it field by field.
	gallery.Handle

	RelationsSvc *relations.Service
	Degraded     bool

	// GeneralCategoryID is the resolved id of the built-in `general`
	// tag_categories row. parseTagInput hits this every detail-page tag
	// add, every upload, every batch-tag job; resolving once at open
	// time saves a SELECT per call. Built-in rows are immutable so no
	// invalidation is needed.
	GeneralCategoryID int64

	// Per-gallery caches of queries that scan every visible image. All are
	// nilled by InvalidateCaches after any ingest/delete/missing-toggle so
	// the next reader re-populates from SQLite. The int counts are pointers
	// so "not cached" is distinguishable from "cached zero".
	folderTree        atomic.Pointer[[]gallery.FolderNode]
	sourceLabelCounts atomic.Pointer[[]gallery.SourceLabelCount]
	visibleCount      atomic.Pointer[int]
	inboxCount        atomic.Pointer[int]
	tagCount          atomic.Pointer[int]
	collectionsCount  atomic.Pointer[int]
	phashMissing      atomic.Pointer[int]

	// Parallel caches keyed by ceiling level, populated lazily on first
	// access from a sidebar / relations-hub render under that ceiling
	// and dropped together with the blind caches on InvalidateCaches.
	// The maps are stored by atomic pointer so reads are lock-free; the
	// helper below performs copy-on-write to add a level. Three active
	// ceilings × N galleries at steady state is trivial storage.
	inboxCountUnder        atomic.Pointer[map[string]int]
	phashMissingUnder      atomic.Pointer[map[string]int]
	folderTreeUnder        atomic.Pointer[map[string][]gallery.FolderNode]
	sourceLabelCountsUnder atomic.Pointer[map[string][]gallery.SourceLabelCount]

	// bkTree is the in-memory phash index used by the find-pairs job
	// and the phash: search keyword. Built lazily on first relations
	// query; the ingest/delete hooks in internal/relations keep it
	// consistent with subsequent writes once it is built.
	bkTree *relations.BKTree

	watcherCancel context.CancelFunc
	watcherDone   chan struct{}

	mangaReclaim *gallery.MangaCacheReclaimer
}

// Sync runs gallery.Sync against this context and drops the per-cx
// caches that the sync's mark-missing / move / ingest steps touch.
// Centralising the invalidation here keeps the contract local: every
// caller (manual sync handler, scheduler, future scheduled phase) gets
// the cache hygiene by construction instead of relying on the caller's
// goroutine to remember the InvalidateCaches at the right point.
func (cx *galleryCtx) Sync(ctx context.Context, maxFileSizeMB int, naming gallery.Naming, progress func(processed, total int, message string)) (gallery.SyncResult, error) {
	result, err := gallery.Sync(ctx, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, maxFileSizeMB, naming, progress)
	cx.InvalidateCaches()
	return result, err
}

// requireActive returns the active gallery context, or writes a 503
// "no gallery" and returns false. Callers must `return` on a false
// result. Use this for any handler whose work can't proceed without a
// live DB; sub-service guards (RelationsSvc==nil, bkTree==nil) still
// belong inline because they check different fields.
func (s *Server) requireActive(w http.ResponseWriter) (*galleryCtx, bool) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return nil, false
	}
	return cx, true
}

// InvalidateCaches drops the folder-tree and visible-count caches. Call after
// any mutation that changes which images are visible (ingest, delete, sync
// mark-missing, watcher remove). Tag and collection counts are dropped too:
// the same image-mutation paths typically change those, and the Settings
// page's per-gallery cells need them fresh.
func (cx *galleryCtx) InvalidateCaches() {
	if cx == nil {
		return
	}
	cx.folderTree.Store(nil)
	cx.sourceLabelCounts.Store(nil)
	cx.visibleCount.Store(nil)
	cx.inboxCount.Store(nil)
	cx.tagCount.Store(nil)
	cx.collectionsCount.Store(nil)
	cx.phashMissing.Store(nil)
	cx.inboxCountUnder.Store(nil)
	cx.phashMissingUnder.Store(nil)
	cx.folderTreeUnder.Store(nil)
	cx.sourceLabelCountsUnder.Store(nil)
	if cx.DB != nil {
		counts.Invalidate(cx.DB)
	}
	// The adjacency cache holds sorted match-id snapshots that pre-date
	// any membership-changing write (delete, move, inbox/favourite
	// toggle, batch tag). Without dropping them here, a re-render of
	// the same query within the 5-min TTL serves the stale list and
	// the gallery shows rows that no longer match.
	search.AdjacencyCacheDropForGallery(cx.Name)
}

// cachedValue is cachedCount for arbitrary types: load slot or run
// query and store on success.
func cachedValue[V any](slot *atomic.Pointer[V], query func() (V, error)) (V, error) {
	if p := slot.Load(); p != nil {
		return *p, nil
	}
	v, err := query()
	if err != nil {
		var zero V
		return zero, err
	}
	slot.Store(&v)
	return v, nil
}

// FolderTree returns the cached tree or builds one on demand. The cache is
// invalidated by InvalidateCaches.
func (cx *galleryCtx) FolderTree() ([]gallery.FolderNode, error) {
	return cachedValue(&cx.folderTree, func() ([]gallery.FolderNode, error) { return gallery.FolderTree(cx.DB) })
}

// SourceLabelCounts returns the cached top-25 source labels for the
// gallery's non-missing image rows. Empty when no image carries a
// source label; the sidebar partial gates rendering on the slice
// being non-empty.
func (cx *galleryCtx) SourceLabelCounts() ([]gallery.SourceLabelCount, error) {
	return cachedValue(&cx.sourceLabelCounts, func() ([]gallery.SourceLabelCount, error) { return gallery.SourceLabelCountsQuery(cx.DB, 25) })
}

// cachedCount lazy-loads and caches a scalar COUNT query. The atomic
// pointer doubles as the cache slot and the "loaded?" flag; nil means
// re-query.
func (cx *galleryCtx) cachedCount(slot *atomic.Pointer[int], query string) (int, error) {
	if p := slot.Load(); p != nil {
		return *p, nil
	}
	var n int
	if err := cx.DB.Read.QueryRow(query).Scan(&n); err != nil {
		return 0, err
	}
	slot.Store(&n)
	return n, nil
}

// VisibleCount returns the cached count of non-missing images or queries it
// on demand. Only used for the unfiltered gallery page - filtered searches
// bypass the cache.
func (cx *galleryCtx) VisibleCount() (int, error) {
	return cx.cachedCount(&cx.visibleCount, `SELECT COUNT(*) FROM images WHERE is_missing = 0`)
}

// InboxCount returns the cached count of visible images sitting in the
// inbox (is_inbox = 1, is_missing = 0). Surfaced in the gallery toolbar's
// inbox toggle so the user sees the triage backlog at a glance. Reads
// off idx_images_inbox_visible.
func (cx *galleryCtx) InboxCount() (int, error) {
	return cx.cachedCount(&cx.inboxCount, `SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_inbox = 1`)
}

// TagCount returns the cached count of non-alias tags or queries it on demand.
// Surfaced in the Settings galleries table and the layout footer; uncached the
// query runs once per render per gallery, which adds up on multi-gallery boxes.
func (cx *galleryCtx) TagCount() (int, error) {
	return cx.cachedCount(&cx.tagCount, `SELECT COUNT(*) FROM tags WHERE is_alias = 0`)
}

// CollectionsCount returns the cached count of distinct collection
// labels across non-missing images, surfaced in the layout footer.
// Reads the trigger-maintained per-label counts, so the re-pay on the
// first render after any cache drop is one row per label.
func (cx *galleryCtx) CollectionsCount() (int, error) {
	return cx.cachedCount(&cx.collectionsCount,
		`SELECT COUNT(*) FROM collection_counts WHERE visible_count > 0`)
}

// lookupByCeiling reads a per-ceiling cache slot; returns (zero, false)
// when the level isn't yet cached. Copy-on-write semantics: the caller
// stages a new map and Stores it on a miss so concurrent readers see a
// fully-formed snapshot.
func lookupByCeiling[V any](slot *atomic.Pointer[map[string]V], level string) (V, bool) {
	if m := slot.Load(); m != nil {
		if v, ok := (*m)[level]; ok {
			return v, true
		}
	}
	var zero V
	return zero, false
}

func storeByCeiling[V any](slot *atomic.Pointer[map[string]V], level string, value V) {
	for {
		current := slot.Load()
		newMap := make(map[string]V, 4)
		if current != nil {
			for k, v := range *current {
				newMap[k] = v
			}
		}
		newMap[level] = value
		if slot.CompareAndSwap(current, &newMap) {
			return
		}
	}
}

// ceilingCached routes cx.XUnder accessors through the shared "inactive
// ceiling delegates to blind / cache / query / store" shape.
func ceilingCached[V any](c *Ceiling, blind func() (V, error), slot *atomic.Pointer[map[string]V], query func() (V, error)) (V, error) {
	if c == nil || !c.IsActive() {
		return blind()
	}
	if v, ok := lookupByCeiling(slot, c.level); ok {
		return v, nil
	}
	v, err := query()
	if err != nil {
		var zero V
		return zero, err
	}
	storeByCeiling(slot, c.level, v)
	return v, nil
}

// InboxCountUnder returns the count of visible inbox images excluding any
// whose tag list intersects the ceiling's excluded rating ids. An inactive
// ceiling delegates to the blind InboxCount.
func (cx *galleryCtx) InboxCountUnder(c *Ceiling) (int, error) {
	return ceilingCached(c, cx.InboxCount, &cx.inboxCountUnder,
		func() (int, error) { return gallery.InboxCountUnder(cx.DB, c.ExcludedTagIDs()) })
}

// PhashMissingUnder returns the relations-hub "PhashMissing" count
// excluding rows above the ceiling. An inactive ceiling reads from
// the blind cache; loadRelationsCounts is invoked on every relations
// hub / browse render, and the underlying SELECT walks every visible
// row because the phash partial index excludes NULLs. The cache is
// dropped on any ingest / delete (InvalidateCaches) and on every
// phash write (InvalidatePhashMissing).
func (cx *galleryCtx) PhashMissingUnder(c *Ceiling) (int, error) {
	return ceilingCached(c,
		func() (int, error) {
			return cx.cachedCount(&cx.phashMissing,
				`SELECT COUNT(*) FROM images WHERE phash IS NULL AND is_missing = 0`)
		},
		&cx.phashMissingUnder,
		func() (int, error) { return gallery.PhashMissingUnder(cx.DB, c.ExcludedTagIDs()) })
}

// InvalidatePhashMissing drops the cached PhashMissing counts. Call
// after a phash write that changes the NULL/non-NULL count: single-
// image recompute, the compute-phashes backfill, rebuild-thumbnails
// completion. Ingest / delete already route through InvalidateCaches.
func (cx *galleryCtx) InvalidatePhashMissing() {
	if cx == nil {
		return
	}
	cx.phashMissing.Store(nil)
	cx.phashMissingUnder.Store(nil)
}

// FolderTreeUnder returns the ceiling-aware folder tree. An inactive
// ceiling delegates to the blind FolderTree cache.
func (cx *galleryCtx) FolderTreeUnder(c *Ceiling) ([]gallery.FolderNode, error) {
	return ceilingCached(c, cx.FolderTree, &cx.folderTreeUnder,
		func() ([]gallery.FolderNode, error) { return gallery.FolderTreeUnder(cx.DB, c.ExcludedTagIDs()) })
}

// SourceLabelCountsUnder returns the ceiling-aware top-25 source labels.
func (cx *galleryCtx) SourceLabelCountsUnder(c *Ceiling) ([]gallery.SourceLabelCount, error) {
	return ceilingCached(c, cx.SourceLabelCounts, &cx.sourceLabelCountsUnder,
		func() ([]gallery.SourceLabelCount, error) {
			return gallery.SourceLabelCountsUnderQuery(cx.DB, 25, c.ExcludedTagIDs())
		})
}

// warmCaches primes the per-gallery aggregations so the first user-facing
// sidebar/gallery/settings request doesn't pay the cold scan. Errors are
// ignored: the lazy path in each accessor still recomputes on demand if the
// warm failed.
func (cx *galleryCtx) warmCaches() {
	if cx == nil || cx.DB == nil {
		return
	}
	cx.FolderTree()        //nolint:errcheck
	cx.SourceLabelCounts() //nolint:errcheck
	cx.VisibleCount()      //nolint:errcheck
	cx.InboxCount()        //nolint:errcheck
	cx.TagCount()          //nolint:errcheck
	cx.CollectionsCount()  //nolint:errcheck
}

// openGalleryCtx opens the DB and creates the thumbnails directory. The
// watcher is started separately so only the active gallery runs one.
func openGalleryCtx(g config.Gallery) (*galleryCtx, error) {
	if dbDir := filepath.Dir(g.DBPath); dbDir != "" && dbDir != "." {
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			return nil, fmt.Errorf("gallery %q: create db dir: %w", g.Name, err)
		}
	}
	database, err := db.Open(g.DBPath)
	if err != nil {
		return nil, fmt.Errorf("gallery %q: open db: %w", g.Name, err)
	}
	if err := db.Bootstrap(database); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: bootstrap db: %w", g.Name, err)
	}
	if err := os.MkdirAll(g.ThumbnailsPath, 0o755); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: create thumbnails dir: %w", g.Name, err)
	}
	degraded := false
	if _, err := os.ReadDir(g.GalleryPath); err != nil {
		logx.Warnf("gallery %q: path %q unreadable: %v - degraded mode", g.Name, g.GalleryPath, err)
		degraded = true
	}
	var generalID int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'general'`,
	).Scan(&generalID); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("gallery %q: resolve general category: %w", g.Name, err)
	}
	tree := relations.NewBKTree()
	relations.DefaultRegistry.Register(database, tree)
	return &galleryCtx{
		Handle: gallery.Handle{
			Name:           g.Name,
			GalleryPath:    g.GalleryPath,
			ThumbnailsPath: g.ThumbnailsPath,
			DBPath:         g.DBPath,
			DB:             database,
			TagSvc:         tags.New(database),
		},
		RelationsSvc:      relations.New(database),
		Degraded:          degraded,
		GeneralCategoryID: generalID,
		bkTree:            tree,
	}, nil
}

// MangaCacheDir returns the per-gallery manga page cache directory.
// Sibling to ThumbnailsPath under the gallery's data root.
func (cx *galleryCtx) MangaCacheDir() string {
	if cx == nil {
		return ""
	}
	return gallery.MangaCacheDir(cx.ThumbnailsPath)
}

// close stops the watcher and closes the DB. Keeps cx.DB non-nil afterwards:
// a concurrent warmCaches goroutine (spawned by StartWatchers, not joined)
// can still race against close at shutdown or gallery removal. A closed pool
// returns "database is closed" on subsequent calls, which the accessors
// discard; a nil pool would panic on the field deref. sql.DB.Close is
// idempotent so a later close still behaves.
func (cx *galleryCtx) close() {
	cx.stopWatcher()
	cx.stopMangaReclaim()
	if cx.DB != nil {
		relations.DefaultRegistry.Unregister(cx.DB)
		counts.Release(cx.DB)
		_ = cx.DB.Close()
	}
}

// startBackground starts everything a freshly-opened gallery runs in the
// background. Paired because a gallery given only the watcher never
// evicts the pages its reader extracts, for the life of the process.
func (cx *galleryCtx) startBackground(watchEnabled bool, maxFileSizeMB int, naming gallery.Naming, jm *jobs.Manager) {
	cx.startWatcher(watchEnabled, maxFileSizeMB, naming, jm)
	cx.startMangaReclaim()
}

// startWatcher no-ops when watching is disabled, the gallery is degraded,
// or a watcher is already running.
func (cx *galleryCtx) startWatcher(watchEnabled bool, maxFileSizeMB int, naming gallery.Naming, jm *jobs.Manager) {
	if !watchEnabled || cx.Degraded || cx.watcherCancel != nil {
		return
	}
	w, err := gallery.NewWatcher(cx.Name, cx.GalleryPath, cx.ThumbnailsPath, maxFileSizeMB, cx.DB, jm)
	if err != nil {
		logx.Warnf("gallery %q: watcher start: %v", cx.Name, err)
		return
	}
	w.Naming = naming
	w.OnEvent = jm.SetWatcherMessage
	w.OnChange = cx.InvalidateCaches
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	cx.watcherCancel = cancel
	cx.watcherDone = done
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			logx.Warnf("gallery %q: watcher stopped: %v", cx.Name, err)
		}
	}()
	logx.Infof("gallery %q: watcher started", cx.Name)
}

func (cx *galleryCtx) stopWatcher() {
	if cx.watcherCancel == nil {
		return
	}
	cx.watcherCancel()
	<-cx.watcherDone
	cx.watcherCancel = nil
	cx.watcherDone = nil
}

// startMangaReclaim spawns the per-gallery idle-page evictor. Idempotent
// against repeated calls. The reclaimer is harmless on galleries that
// have never ingested a manga - sweepOnce no-ops when the cache dir
// doesn't exist.
func (cx *galleryCtx) startMangaReclaim() {
	if cx.mangaReclaim != nil {
		return
	}
	r := gallery.NewMangaCacheReclaimer(cx.MangaCacheDir())
	r.Start(context.Background())
	cx.mangaReclaim = r
}

func (cx *galleryCtx) stopMangaReclaim() {
	if cx.mangaReclaim == nil {
		return
	}
	cx.mangaReclaim.Stop()
	cx.mangaReclaim = nil
}

// Accessors below resolve to the active gallery's fields. The
// ContextMiddleware RLock keeps the returned pointers stable per request.

func (s *Server) db() *db.DB {
	if cx := s.Active(); cx != nil {
		return cx.DB
	}
	return nil
}

func (s *Server) tagSvc() *tags.Service {
	if cx := s.Active(); cx != nil {
		return cx.TagSvc
	}
	return nil
}

// categoryExists reports whether name matches a row in tag_categories on
// the active gallery. Callers use it to disambiguate a `prefix:value`
// token that might be category-qualified or a literal tag containing a
// colon. Database errors (including nil gallery) count as "no match" so
// an ambiguous input degrades to literal.
func (s *Server) categoryExists(name string) bool {
	_, ok := s.categoryIDByName(name)
	return ok
}

// categoryIDByName resolves a category name to its row id.
func (s *Server) categoryIDByName(name string) (int64, bool) {
	d := s.db()
	if d == nil {
		return 0, false
	}
	var id int64
	err := d.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id)
	return id, err == nil
}

func (s *Server) galleryPath() string {
	if cx := s.Active(); cx != nil {
		return cx.GalleryPath
	}
	return ""
}

func (s *Server) relationsSvc() *relations.Service {
	if cx := s.Active(); cx != nil {
		return cx.RelationsSvc
	}
	return nil
}

// onImageDeleteCallback wires the active gallery's relations service
// into the gallery.DeleteImage signature. Returns nil when the
// service isn't available (e.g. mid-switch), so DeleteImage skips the
// relations cleanup step rather than crashing.
func (s *Server) onImageDeleteCallback() func(*sql.Tx, int64) error {
	svc := s.relationsSvc()
	if svc == nil {
		return nil
	}
	return svc.OnImageDeleteTx
}

// onImagesDeleteCallback is onImageDeleteCallback for the chunked bulk
// paths, which hand the whole chunk over so each group is decided once.
func (s *Server) onImagesDeleteCallback() func(*sql.Tx, []int64) error {
	svc := s.relationsSvc()
	if svc == nil {
		return nil
	}
	return svc.OnImagesDeleteTx
}

func (s *Server) thumbnailsPath() string {
	if cx := s.Active(); cx != nil {
		return cx.ThumbnailsPath
	}
	return ""
}

func (s *Server) dbPath() string {
	if cx := s.Active(); cx != nil {
		return cx.DBPath
	}
	return ""
}
