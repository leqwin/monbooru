package gallery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
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
	// Naming renames what the watcher picks up, when the operator opted
	// in. Zero value leaves a dropped file under the name it arrived with.
	Naming Naming

	mu sync.Mutex
	// selfMoved holds the paths of a rename the watcher performed itself,
	// until the events that rename emits have had time to arrive. Without
	// it every dropped file is hashed a second time by the ingest its own
	// rename triggers.
	selfMoved map[string]time.Time
	closing   bool
	timers    map[string]*debounceTimer
	wg        sync.WaitGroup // in-flight ingests from fired timers
}

const debounceDelay = 500 * time.Millisecond

// movedOutGrace outlasts the debounced ingest a vanished file may pair
// with, so whatever claims the row does so before the file is judged
// gone: a move inside the tree re-ingests at the destination, and an
// in-place replace rewrites the same path.
const movedOutGrace = 2 * time.Second

// debounceTimer pairs a pending timer with the deadline of its latest
// arm, so a callback that fired just before a Reset can tell the re-arm
// happened and yield to the later fire. run is what the fire does: the
// last event on a path decides that too, not just when it lands.
type debounceTimer struct {
	t        *time.Timer
	deadline time.Time
	run      func(string)
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
		timers:         map[string]*debounceTimer{},
		selfMoved:      map[string]time.Time{},
	}

	if addErr := fsw.Add(galleryPath); addErr != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("fsnotify watch gallery root: %w", addErr)
	}
	logx.Infof("watcher: watching %s", galleryPath)

	// Walk and watch every subdirectory, stopping gracefully on inotify limits.
	watchCount := 1
	limitHit := false
	if walkErr := filepath.WalkDir(galleryPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == galleryPath {
			return nil
		}
		if limitHit {
			return filepath.SkipAll
		}
		if addErr := fsw.Add(path); addErr != nil {
			// Prefer typed errno detection so localised glibc and a
			// future fsnotify version that wraps the syscall error
			// differently don't silently break inotify-limit handling.
			// Fall back to the substring match because some wrappers
			// stringify and don't unwrap to syscall.Errno.
			if errors.Is(addErr, syscall.ENOSPC) ||
				errors.Is(addErr, syscall.EMFILE) ||
				strings.Contains(addErr.Error(), "no space left") ||
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
	}); walkErr != nil {
		// A WalkDir error here means the outer traversal could not
		// finish - typically a permission denied at the gallery root or
		// a vanished symlink. Surface it at warn so the operator can
		// fix the access rights; the partially-built watcher still
		// works for the dirs it did register.
		logx.Warnf("watcher: walk %q: %v", galleryPath, walkErr)
	}

	return w, nil
}

// Run starts the event loop. Returns when ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.shutdown()
			return nil

		case event, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}

			if w.jobSuppressesIngest() {
				continue
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				info, err := os.Stat(event.Name)
				if err != nil {
					// inotify reports the source of a move as Rename
					// carrying the old name. A move inside the tree pairs
					// it with a Create for the destination; one that
					// nothing claims took the file out of the library.
					if event.Has(fsnotify.Rename) {
						w.schedule(event.Name, movedOutGrace, w.reconcileVanished)
					}
					continue
				}

				if info.IsDir() {
					w.registerTree(event.Name)
					continue
				}

				w.debounce(event.Name)
			}

			// Slow writers (network copies, large archives) keep firing
			// IN_MODIFY long after IN_CREATE; debouncing the write extends
			// the pending create-timer so ingestFile runs once on the
			// settled bytes, not a partial file the archive parser would
			// reject for a missing central directory. If the create-timer
			// already fired on a partial archive, the late writes schedule
			// a follow-up ingest that finds the now-complete file; a write
			// to an already-ingested file re-ingests but dedups on SHA-256.
			if event.Has(fsnotify.Write) {
				w.debounce(event.Name)
			}

			// A delete waits out the same grace as a move: replacing a
			// file in place deletes and rewrites the same path when the
			// staging dir and the gallery sit on different filesystems,
			// and marking the row missing in between races the replace's
			// own commit.
			if event.Has(fsnotify.Remove) {
				_ = w.fsw.Remove(event.Name)
				w.schedule(event.Name, movedOutGrace, w.reconcileVanished)
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

// registerTree watches dir and every subdirectory beneath it, and schedules
// ingest for any file already present. A mkdir + write burst can land files
// inside a new directory before its Create event is handled, so entries that
// predate the watch emit no further event and would otherwise wait for the
// next manual sync.
func (w *Watcher) registerTree(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if addErr := w.fsw.Add(path); addErr != nil {
				logx.Warnf("fsnotify add new dir %q: %v", path, addErr)
			}
			return nil
		}
		w.debounce(path)
		return nil
	})
}

// jobSuppressesIngest reports whether a running manual sync, move,
// transfer, delete, or tag job owns the image_paths / image_tags surface
// right now; each one already mutates them under its own transaction, so
// a concurrent watcher ingest would race on the UNIQUE constraint or
// trip markFileMissing on the source. A transfer writes into this
// gallery from another gallery's request, so the copy it lands must not
// be re-ingested by the watcher underneath it. Checked in the event loop
// and again when a debounce timer fires, since a timer scheduled just
// before the job started would otherwise ingest concurrently with it.
func (w *Watcher) jobSuppressesIngest() bool {
	if w.jobs == nil {
		return false
	}
	st := w.jobs.Get()
	if st == nil || !st.Running {
		return false
	}
	switch st.JobType {
	case "sync", "move", "transfer", "delete", "tag":
		return true
	}
	return false
}

// shutdown stops pending timers and waits for any ingest whose timer
// already fired, so the caller can close the DB the moment Run returns.
func (w *Watcher) shutdown() {
	w.mu.Lock()
	w.closing = true
	for path, e := range w.timers {
		e.t.Stop()
		delete(w.timers, path)
	}
	w.mu.Unlock()
	w.wg.Wait()
	_ = w.fsw.Close()
}

func (w *Watcher) debounce(path string) {
	w.schedule(path, debounceDelay, w.ingestFile)
}

// selfMovedGrace outlasts both timers a self-inflicted rename can arm, so
// the CREATE for the destination and the deferred vanish-check on the
// source are both ignored.
const selfMovedGrace = movedOutGrace + debounceDelay

// claimSelfMove marks paths as the watcher's own work and cancels
// anything already armed for them. Cancelling matters as much as the
// mark: the events land before the rename returns, so the timer they arm
// is usually already ticking by the time this runs.
func (w *Watcher) claimSelfMove(paths ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(selfMovedGrace)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if e, ok := w.timers[p]; ok {
			e.t.Stop()
			delete(w.timers, p)
		}
		w.selfMoved[p] = deadline
	}
}

// schedule arms run(path) for delay from now, replacing anything already
// pending on the same path.
func (w *Watcher) schedule(path string, delay time.Duration, run func(string)) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closing {
		return
	}
	now := time.Now()
	for p, deadline := range w.selfMoved {
		if now.After(deadline) {
			delete(w.selfMoved, p)
		}
	}
	if _, mine := w.selfMoved[path]; mine {
		return
	}
	if e, ok := w.timers[path]; ok {
		e.deadline = time.Now().Add(delay)
		e.run = run
		e.t.Reset(delay)
		return
	}

	e := &debounceTimer{deadline: time.Now().Add(delay), run: run}
	e.t = time.AfterFunc(delay, func() { w.onDebounceFired(path, e) })
	w.timers[path] = e
}

// onDebounceFired runs when a debounce timer expires. A Reset that won
// the race against this callback pushed the deadline forward - that
// re-armed fire ingests the settled bytes, so this one yields instead of
// ingesting a file still being written. The WaitGroup registration
// happens under the lock so shutdown's wait can't miss an ingest that
// already passed the closing check.
func (w *Watcher) onDebounceFired(path string, e *debounceTimer) {
	w.mu.Lock()
	if w.closing || w.timers[path] != e || time.Now().Before(e.deadline) {
		w.mu.Unlock()
		return
	}
	delete(w.timers, path)
	run := e.run
	w.wg.Add(1)
	w.mu.Unlock()
	defer w.wg.Done()

	run(path)
}

// reconcileVanished marks the row at path missing when nothing has taken
// the file over in the meantime. A move inside the tree re-ingests at the
// destination and repoints the row, and a replace puts new bytes back at
// the same path; only a file that really left the library is still
// standing here.
func (w *Watcher) reconcileVanished(path string) {
	if w.jobSuppressesIngest() {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return
	}
	w.markFileMissing(path)
}

func (w *Watcher) ingestFile(path string) {
	if w.jobSuppressesIngest() {
		return
	}
	if _, err := DetectFileType(path); err != nil {
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

	img, isDup, err := Ingest(w.db, w.galleryPath, w.thumbnailsPath, path, "")
	if err != nil {
		logx.Warnf("watcher ingest %q: %v", path, err)
	} else if isDup {
		logx.Infof("watcher: duplicate %q", path)
	} else {
		if !w.Naming.Empty() && img != nil {
			path = w.renameIngested(img.ID, path)
		}
		logx.Infof("watcher: ingested %q", path)
		if w.OnEvent != nil {
			w.OnEvent(w.eventPrefix() + "added " + filepath.Base(path))
		}
		if w.OnChange != nil {
			w.OnChange()
		}
	}
}

// renameIngested applies the operator's naming to a file the watcher just
// picked up and returns where it ended up. Both paths are claimed before
// the rename so the CREATE and REMOVE it emits do not re-enter ingest; a
// failed rename leaves the file where it was, which is still a complete
// row.
func (w *Watcher) renameIngested(id int64, path string) string {
	w.claimSelfMove(path)
	// Background rather than the run context: the ingest that just read
	// this file end to end is not cancellable either, so cutting the
	// rename short at shutdown would only strand the file half-filed.
	newPath, err := w.Naming.Apply(context.Background(), w.db, w.galleryPath, id, "", "")
	if err != nil {
		logx.Warnf("watcher: name %q: %v", path, err)
		return path
	}
	if newPath == "" {
		return path
	}
	w.claimSelfMove(newPath)
	return newPath
}

// promoteSurvivingCopy repoints id at one of its sha-identical copies when
// the canonical file has gone but a copy is still on disk, reporting
// whether it found one. Sync answers the same state the same way
// (promoteAliasToCanonical); without this the image is hidden until a sync
// runs, and Prune missing images would delete the row and its tags while
// the bytes sit in the tree.
func (w *Watcher) promoteSurvivingCopy(id int64, gonePath string) bool {
	copies, err := AliasPathsFor(w.db.Read, []int64{id})
	if err != nil {
		logx.Warnf("watcher promote copy %d: list paths: %v", id, err)
		return false
	}
	for _, c := range copies[id] {
		p := c.Path
		info, statErr := os.Stat(p)
		if statErr != nil {
			continue
		}
		// Sync reaches the same decision with a fresh hash in hand; the
		// length is the cheap half of that. Without it a copy overwritten
		// while nothing was watching becomes the canonical, and sha256,
		// md5 and file_size then describe bytes that are not there.
		if info.Size() != c.Size {
			logx.Warnf("watcher promote copy %d: %q holds %d bytes, not the row's %d - left alone",
				id, p, info.Size(), c.Size)
			continue
		}
		if err := repointCanonical(w.db.Write, id, p, FolderPath(w.galleryPath, p), gonePath); err != nil {
			logx.Warnf("watcher promote copy %d: %v", id, err)
			return false
		}
		logx.Infof("watcher: %q went, kept id=%d at %q", gonePath, id, p)
		if w.OnChange != nil {
			w.OnChange()
		}
		return true
	}
	return false
}

// markFileMissing flips is_missing=1 and rebalances the usage_count of
// every tag the image was carrying, in one write transaction. usage_count
// is the visible-image count for a tag, so removing an image from the
// visible set owes each of its tags a decrement. The raw UPDATE is
// deliberate: it runs after is_missing = 1 in the same transaction, where
// the guarded DropTagUsageTx helper would see a row it no longer counts
// and refuse.
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
			 WHERE ip.path = ? AND ip.is_canonical = 1 AND i.is_missing = 0`, path,
		).Scan(&imgID)
		if err2 != nil {
			return
		}
	}

	if w.promoteSurvivingCopy(imgID, path) {
		return
	}

	tx, err := w.db.Write.Begin()
	if err != nil {
		logx.Warnf("watcher mark missing %q: begin tx: %v", path, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, imgID); err != nil {
		logx.Warnf("watcher mark missing %q: %v", path, err)
		return
	}

	tagIDs, err := db.QueryIDs(tx, `SELECT tag_id FROM image_tags WHERE image_id = ?`, imgID)
	if err != nil {
		logx.Warnf("watcher mark missing %q: list tags: %v", path, err)
		return
	}

	for _, tid := range tagIDs {
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, tid,
		); err != nil {
			logx.Warnf("watcher mark missing %q: decrement tag %d: %v", path, tid, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logx.Warnf("watcher mark missing %q: commit: %v", path, err)
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
