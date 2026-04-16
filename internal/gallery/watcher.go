package gallery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
)

// Watcher watches the gallery directory for new files and ingests them.
type Watcher struct {
	fsw     *fsnotify.Watcher
	cfg     *config.Config
	db      *db.DB
	jobs    *jobs.Manager
	OnEvent func(msg string) // callback for status notifications (may be nil)

	mu     sync.Mutex
	timers map[string]*time.Timer
}

// NewWatcher creates and initializes a filesystem watcher.
func NewWatcher(cfg *config.Config, database *db.DB, jobManager *jobs.Manager) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		fsw:    fsw,
		cfg:    cfg,
		db:     database,
		jobs:   jobManager,
		timers: map[string]*time.Timer{},
	}

	if addErr := fsw.Add(cfg.Paths.GalleryPath); addErr != nil {
		fsw.Close()
		return nil, fmt.Errorf("fsnotify watch gallery root: %w", addErr)
	}
	logx.Infof("watcher: watching %s", cfg.Paths.GalleryPath)

	// Walk and watch all existing subdirectories, stopping gracefully on inotify limits.
	watchCount := 1
	limitHit := false
	filepath.WalkDir(cfg.Paths.GalleryPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == cfg.Paths.GalleryPath {
			return nil
		}
		if limitHit {
			return filepath.SkipAll
		}
		if addErr := fsw.Add(path); addErr != nil {
			if strings.Contains(addErr.Error(), "no space left") ||
				strings.Contains(addErr.Error(), "too many open files") {
				logx.Warnf("fsnotify: inotify limit hit at %d dirs. "+
					"Increase: echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.conf && sudo sysctl -p", watchCount)
				limitHit = true
				return filepath.SkipAll
			}
			logx.Warnf("fsnotify add %q: %v", path, addErr)
		} else {
			watchCount++
		}
		return nil
	})

	return w, nil
}

// Run starts the event loop. Returns when ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.cancelPendingTimers()
			w.fsw.Close()
			return nil

		case event, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}

			// Drop events while a manual sync is running; sync walks the
			// tree itself and ingests via its own transaction, so a
			// concurrent watcher Ingest would race on the image_paths
			// UNIQUE and fail with "constraint failed: UNIQUE ... (2067)".
			if w.jobs != nil {
				if st := w.jobs.Get(); st != nil && st.Running && st.JobType == "sync" {
					continue
				}
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				info, err := os.Stat(event.Name)
				if err != nil {
					continue
				}

				if info.IsDir() {
					if addErr := w.fsw.Add(event.Name); addErr != nil {
						logx.Warnf("fsnotify add new dir %q: %v", event.Name, addErr)
					}
					continue
				}

				w.debounce(event.Name)
			}

			if event.Has(fsnotify.Remove) {
				// fsw.Remove may no-op if the target was already gone (e.g. a file remove).
				w.fsw.Remove(event.Name)
				w.markFileMissing(event.Name)
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			logx.Warnf("fsnotify error: %v", err)
		}
	}
}

func (w *Watcher) cancelPendingTimers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, t := range w.timers {
		t.Stop()
		delete(w.timers, path)
	}
}

func (w *Watcher) debounce(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.timers[path]; ok {
		t.Reset(500 * time.Millisecond)
		return
	}

	w.timers[path] = time.AfterFunc(500*time.Millisecond, func() {
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()

		w.ingestFile(path)
	})
}

func (w *Watcher) ingestFile(path string) {
	ft, err := DetectFileType(path)
	if err != nil {
		return
	}

	// Mirror the size filter Sync applies in Phase 1. Without it the
	// watcher silently ingests any file the user drops into the gallery,
	// however large — including multi-GB videos that would fail to
	// thumbnail and hold a write transaction for minutes.
	if maxMB := w.cfg.Gallery.MaxFileSizeMB; maxMB > 0 {
		if info, statErr := os.Stat(path); statErr == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				logx.Warnf("watcher: skipping %q (size %d exceeds %d MB)",
					path, info.Size(), maxMB)
				return
			}
		}
	}

	_, isDup, err := Ingest(w.db, w.cfg, path, ft)
	if err != nil {
		logx.Warnf("watcher ingest %q: %v", path, err)
	} else if isDup {
		logx.Infof("watcher: duplicate %q", path)
	} else {
		logx.Infof("watcher: ingested %q", path)
		if w.OnEvent != nil {
			w.OnEvent("watcher: added " + filepath.Base(path))
		}
	}
}

// markFileMissing marks a file as is_missing=true in the DB if it exists.
func (w *Watcher) markFileMissing(path string) {
	if !strings.HasPrefix(path, w.cfg.Paths.GalleryPath) {
		return
	}

	var imgID int64
	err := w.db.Read.QueryRow(
		`SELECT id FROM images WHERE canonical_path = ? AND is_missing = 0`, path,
	).Scan(&imgID)
	if err != nil {
		err2 := w.db.Read.QueryRow(
			`SELECT ip.image_id FROM image_paths ip
			 JOIN images i ON i.id = ip.image_id
			 WHERE ip.path = ? AND i.is_missing = 0`, path,
		).Scan(&imgID)
		if err2 != nil {
			return
		}
	}

	if _, err := w.db.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, imgID); err != nil {
		logx.Warnf("watcher mark missing %q: %v", path, err)
		return
	}
	logx.Infof("watcher: marked missing %q (id=%d)", path, imgID)
	if w.OnEvent != nil {
		w.OnEvent("watcher: removed " + filepath.Base(path))
	}
}
