package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tags"
)

// galleryCtx holds everything per-gallery: paths, DB, tag service, degraded
// flag, and watcher bookkeeping.
type galleryCtx struct {
	Name           string
	GalleryPath    string
	DBPath         string
	ThumbnailsPath string
	DB             *db.DB
	TagSvc         *tags.Service
	Degraded       bool

	// Per-gallery caches of queries that scan every visible image. All three
	// are nilled by InvalidateCaches after any ingest/delete/missing-toggle
	// so the next reader re-populates from SQLite. visibleCount holds the
	// count of is_missing=0 images as a pointer so "not cached" is
	// distinguishable from "cached zero".
	folderTree   atomic.Pointer[[]gallery.FolderNode]
	sourceCounts atomic.Pointer[gallery.SourceCounts]
	visibleCount atomic.Pointer[int]

	watcherCancel context.CancelFunc
	watcherDone   chan struct{}
}

// InvalidateCaches drops the folder-tree and visible-count caches. Call after
// any mutation that changes which images are visible (ingest, delete, sync
// mark-missing, watcher remove).
func (cx *galleryCtx) InvalidateCaches() {
	if cx == nil {
		return
	}
	cx.folderTree.Store(nil)
	cx.sourceCounts.Store(nil)
	cx.visibleCount.Store(nil)
}

// FolderTree returns the cached tree or builds one on demand. The cache is
// invalidated by InvalidateCaches.
func (cx *galleryCtx) FolderTree() ([]gallery.FolderNode, error) {
	if p := cx.folderTree.Load(); p != nil {
		return *p, nil
	}
	tree, err := gallery.FolderTree(cx.DB)
	if err != nil {
		return nil, err
	}
	cx.folderTree.Store(&tree)
	return tree, nil
}

// SourceCounts returns the cached source-tree counts or queries them on
// demand. The cache is invalidated by InvalidateCaches.
func (cx *galleryCtx) SourceCounts() (gallery.SourceCounts, error) {
	if p := cx.sourceCounts.Load(); p != nil {
		return *p, nil
	}
	sc, err := gallery.SourceCountsQuery(cx.DB)
	if err != nil {
		return gallery.SourceCounts{}, err
	}
	cx.sourceCounts.Store(&sc)
	return sc, nil
}

// VisibleCount returns the cached count of non-missing images or queries it
// on demand. Only used for the unfiltered gallery page - filtered searches
// bypass the cache.
func (cx *galleryCtx) VisibleCount() (int, error) {
	if p := cx.visibleCount.Load(); p != nil {
		return *p, nil
	}
	var n int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE is_missing = 0`).Scan(&n); err != nil {
		return 0, err
	}
	cx.visibleCount.Store(&n)
	return n, nil
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
		database.Close()
		return nil, fmt.Errorf("gallery %q: bootstrap db: %w", g.Name, err)
	}
	if err := os.MkdirAll(g.ThumbnailsPath, 0o755); err != nil {
		database.Close()
		return nil, fmt.Errorf("gallery %q: create thumbnails dir: %w", g.Name, err)
	}
	degraded := false
	if _, err := os.ReadDir(g.GalleryPath); err != nil {
		logx.Warnf("gallery %q: path %q unreadable: %v - degraded mode", g.Name, g.GalleryPath, err)
		degraded = true
	}
	return &galleryCtx{
		Name:           g.Name,
		GalleryPath:    g.GalleryPath,
		DBPath:         g.DBPath,
		ThumbnailsPath: g.ThumbnailsPath,
		DB:             database,
		TagSvc:         tags.New(database),
		Degraded:       degraded,
	}, nil
}

// close stops the watcher and closes the DB.
func (cx *galleryCtx) close() {
	cx.stopWatcher()
	if cx.DB != nil {
		cx.DB.Close()
		cx.DB = nil
	}
}

// startWatcher no-ops when watching is disabled, the gallery is degraded,
// or a watcher is already running.
func (cx *galleryCtx) startWatcher(watchEnabled bool, maxFileSizeMB int, jm *jobs.Manager) {
	if !watchEnabled || cx.Degraded || cx.watcherCancel != nil {
		return
	}
	w, err := gallery.NewWatcher(cx.Name, cx.GalleryPath, cx.ThumbnailsPath, maxFileSizeMB, cx.DB, jm)
	if err != nil {
		logx.Warnf("gallery %q: watcher start: %v", cx.Name, err)
		return
	}
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
	d := s.db()
	if d == nil {
		return false
	}
	var n int
	return d.Read.QueryRow(
		`SELECT 1 FROM tag_categories WHERE name = ? LIMIT 1`, name,
	).Scan(&n) == nil
}

func (s *Server) galleryPath() string {
	if cx := s.Active(); cx != nil {
		return cx.GalleryPath
	}
	return ""
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
