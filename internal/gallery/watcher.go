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

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
)

// Watcher watches the gallery directory for new files and ingests them.
type Watcher struct {
	fsw            *fsnotify.Watcher
	galleryName    string // for prefixing status messages when multiple galleries are watched
	galleryPath    string
	thumbnailsPath string
	maxFileSizeMB  int
	db             *db.DB
	jobs           *jobs.Manager
	OnEvent        func(msg string) // callback for status notifications (may be nil)
	OnChange       func()           // callback fired after any image add/remove (may be nil)

	mu     sync.Mutex
	timers map[string]*time.Timer
}

// NewWatcher creates and initializes a filesystem watcher for one gallery.
// galleryName prefixes status messages so multi-gallery setups can tell
// which gallery an event came from.
func NewWatcher(galleryName, galleryPath, thumbnailsPath string, maxFileSizeMB int, database *db.DB, jobManager *jobs.Manager) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		fsw:            fsw,
		galleryName:    galleryName,
		galleryPath:    galleryPath,
		thumbnailsPath: thumbnailsPath,
		maxFileSizeMB:  maxFileSizeMB,
		db:             database,
		jobs:           jobManager,
		timers:         map[string]*time.Timer{},
	}

	if addErr := fsw.Add(galleryPath); addErr != nil {
		fsw.Close()
		return nil, fmt.Errorf("fsnotify watch gallery root: %w", addErr)
	}
	logx.Infof("watcher: watching %s", galleryPath)

	// Walk and watch every subdirectory, stopping gracefully on inotify limits.
	watchCount := 1
	limitHit := false
	filepath.WalkDir(galleryPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == galleryPath {
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

			// Drop events while a manual sync or move job is running;
			// each one already touches image_paths under its own
			// transaction, so a concurrent watcher ingest would race on
			// the UNIQUE constraint or trip markFileMissing on the source.
			if w.jobs != nil {
				if st := w.jobs.Get(); st != nil && st.Running && (st.JobType == "sync" || st.JobType == "move") {
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

// eventPrefix returns `watcher: ` by default, or `watcher [name]: ` when
// the watcher has a non-empty gallery name. The bracketed form lets users
// tell multi-gallery events apart in the status bar.
func (w *Watcher) eventPrefix() string {
	if w.galleryName == "" {
		return "watcher: "
	}
	return "watcher [" + w.galleryName + "]: "
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

	// Mirror Sync's per-file size cap. Without it, dropping a multi-GB
	// video into the gallery would hang thumbnail generation and hold a
	// write transaction for minutes.
	if maxMB := w.maxFileSizeMB; maxMB > 0 {
		if info, statErr := os.Stat(path); statErr == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				logx.Warnf("watcher: skipping %q (size %d exceeds %d MB)",
					path, info.Size(), maxMB)
				return
			}
		}
	}

	_, isDup, err := Ingest(w.db, w.galleryPath, w.thumbnailsPath, path, ft, "")
	if err != nil {
		logx.Warnf("watcher ingest %q: %v", path, err)
	} else if isDup {
		logx.Infof("watcher: duplicate %q", path)
	} else {
		logx.Infof("watcher: ingested %q", path)
		if w.OnEvent != nil {
			w.OnEvent(w.eventPrefix() + "added " + filepath.Base(path))
		}
		if w.OnChange != nil {
			w.OnChange()
		}
	}
}

// markFileMissing marks a file as is_missing=true in the DB if it exists.
func (w *Watcher) markFileMissing(path string) {
	// filepath.Rel containment so a sibling directory sharing a prefix
	// (/data/gallery vs /data/gallery_backup) is correctly rejected.
	rootAbs, err := filepath.Abs(w.galleryPath)
	if err != nil {
		return
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	if !PathInside(rootAbs, pathAbs) {
		return
	}

	var imgID int64
	err = w.db.Read.QueryRow(
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
		w.OnEvent(w.eventPrefix() + "removed " + filepath.Base(path))
	}
	if w.OnChange != nil {
		w.OnChange()
	}
}
