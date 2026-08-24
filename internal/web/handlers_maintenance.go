package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	meta "github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tagger"
)

// pruneMissingImagesPost queues the missing-row sweep as a background
// `delete` job so a concurrent submit lands on the shared "A job is
// already running" path the other long maintenance handlers use, and
// progress flows through the same status bar.
func (s *Server) pruneMissingImagesPost(w http.ResponseWriter, r *http.Request) {
	ids, err := db.QueryIDs(s.db().Read, `SELECT id FROM images WHERE is_missing = 1`)
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if len(ids) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 missing image(s).")
		return
	}

	if !s.startJob(w, models.JobTypeDelete) {
		return
	}
	thumbnailsPath := s.thumbnailsPath()
	tagSvc := s.tagSvc()
	active := s.Active()
	onDelete := s.onImagesDeleteCallback()
	go func() {
		ctx := s.jobs.Context()
		total := len(ids)
		s.jobs.Update(0, total, "pruning…")
		done := 0
		removed := 0
		affectedTags, processed, cancelled, err := tagSvc.ChunkedDeleteWithTagRecalc(
			ctx, ids, "", nil,
			func(tx *sql.Tx, chunk []int64, placeholders string, args []any) error {
				if onDelete != nil {
					if err := onDelete(tx, chunk); err != nil {
						return err
					}
				}
				res, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					removed += int(n)
				}
				return nil
			},
			func(chunk []int64) {
				for _, id := range chunk {
					gallery.RemoveImageArtifacts(thumbnailsPath, id, "")
				}
				done += len(chunk)
				s.jobs.Update(done, total, "pruning…")
			},
		)
		if err == nil {
			if len(affectedTags) > 0 {
				s.jobs.Update(processed, total, "reconciling tag counts…")
				if err := tagSvc.RecalcIDs(affectedTags); err != nil {
					logx.Warnf("prune-missing recalc IDs: %v", err)
				}
			}
			if removed > 0 && active != nil {
				active.InvalidateCaches()
			}
		}
		s.finishJob(err, cancelled, fmt.Sprintf("prune cancelled (%d/%d removed)", removed, total), fmt.Sprintf("Removed %d missing image(s).", removed))
	}()
	writeInlineFlash(w, "ok", "Prune started.")
}

// pruneOrphanedThumbnailsPost queues the orphan sweep as a background
// `prune-thumbs` job so the request returns immediately and progress
// surfaces through the same /internal/job/status poll as the other
// long maintenance buttons. The body is shared with scheduledRemoveOrphans
// via runOrphanSweep.
func (s *Server) pruneOrphanedThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil {
		writeInlineFlash(w, "err", "No active gallery.")
		return
	}
	if !s.startJob(w, models.JobTypePruneThumbs) {
		return
	}
	go func() {
		ctx := s.jobs.Context()
		removed, processed, total, err := s.runOrphanSweep(ctx, cx)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		if ctx.Err() != nil {
			s.jobs.Complete(fmt.Sprintf("orphan sweep cancelled (%d/%d scanned, %d removed)", processed, total, removed))
			return
		}
		s.jobs.Complete(fmt.Sprintf("Removed %d orphaned thumbnail(s).", removed))
	}()
	writeInlineFlash(w, "ok", "Thumbnail prune started.")
}

func (s *Server) recalcTagsPost(w http.ResponseWriter, r *http.Request) {
	updated, err := s.tagSvc().RecalcCount()
	s.Active().InvalidateCaches()
	if err != nil {
		writeInlineFlash(w, "err", fmt.Sprintf("Recalc partially completed (%d updated): %s", updated, err.Error()))
		return
	}
	writeInlineFlash(w, "ok", fmt.Sprintf("Recalculated %d tag count(s).", updated))
}

// tagCategoryConflictsPost counts the tag rows whose name occupies more
// than one category (the same number the Conflicts badge carries, so the
// two surfaces agree) and points the operator at the Tags page's filter,
// where the fix tools live (inline category select with merge-on-collision,
// the batch bar). The split is legal - a tag's unique key is
// (name, category_id) - but usually means two sources disagreed. Read-only.
func (s *Server) tagCategoryConflictsPost(w http.ResponseWriter, r *http.Request) {
	n, err := s.tagSvc().ConflictsCount()
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if n == 0 {
		writeInlineFlash(w, "ok", "No tags share a name across categories.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		`<div class="flash flash-ok"><strong>%d</strong> tag%s share a name across categories - <a href="/tags?conflicts=1">review them on the Tags page</a>.</div>`,
		n, plural(n))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// findFoldedDuplicatesPost recomputes folded_tag_pairs in the background,
// pairing each pre-widening folded tag with the richer spelling that
// superseded it for the /tags Folded-duplicates view.
func (s *Server) findFoldedDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if !s.startJob(w, models.JobTypeFold) {
		return
	}
	go func() {
		n, err := s.tagSvc().ScanFoldedDuplicates()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("Found %d folded duplicate(s).", n))
	}()
	writeInlineFlash(w, "ok", "Folded-duplicate scan started.")
}

// duplicatesFragmentCap bounds the one-shot fragment; the paginated
// walker page is where a long list is meant to be worked through.
const duplicatesFragmentCap = 100

func (s *Server) duplicatesListHandler(w http.ResponseWriter, r *http.Request) {
	// The endpoint is an htmx target on the Relations page; non-htmx
	// callers (refresh, paste, bookmark) get redirected to the page so
	// the URL produces a useful view either way rather than a naked
	// <table> fragment.
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/relations#file-duplicates", http.StatusSeeOther)
		return
	}
	// This is the one duplicates surface that renders filenames and ids
	// rather than counts, so it owes the ceiling the same filter the
	// sha256 walker and the delete-all branch apply - otherwise a SFW
	// ceiling still prints the paths of what it hides, and [promote]
	// acts on them.
	from := ` FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0`
	args := []any{}
	if where, wargs := resolveCeiling(r, s.Active()).WhereOne("i.id"); where != "" {
		from += ` WHERE ` + where
		args = append(args, wargs...)
	}
	var total int
	if err := s.db().Read.QueryRow(`SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type aliasRow struct {
		ImageID       int64
		CanonicalPath string
		PathID        int64
		AliasPath     string
	}
	aliases, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (aliasRow, error) {
		var a aliasRow
		err := rows.Scan(&a.ImageID, &a.CanonicalPath, &a.PathID, &a.AliasPath)
		return a, err
	}, `SELECT i.id, i.canonical_path, ip.id as path_id, ip.path`+from+`
		ORDER BY i.id, ip.id
		LIMIT ?`, append(append([]any{}, args...), duplicatesFragmentCap)...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderTemplate(w, "partials/duplicates_list.html", map[string]any{
		"Aliases":   aliases,
		"Withheld":  max(total-len(aliases), 0),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) removeDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	// Remove a specific subset when the form carries path_id values (one per
	// listed row), or every non-canonical row when the form carries
	// `all=true`. Refusing to fall through unless one of the two is set
	// keeps a stray POST with just a CSRF token from wiping the whole
	// library of alias files at once.
	selected := r.Form["path_id"]
	allFlag := r.FormValue("all") == "true"
	if len(selected) == 0 && !allFlag {
		writeInlineFlash(w, "err", "No duplicate paths selected.")
		return
	}

	var query string
	var args []any
	if allFlag {
		// The sha256 walker filters non-canonical aliases by ceiling
		// so explicit-rated images don't surface in the table. Mirror
		// that filter here so "Delete all duplicate files" can't wipe
		// aliases the operator can't see.
		query = `
			SELECT ip.id, ip.path
			FROM image_paths ip
			JOIN images i ON i.id = ip.image_id
			WHERE ip.is_canonical = 0`
		if where, wargs := resolveCeiling(r, s.Active()).WhereOne("i.id"); where != "" {
			query += ` AND ` + where
			args = append(args, wargs...)
		}
	} else {
		// Build an IN (?,?,...) query restricted to the supplied path_ids
		// that still aren't canonical - callers can't use this endpoint to
		// remove the canonical path for an image.
		ids := make([]int64, 0, len(selected))
		for _, s := range selected {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			writeInlineFlash(w, "err", "No valid path_ids in request.")
			return
		}
		var placeholders string
		placeholders, args = db.InPlaceholders(ids)
		query = `SELECT ip.id, ip.path FROM image_paths ip
			 WHERE ip.is_canonical = 0 AND ip.id IN (` + placeholders + `)`
	}

	type pathRow struct {
		ID   int64
		Path string
	}
	paths, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (pathRow, error) {
		var p pathRow
		err := rows.Scan(&p.ID, &p.Path)
		return p, err
	}, query, args...)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}

	if len(paths) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 duplicate path(s).")
		return
	}

	// Reserve a job slot for the duration; sibling long-running
	// maintenance handlers all do this so a concurrent autotag /
	// rebuild-thumbs / vacuum doesn't race the per-path unlinks. The
	// goroutine drives the actual work; the response returns
	// immediately and the status bar surfaces progress.
	if !s.startJob(w, models.JobTypeDelete) {
		return
	}
	galleryRoot := s.galleryPath()
	go func() {
		ctx := s.jobs.Context()
		total := len(paths)
		removed := 0
		const chunkSize = 500
		pathIDs := make([]int64, len(paths))
		byID := make(map[int64]string, len(paths))
		for i, p := range paths {
			pathIDs[i] = p.ID
			byID[p.ID] = p.Path
		}
		// Batch DELETEs by chunk in one transaction each so the writer
		// pool sees one Exec per 500 rows instead of one per row.
		_, cancelled, err := chunkedJob(ctx, s.jobs, pathIDs, chunkSize, "removing", func(chunk []int64) error {
			ph, args := db.InPlaceholders(chunk)
			if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id IN (`+ph+`)`, args...); err != nil {
				logx.Warnf("remove duplicates chunk delete: %v", err)
				return err
			}
			for _, id := range chunk {
				path := byID[id]
				if path == "" {
					removed++
					continue
				}
				if err := unlinkUnderGallery(galleryRoot, path); err != nil {
					logx.Warnf("remove duplicate %q: %v", path, err)
				}
				removed++
			}
			return nil
		})
		s.finishJob(err, cancelled, fmt.Sprintf("remove duplicates cancelled (%d/%d)", removed, total), fmt.Sprintf("Removed %d duplicate path(s).", removed))
	}()
	writeInlineFlash(w, "ok", "Duplicate-path removal started.")
}

// promoteAliasPathPost flips an alias path to the canonical path for
// its image. Used by the Relations page's file-duplicates section,
// where the operator may have spotted that the preferred copy is
// living at the alias location and wants to swap which path the gallery
// considers authoritative.
func (s *Server) promoteAliasPathPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	pathIDRaw := r.FormValue("path_id")
	pathID, err := strconv.ParseInt(pathIDRaw, 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid path id.")
		return
	}
	var imageID int64
	var newPath string
	var alreadyCanonical int
	if err := s.db().Read.QueryRow(
		`SELECT image_id, path, is_canonical FROM image_paths WHERE id = ?`, pathID,
	).Scan(&imageID, &newPath, &alreadyCanonical); err != nil {
		flashStatus(w, http.StatusNotFound, "Path not found.")
		return
	}
	if alreadyCanonical == 1 {
		writeInlineFlash(w, "ok", "Already canonical.")
		return
	}
	if _, statErr := os.Stat(newPath); statErr != nil {
		flashStatus(w, http.StatusBadRequest, "Cannot promote: file is missing on disk.")
		return
	}
	if err := s.promoteCanonicalPath(imageID, newPath,
		`UPDATE image_paths SET is_canonical = 1 WHERE id = ?`, pathID); err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeInlineFlash(w, "ok", "Promoted to canonical.")
}

func (s *Server) rebuildThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	if err := s.startRebuildThumbsJob(s.Active()); err != nil {
		if errors.Is(err, jobs.ErrJobRunning) {
			flashStatus(w, http.StatusConflict, "A job is already running.")
			return
		}
		writeInlineFlash(w, "err", err.Error())
		return
	}
	writeInlineFlash(w, "ok", "Thumbnail rebuild started.")
}

// startRebuildThumbsJob queues a rebuild-thumbs job against the supplied
// gallery context, reading images and writing thumbnails from that gallery's
// own DB + thumbnails dir. Reused by the manual handler (active gallery) and
// the post-import hook (imported non-active gallery).
func (s *Server) startRebuildThumbsJob(cx *galleryCtx) error {
	if cx == nil || cx.DB == nil {
		return fmt.Errorf("no gallery context")
	}
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
	}
	imgs, err := db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (imgRow, error) {
		var img imgRow
		err := rows.Scan(&img.ID, &img.Path, &img.FileType)
		return img, err
	}, `SELECT id, canonical_path, file_type FROM images WHERE is_missing = 0`)
	if err != nil {
		return err
	}

	if err := s.jobs.Start(models.JobTypeRebuildThumbs); err != nil {
		return err
	}
	thumbnailsPath := cx.ThumbnailsPath
	galleryName := cx.Name
	database := cx.DB
	go func() {
		ctx := s.jobs.Context()
		processed, rebuilt := 0, 0
		total := len(imgs)
		// A refused row - past the decode budget, or a video with no ffmpeg -
		// still advances the walk, so the progress bar counts it while the
		// summary keeps it out of the rebuilt tally.
		failedNote := func() string {
			if processed == rebuilt {
				return ""
			}
			return fmt.Sprintf(", %d failed", processed-rebuilt)
		}
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("[%s] rebuild cancelled (%d/%d rebuilt%s)", galleryName, rebuilt, total, failedNote()))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("[%s] rebuilding…", galleryName))
			if err := gallery.Generate(img.Path, thumbnailsPath, img.ID, img.FileType); err != nil {
				logx.Warnf("rebuild thumbnail for %d: %v", img.ID, err)
			} else {
				rebuilt++
			}
			// Backfill width/height for video rows that pre-date the
			// ingest-time ffprobe probe. Cheap (one ffprobe per row, only
			// when the file_type is video) and rides the existing rebuild
			// iteration so the operator gets the fix without a new button.
			if gallery.IsVideoType(img.FileType) {
				if w, h, ok := gallery.ProbeVideoDimensions(img.Path); ok {
					if _, err := database.Write.ExecContext(ctx,
						`UPDATE images SET width = ?, height = ? WHERE id = ?`,
						w, h, img.ID,
					); err != nil {
						logx.Warnf("backfill video dimensions for %d: %v", img.ID, err)
					}
				}
			}
			processed++
		}
		s.jobs.Complete(fmt.Sprintf("[%s] rebuilt %d thumbnail(s)%s.", galleryName, rebuilt, failedNote()))
	}()
	return nil
}

// computeHashesPost backfills the two derived digests for every visible
// row missing one: images.phash, then images.md5. The phash pass runs
// first because it reads small cached thumbnails while the md5 pass
// reads every original byte in the gallery - a cancel a few seconds in
// leaves the cheap half done rather than nothing.
func (s *Server) computeHashesPost(w http.ResponseWriter, r *http.Request) {
	if !s.startJob(w, models.JobTypeHashes) {
		return
	}
	database := s.db()
	thumbnailsPath := s.thumbnailsPath()
	active := s.Active()
	tree := active.bkTree
	go func() {
		ctx := s.jobs.Context()
		phashed, phashUpdated, err := relations.BackfillPhashes(ctx, database, thumbnailsPath, func(p, total int, _ string) {
			s.jobs.Update(p, total, "Perceptual hashes…")
		})
		// Drop the in-memory tree so the next find-pairs / phash: query
		// rebuilds against the now-fully-phashed DB instead of replaying
		// thousands of incremental Inserts the hook would have fired
		// during a built-tree run. Reset is cheap and the rebuild is the
		// uncached path the same query would pay on a cold server. Runs
		// before the md5 pass so cancelling that one still leaves the
		// tree consistent with what was just written.
		if tree != nil {
			tree.Reset()
		}
		active.InvalidatePhashMissing()
		cancelled := func(err error) bool { return err == context.Canceled || ctx.Err() != nil }
		if cancelled(err) {
			s.jobs.Complete(fmt.Sprintf("hashes cancelled (%d phash, 0 md5)", phashUpdated))
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		summed, sumUpdated, err := gallery.BackfillMD5s(ctx, database, func(p, total int, _ string) {
			s.jobs.Update(p, total, "MD5…")
		})
		if cancelled(err) {
			s.jobs.Complete(fmt.Sprintf("hashes cancelled (%d phash, %d md5)", phashUpdated, sumUpdated))
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("Computed %d perceptual hash(es) of %d and %d md5 digest(s) of %d.",
			phashUpdated, phashed, sumUpdated, summed))
	}()
	writeInlineFlash(w, "ok", "Hash backfill started.")
}

func (s *Server) vacuumDBPost(w http.ResponseWriter, r *http.Request) {
	// VACUUM holds the writer for tens of seconds on a multi-GB DB. Take
	// a job slot so the status bar reflects what's running and the
	// scheduler / a concurrent user-triggered job is refused with the
	// usual "a job is already running" message instead of silently
	// queueing behind the writer.
	if !s.startJob(w, models.JobTypeVacuum) {
		return
	}
	// Run the (long) VACUUM + checkpoint sequence in a goroutine so the
	// HTTP request returns immediately (mirrors every other long-running
	// maintenance handler); running synchronously would block the
	// request thread for tens of seconds.
	go func() {
		beforeSize := dbFileSize(s.dbPath())
		if _, err := s.db().Write.Exec(`VACUUM`); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		// VACUUM in WAL mode writes the rebuilt pages into the -wal file,
		// so the user sees no drop in on-disk footprint until the WAL is
		// consolidated. Truncate the WAL explicitly so the reclaimed
		// space is actually released.
		if _, err := s.db().Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			logx.Warnf("vacuum wal_checkpoint: %v", err)
		}
		afterSize := dbFileSize(s.dbPath())
		freed := max(beforeSize-afterSize, 0)
		s.jobs.Complete(fmt.Sprintf("Vacuumed (reclaimed %s).", humanBytesFmt(freed)))
	}()
	writeInlineFlash(w, "ok", "Vacuum started. Watch the status bar for the reclaimed-space report.")
}

// freeMemoryPost runs the on-demand version of runMemoryReclaim: trims
// every gallery's SQLite page cache, returns the Go heap, and SIGTERMs
// the auto-tagger worker so its CUDA libraries (when loaded) go with
// it. Reserves a job slot for the duration so a concurrent autotag /
// sync starting between our entry and ReleaseAll can't see the worker
// killed mid-inference; the standard "A job is already running" reply
// is what the racing handler observes.
func (s *Server) freeMemoryPost(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Start(models.JobTypeFreeMemory); err != nil {
		flashStatus(w, http.StatusConflict, "A job is running; try again when it finishes.")
		return
	}
	defer s.jobs.Complete("Memory caches released.")
	before := readVmRSS()
	ctxs := s.allContexts()
	for _, cx := range ctxs {
		if err := cx.DB.ShrinkMemory(context.Background()); err != nil {
			logx.Warnf("free memory: shrink %q: %v", cx.Name, err)
		}
	}
	debug.FreeOSMemory()
	tagger.ReleaseAll()
	after := readVmRSS()
	if before > 0 && after > 0 && before > after {
		writeInlineFlash(w, "ok", "Freed "+humanBytesFmt(int64(before-after))+".")
		return
	}
	writeInlineFlash(w, "ok", "Memory caches released.")
}

// dbFileSize returns the total on-disk footprint of the SQLite database -
// the main file plus the WAL and shared-memory sidecars. A post-VACUUM
// "reclaimed N" figure that only counts the main file misleads the user
// whenever the WAL holds the bulk of the pages (common after mass deletes).
func dbFileSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// humanBytesFmt formats a byte count with binary units. The template
// function "humanBytes" exposes the same body to template authors.
func humanBytesFmt(b int64) string {
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
}

func (s *Server) reExtractMetadataPost(w http.ResponseWriter, r *http.Request) {
	// Stream rows into a slice of lightweight structs so the DB cursor is closed
	// before the long-running goroutine starts. This avoids holding a read
	// connection open for the entire re-extraction job while keeping memory
	// usage proportional to the number of images (IDs + short paths only).
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
		// Current persisted hashes; we use them to skip the rewrite when the
		// new extraction would produce the same generation_hash - most runs
		// on an unchanged library now turn into pure reads.
		sdHash    string
		comfyHash string
		source    string
	}

	imgs, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (imgRow, error) {
		var img imgRow
		err := rows.Scan(&img.ID, &img.Path, &img.FileType, &img.source, &img.sdHash, &img.comfyHash)
		return img, err
	}, `
		SELECT i.id, i.canonical_path, i.file_type, i.source_type,
		       COALESCE(sm.generation_hash, ''),
		       COALESCE(cm.generation_hash, '')
		FROM images i
		LEFT JOIN sd_metadata sm ON sm.image_id = i.id
		LEFT JOIN comfyui_metadata cm ON cm.image_id = i.id
		WHERE i.is_missing = 0
	`)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}

	if !s.startJob(w, models.JobTypeReExtract) {
		return
	}

	database := s.db()
	thumbnailsPath := s.thumbnailsPath()
	active := s.Active()
	go func() {
		ctx := s.jobs.Context()
		processed := 0
		updated := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("re-extraction cancelled (%d/%d processed, %d updated)", processed, total, updated))
				return
			}
			s.jobs.Update(processed, total, "Processing…")
			// Recompute phash from the on-disk thumbnail. A decode
			// failure (missing thumbnail, corrupt jpg) leaves the
			// previous value in place; the operator can rebuild
			// thumbnails first if they care about that row.
			if err := gallery.RecomputeAndStorePhash(ctx, database, img.ID, thumbnailsPath); err != nil {
				logx.Debugf("re-extract phash %d: %v", img.ID, err)
			}
			sdMeta, comfyMeta, _ := meta.Extract(img.Path, img.FileType)

			sourceType := models.SourceTypeNone
			if sdMeta != nil && comfyMeta != nil {
				sourceType = models.SourceTypeBoth
			} else if sdMeta != nil {
				sourceType = models.SourceTypeA1111
			} else if comfyMeta != nil {
				sourceType = models.SourceTypeComfyUI
			}

			newSDHash := ""
			if sdMeta != nil {
				newSDHash = sdMeta.GenerationHash
			}
			newComfyHash := ""
			if comfyMeta != nil {
				newComfyHash = comfyMeta.GenerationHash
			}

			// Probe video duration when ffmpeg is available. NULL stays
			// NULL for non-video file types so static images don't grow
			// a phantom duration column.
			var durationSec *float64
			if gallery.IsVideoType(img.FileType) {
				if d, ok := gallery.ProbeDurationSeconds(img.Path); ok {
					durationSec = &d
				}
			}

			// Skip the delete+insert churn when the new extraction lines up
			// with what the DB already holds. Any pipeline change that adds
			// or drops fields changes the generation hash, so this stays
			// responsive to real metadata schema updates. Successful video
			// probes always re-write: the previous duration_seconds isn't
			// in the per-image SELECT, so we can't compare, and overwriting
			// the same float is cheap.
			if newSDHash == img.sdHash && newComfyHash == img.comfyHash && sourceType == img.source && durationSec == nil {
				processed++
				continue
			}

			// Single transaction per image so a mid-flight failure can't leave
			// images.source_type updated against a half-deleted metadata table
			// or a deleted-but-not-reinserted row.
			if err := reExtractApply(ctx, database, img.ID, sourceType, durationSec, sdMeta, comfyMeta); err != nil {
				logx.Warnf("re-extract image %d: %v", img.ID, err)
				processed++
				continue
			}
			processed++
			updated++
		}
		active.InvalidatePhashMissing()
		s.jobs.Complete(fmt.Sprintf("Re-extracted metadata for %d image(s) (%d updated).", processed, updated))
	}()

	writeInlineFlash(w, "ok", "Re-extraction started.")
}

// reExtractApply commits a re-extracted image's source_type, deletes the
// previous SD/ComfyUI rows, and reinserts whichever the parser produced.
// All four steps run in one transaction so a partial failure (writer
// contention, ctx cancellation mid-statement) never leaves the row with
// updated source_type but missing metadata. durationSec is set on
// video rows whose ffprobe call succeeded; nil leaves the column
// untouched so non-videos and probe-failed videos don't churn.
func reExtractApply(ctx context.Context, database *db.DB, imageID int64, sourceType string, durationSec *float64, sdMeta *models.SDMetadata, comfyMeta *models.ComfyUIMetadata) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE images SET source_type = ? WHERE id = ?`, sourceType, imageID); err != nil {
		return fmt.Errorf("update source_type: %w", err)
	}
	if durationSec != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE images SET duration_seconds = ? WHERE id = ?`, *durationSec, imageID); err != nil {
			return fmt.Errorf("update duration_seconds: %w", err)
		}
	}
	if err := gallery.ReplaceGenerationMetadata(ctx, tx, imageID, sdMeta, comfyMeta); err != nil {
		return err
	}
	return tx.Commit()
}
