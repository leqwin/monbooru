package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
)

// resolveBatchScope returns the image-id slice the caller's batch
// operates on, materialised from either the "selection" (checked ids)
// or "search" (everything matching the current query) form scope.
// Writes an error fragment and returns ok=false on bad input.
func (s *Server) resolveBatchScope(w http.ResponseWriter, r *http.Request, errLabel string) ([]int64, bool) {
	scope := strings.TrimSpace(r.FormValue("scope"))
	switch scope {
	case "selection":
		return parseIDList(r.Form["ids"]), true
	case "search":
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			flashStatus(w, http.StatusBadRequest, "Could not parse search: "+parseErr.Error())
			return nil, false
		}
		// "act on current search" must mirror what the operator sees in
		// the gallery - including the cookie ceiling. Without this wrap
		// a SFW-ceiling-on operator clicking "delete all current search"
		// would wipe explicit rows they can't even see.
		expr = resolveCeiling(r, s.Active()).Apply(expr)
		var ids []int64
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("%s search: %v", errLabel, err)
			flashStatus(w, http.StatusInternalServerError, "Search error.")
			return nil, false
		}
		return ids, true
	default:
		flashStatus(w, http.StatusBadRequest, "scope must be search or selection")
		return nil, false
	}
}

func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids := parseIDList(r.Form["ids"])
	if len(ids) == 0 {
		s.startBulkDelete(w, nil)
		return
	}
	// Single IN query feeds every target through one round-trip; a
	// 1000-checkbox selection used to pay 1000 reads here. The order
	// returned by the SELECT is undefined under SQLite without an
	// ORDER BY, so re-emit in the caller's input order via a map.
	placeholders, args := db.InPlaceholders(ids)
	rows, err := s.db().Read.Query(
		`SELECT id, canonical_path, folder_path, is_missing FROM images WHERE id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		// startBulkDelete(nil) would 202 with nothing queued, which the
		// client reads as success - so surface the failure instead.
		logx.Warnf("batch delete: load targets: %v", err)
		flashStatus(w, http.StatusInternalServerError, "Could not load the selected images.")
		return
	}
	defer func() { _ = rows.Close() }()
	byID := make(map[int64]search.DeleteTarget, len(ids))
	for rows.Next() {
		var t search.DeleteTarget
		var isMissing int
		if err := rows.Scan(&t.ID, &t.CanonicalPath, &t.FolderPath, &isMissing); err != nil {
			continue
		}
		t.IsMissing = isMissing == 1
		byID[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		logx.Warnf("batch delete: scan targets: %v", err)
		flashStatus(w, http.StatusInternalServerError, "Could not load the selected images.")
		return
	}
	targets := make([]search.DeleteTarget, 0, len(ids))
	for _, id := range ids {
		if t, ok := byID[id]; ok {
			targets = append(targets, t)
		}
	}
	s.startBulkDelete(w, targets)
}

func (s *Server) deleteSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	queryStr := r.FormValue("q")

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		logx.Warnf("delete-search parse: %v", parseErr)
		flashStatus(w, http.StatusBadRequest, "Could not parse search: "+parseErr.Error())
		return
	}
	expr = resolveCeiling(r, s.Active()).Apply(expr)

	// Stream the matching targets off the cursor so very large result sets
	// don't allocate a second intermediate copy on top of whatever the
	// bulk-delete worker holds.
	var targets []search.DeleteTarget
	err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
		targets = append(targets, t)
		return nil
	})
	if err != nil {
		logx.Errorf("delete-search: %v", err)
		flashStatus(w, http.StatusInternalServerError, "Search error.")
		return
	}

	s.startBulkDelete(w, targets)
}

// startBulkDelete kicks off a background delete job for the given targets and
// writes the response. The job reports progress via jobs.Manager; the client
// sees the running state in the top-right status bar.
func (s *Server) startBulkDelete(w http.ResponseWriter, targets []search.DeleteTarget) {
	if len(targets) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.startJob(w, models.JobTypeDelete) {
		return
	}
	go s.runBulkDelete(targets)
	w.WriteHeader(http.StatusAccepted)
}

// runBulkDelete processes targets in chunks with one transaction per chunk.
// The images schema cascades image_tags / image_paths / sd_metadata /
// comfyui_metadata on image delete, so a single DELETE FROM images clears the
// dependent rows. dup_groups.original_image_id has no CASCADE, so the
// relations hook runs first to promote or dissolve, exactly as the
// single-image delete does. Tag usage counts are reconciled at the end by a
// targeted recalc scoped to the tag IDs actually touched by the cascade
// (collected from image_tags before the DELETE), avoiding a full-table Recalc
// that would walk every tag in the library.
// recalcAffectedTags reconciles usage counts for the tags a batch touched,
// reporting the step on the job bar. logCtx names the caller in the warning.
func (s *Server) recalcAffectedTags(affected []int64, processed, total int, logCtx string) {
	if len(affected) == 0 {
		return
	}
	s.jobs.Update(processed, total, "reconciling tag counts…")
	if err := s.tagSvc().RecalcIDs(affected); err != nil {
		logx.Warnf("%s recalc IDs: %v", logCtx, err)
	}
}

func (s *Server) runBulkDelete(targets []search.DeleteTarget) {
	ctx := s.jobs.Context()
	total := len(targets)
	byID := make(map[int64]search.DeleteTarget, len(targets))
	ids := make([]int64, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.ID)
		byID[t.ID] = t
	}

	s.jobs.Update(0, total, "deleting…")
	done := 0
	onDelete := s.onImagesDeleteCallback()
	affectedTags, processed, cancelled, err := s.tagSvc().ChunkedDeleteWithTagRecalc(
		ctx, ids, "", nil,
		func(tx *sql.Tx, chunk []int64, placeholders string, args []any) error {
			if onDelete != nil {
				if err := onDelete(tx, chunk); err != nil {
					return err
				}
			}
			_, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...)
			return err
		},
		func(chunk []int64) {
			for _, id := range chunk {
				t := byID[id]
				gallery.RemoveImageArtifacts(s.thumbnailsPath(), id, "")
				if !t.IsMissing {
					gallery.UnlinkImageFile(s.galleryPath(), t.CanonicalPath, id)
				}
			}
			done += len(chunk)
			s.jobs.Update(done, total, "deleting…")
		},
	)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	s.recalcAffectedTags(affectedTags, processed, total, "bulk delete")

	if processed > 0 {
		s.Active().InvalidateCaches()
	}
	s.finishJob(nil, cancelled, fmt.Sprintf("delete cancelled (%d/%d deleted)", processed, total), fmt.Sprintf("Deleted %d image(s).", processed))
}

// batchMove kicks off a background `move` job that relocates the selected
// image IDs into the requested folder. Collisions on filename auto-suffix via
// UniqueDestPath. The watcher suppresses its events while this job runs so
// the Rename pairs don't flap the images as missing in transit.
//
// scope=search materialises ids by streaming the search result through
// search.ExecuteForDeleteStream (same idiom as batchTag and deleteSearchPost);
// scope=selection (or empty) reads ids[] from the form.
func (s *Server) batchMove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	targetFolder := strings.TrimSpace(r.FormValue("folder"))

	// Validate the folder once up-front so the user sees the error inline
	// rather than as a per-image log entry once the job starts.
	if _, err := gallery.ResolveSubdir(s.galleryPath(), targetFolder); err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}

	s.startScopedJob(w, r, "batch-move", models.JobTypeMove, func(ids []int64) {
		s.runBatchMove(ids, targetFolder)
	})
}

// runPerImageJob walks ids one at a time, applying op and counting the
// per-image failures rather than stopping on them - a single unreadable
// file must not strand the rest of the scope. Progress lands every 25
// images; the caches are dropped only when something actually changed.
// perImageLoop walks ids with per-image error isolation, reporting
// progress every 25th and on the last. The callers own the completion
// summary: they differ in which caches to invalidate and what to name
// the result.
func (s *Server) perImageLoop(ids []int64, verb, gerund string, op func(i int, id int64) error) (done, failed int, cancelled bool) {
	ctx := s.jobs.Context()
	total := len(ids)
	s.jobs.Update(0, total, gerund+"…")
	for i, id := range ids {
		if ctx.Err() != nil {
			return done, failed, true
		}
		if err := op(i, id); err != nil {
			logx.Warnf("batch %s %d: %v", verb, id, err)
			failed++
			continue
		}
		done++
		if (i+1)%25 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, gerund+"…")
		}
	}
	return done, failed, false
}

func (s *Server) runPerImageJob(ids []int64, verb, gerund, past string, op func(i int, id int64) error) {
	total := len(ids)
	done, failed, cancelled := s.perImageLoop(ids, verb, gerund, op)

	if done > 0 {
		s.Active().InvalidateCaches()
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d %s)", verb, done, total, past))
		return
	}
	title := strings.ToUpper(past[:1]) + past[1:]
	summary := fmt.Sprintf("%s %d image(s).", title, done)
	if failed > 0 {
		summary = fmt.Sprintf("%s %d image(s), %d failed.", title, done, failed)
	}
	s.jobs.Complete(summary)
}

// runBatchMove relocates every target into one folder. Each MoveImage
// has its own small write txn + Rename.
func (s *Server) runBatchMove(ids []int64, targetFolder string) {
	s.runPerImageJob(ids, "move", "moving", "moved", func(_ int, id int64) error {
		_, err := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder)
		return err
	})
}

// batchRename kicks off a background `move` job that renames the selected
// image files to a numbered sequence off the given base name. It rides the
// move job type for the same watcher suppression as batchMove.
func (s *Server) batchRename(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	base := strings.TrimSpace(r.FormValue("name"))
	if base == "" || base != filepath.Base(base) || base == "." || base == ".." {
		flashStatus(w, http.StatusBadRequest, "Base name required (no path separators).")
		return
	}

	s.startScopedJob(w, r, "batch-rename", models.JobTypeMove, func(ids []int64) {
		s.runBatchRename(ids, base)
	})
}

// runBatchRename renames the scope to a numbered sequence, zero-padded
// to the width the whole run needs so the names sort in one order.
// Collisions still auto-suffix inside RenameImage; per-image failures
// are logged and counted like batch moves.
func (s *Server) runBatchRename(ids []int64, base string) {
	width := max(len(strconv.Itoa(len(ids))), 2)
	s.runPerImageJob(ids, "rename", "renaming", "renamed", func(i int, id int64) error {
		_, err := gallery.RenameImage(s.db(), s.galleryPath(), id, fmt.Sprintf("%s%0*d", base, width, i+1))
		return err
	})
}

// batchTag kicks off a background `tag` job that adds (op=add) or removes
// (op=remove) a tag set across either every image in the current search
// (scope=search) or just the checked ids (scope=selection). The dialogs in
// gallery.html post the tags string verbatim (parsed server-side so
// category:name and quoted spans behave identically to the detail-page
// tag input). The op=remove path is the "specific tags by name" branch of
// #batch-strip-dialog; the bulk user/auto/all branches go through batchStrip.
func (s *Server) batchTag(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	op := strings.TrimSpace(r.FormValue("op"))
	if op != "add" && op != "remove" {
		flashStatus(w, http.StatusBadRequest, "op must be add or remove")
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tags"))
	if tagInput == "" {
		flashStatus(w, http.StatusBadRequest, "No tags provided.")
		return
	}
	catTags, parseErrMsg := s.parseTagInput(tagInput)
	if parseErrMsg != "" {
		flashStatus(w, http.StatusBadRequest, parseErrMsg)
		return
	}
	if len(catTags) == 0 {
		flashStatus(w, http.StatusBadRequest, "No tags to apply.")
		return
	}

	s.startScopedJob(w, r, "batch-tag", models.JobTypeTag, func(ids []int64) {
		s.runBatchTag(ids, op, catTags)
	})
}

// anyTagHasImplications reports whether any of the supplied tag ids
// appears as a parent in tag_implications. Used by runBatchTag to pick
// a smaller chunk size when the fan-out closure would otherwise pin
// the writer for tens of seconds per 500-row chunk.
func (s *Server) anyTagHasImplications(tagIDs []int64) bool {
	if len(tagIDs) == 0 {
		return false
	}
	placeholders, args := db.InPlaceholders(tagIDs)
	var n int
	if err := s.db().Read.QueryRow(
		`SELECT 1 FROM tag_implications WHERE parent_tag_id IN (`+placeholders+`) LIMIT 1`,
		args...,
	).Scan(&n); err != nil {
		return false
	}
	return n == 1
}

// runBatchTag resolves each (catID, name) token to a tag id once up front
// (creating new tags on add, looking up only existing ones on remove) and
// applies the resolved set to every image in turn. Cancellable via the
// shared job context, identical to runBulkDelete's pattern.
func (s *Server) runBatchTag(ids []int64, op string, catTags []catTag) {
	type resolvedTag struct {
		id   int64
		name string
	}
	var resolved []resolvedTag
	if op == "add" {
		for _, ct := range catTags {
			t, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
			if err != nil {
				logx.Warnf("batch-tag get-or-create %q: %v", ct.name, err)
				continue
			}
			resolved = append(resolved, resolvedTag{t.ID, t.Name})
		}
	} else {
		for _, ct := range catTags {
			var id int64
			err := s.db().Read.QueryRow(
				`SELECT id FROM tags WHERE name = ? AND category_id = ?`, ct.name, ct.catID,
			).Scan(&id)
			if err != nil {
				continue // unknown tag; nothing to remove
			}
			resolved = append(resolved, resolvedTag{id, ct.name})
		}
	}
	if len(resolved) == 0 {
		s.jobs.Complete(fmt.Sprintf("nothing to %s (no matching tags)", op))
		return
	}

	label, summary := "tagging", "Tagged"
	if op == "remove" {
		label, summary = "untagging", "Untagged"
	}

	ctx := s.jobs.Context()
	total := len(ids)
	applied := 0
	reRated := 0

	tagIDs := make([]int64, 0, len(resolved))
	for _, t := range resolved {
		tagIDs = append(tagIDs, t.id)
	}

	// Chunk size compresses to 100 when any resolved tag carries
	// implications so the per-row fan-out cost in addTagToImageTxReportingDup
	// doesn't hold the writer for tens of seconds on a 500-row chunk.
	// The 500-row default still applies to bare-add jobs where the
	// per-row work is just an INSERT OR IGNORE + usage_count bump.
	chunkSize := 500
	if op == "add" && s.anyTagHasImplications(tagIDs) {
		chunkSize = 100
	}
	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, label, func(chunk []int64) error {
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		var n, replaced int
		if op == "add" {
			n, replaced, err = s.tagSvc().BatchAddTagsTx(tx, chunk, tagIDs)
		} else {
			n, err = s.tagSvc().BatchRemoveTagsTx(tx, chunk, tagIDs)
		}
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		applied += n
		reRated += replaced
		return nil
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	if err := s.tagSvc().RecalcIDs(tagIDs); err != nil {
		logx.Warnf("batch-tag recalc IDs: %v", err)
	}
	s.Active().InvalidateCaches()

	done := fmt.Sprintf("%s %d image(s) (%d row change(s)).", summary, processed, applied)
	if reRated > 0 {
		done += fmt.Sprintf(" Replaced the rating on %d image(s).", reRated)
	}
	s.finishJob(nil, cancelled, fmt.Sprintf("%s cancelled (%d/%d processed)", label, processed, total), done)
}

// batchStrip kicks off a background `tag` job that strips tags by category
// (mode=user|auto|all) across either every image in the current search
// (scope=search) or the checked ids (scope=selection). Mirrors batchTag's
// scope dispatch; the per-mode predicate decides which image_tags rows the
// chunked DELETE in runBatchStrip touches. When mode=auto and tagger_name is
// set, the strip is further scoped to that tagger's output rows.
func (s *Server) batchStrip(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	switch mode {
	case "user", "auto", "all", "source", "source-all", "stale":
	default:
		flashStatus(w, http.StatusBadRequest, "mode must be user, auto, all, source, source-all, or stale")
		return
	}
	// filterName narrows mode=auto to one tagger's output and mode=source to
	// one site's tags; the bulk modes carry no name.
	var filterName string
	switch mode {
	case "auto":
		filterName = strings.TrimSpace(r.FormValue("tagger_name"))
	case "source":
		filterName = strings.TrimSpace(r.FormValue("source"))
		if filterName == "" {
			flashStatus(w, http.StatusBadRequest, "pick a source")
			return
		}
	}

	s.startScopedJob(w, r, "batch-strip", models.JobTypeTag, func(ids []int64) {
		s.runBatchStrip(ids, mode, filterName)
	})
}

// runBatchStrip processes targets in chunks of 500 with one transaction per
// chunk. The per-chunk pattern collects the distinct touched tag_ids before
// the DELETE so the post-pass RecalcIDs is scoped to the tags that
// actually changed (mirrors runBulkDelete). modePredicate narrows the strip:
//
//	user       → AND is_auto = 0 AND is_implied = 0 AND (tagger_name IS NULL OR '')
//	auto       → AND is_auto = 1              (+ AND tagger_name = ? when scoped)
//	source     → AND is_auto = 0 AND tagger_name = ?
//	source-all → AND is_auto = 0 AND tagger_name <> '' AND tagger_name IS NOT NULL
//	stale      → AND stale = 1
//	all        → (no extra predicate)
func (s *Server) runBatchStrip(ids []int64, mode, filterName string) {
	var modePredicate, label, summary string
	var extraArgs []any
	switch mode {
	case "user":
		modePredicate = ` AND is_auto = 0 AND is_implied = 0 AND (tagger_name IS NULL OR tagger_name = '')`
		label, summary = "removing user tags", "Removed user tags from"
	case "auto":
		modePredicate = ` AND is_auto = 1`
		if filterName != "" {
			modePredicate += ` AND tagger_name = ?`
			extraArgs = append(extraArgs, filterName)
			label = fmt.Sprintf("removing %s auto-tags", filterName)
			summary = fmt.Sprintf("Removed %s auto-tags from", filterName)
		} else {
			label, summary = "removing auto-tags", "Removed auto-tags from"
		}
	case "source":
		modePredicate = ` AND is_auto = 0 AND tagger_name = ?`
		extraArgs = append(extraArgs, filterName)
		label = fmt.Sprintf("removing %s tags", filterName)
		summary = fmt.Sprintf("Removed %s tags from", filterName)
	case "source-all":
		modePredicate = ` AND is_auto = 0 AND tagger_name <> '' AND tagger_name IS NOT NULL`
		label, summary = "removing source tags", "Removed source tags from"
	case "stale":
		modePredicate = ` AND stale = 1`
		label, summary = "removing stale tags", "Removed stale tags from"
	case "all":
		modePredicate = ``
		label, summary = "removing tags", "Removed all tags from"
	}

	ctx := s.jobs.Context()
	total := len(ids)
	s.jobs.Update(0, total, fmt.Sprintf("%s…", label))
	done := 0
	removed := int64(0)
	affectedTags, processed, cancelled, err := s.tagSvc().ChunkedDeleteWithTagRecalc(
		ctx, ids, modePredicate, extraArgs,
		func(tx *sql.Tx, _ []int64, placeholders string, args []any) error {
			res, err := tx.Exec(
				`DELETE FROM image_tags WHERE image_id IN (`+placeholders+`)`+modePredicate, args...)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			removed += n
			return err
		},
		func(chunk []int64) {
			done += len(chunk)
			s.jobs.Update(done, total, fmt.Sprintf("%s…", label))
		},
	)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	// The predicate DELETE only touches the rows it matches, so implied rows
	// whose last parent it just took have to be swept after it.
	if !cancelled {
		orphanTags, orphans, err := s.tagSvc().PruneOrphanedImplied(ctx, ids)
		if err != nil {
			logx.Warnf("batch-strip prune implied: %v", err)
		}
		affectedTags = append(affectedTags, orphanTags...)
		removed += int64(orphans)
	}

	s.recalcAffectedTags(affectedTags, processed, total, "batch-strip")
	s.Active().InvalidateCaches()

	s.finishJob(nil, cancelled, fmt.Sprintf("%s cancelled (%d/%d processed)", label, processed, total),
		fmt.Sprintf("%s %d image(s) (%d row change(s)).", summary, processed, removed))
}

// batchInbox kicks off a background `tag` job that flips is_inbox across
// every image in the current search (scope=search) or the checked ids
// (scope=selection). The op is always a per-row toggle: inbox rows
// become archived, archived become inbox. Mirrors batchTag's scope
// dispatch and runBulkDelete's chunked-tx shape.
// The job processes ids in chunks of 500 with one transaction per
// chunk. SQLite's `1 - is_inbox` does the per-row toggle in a single
// UPDATE so a mixed selection (some inbox, some archived) ends up
// cleanly inverted.
func (s *Server) batchInbox(w http.ResponseWriter, r *http.Request) {
	s.startScopedJob(w, r, "batch-inbox", models.JobTypeTag, func(ids []int64) {
		s.runBulkToggle(ids, "is_inbox", "inbox state", "inbox toggle", "Toggled inbox state")
	})
}

// batchFavorite mirrors batchInbox for the is_favorited column: a
// per-row toggle that flips favorited rows to unfavorited and vice
// versa across the resolved scope.
func (s *Server) batchFavorite(w http.ResponseWriter, r *http.Request) {
	s.startScopedJob(w, r, "batch-favorite", models.JobTypeTag, func(ids []int64) {
		s.runBulkToggle(ids, "is_favorited", "favorite state", "favorite toggle", "Toggled favorite state")
	})
}

// startScopedJob is the HTTP shell shared by every batch handler: parse
// form, resolve scope, claim the jobs lane, spawn, 202. Callers validate
// their own fields first (ParseForm is idempotent).
func (s *Server) startScopedJob(w http.ResponseWriter, r *http.Request, scopeLabel, jobType string, run func([]int64)) {
	if !parseFormOK(w, r) {
		return
	}
	ids, ok := s.resolveBatchScope(w, r, scopeLabel)
	if !ok {
		return
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.startJob(w, jobType) {
		return
	}
	go run(ids)
	w.WriteHeader(http.StatusAccepted)
}

// runBulkToggle flips the named INTEGER column on every id via
// SQLite's `1 - col` toggle, chunked at 500 ids per write tx.
// progress/cancel/successNoun fill the per-chunk and completion
// summaries.
func (s *Server) runBulkToggle(ids []int64, column, progressNoun, cancelNoun, successNoun string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)

	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, "toggling "+progressNoun, func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE images SET `+column+` = 1 - `+column+` WHERE id IN (`+placeholders+`)`, args...,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	s.Active().InvalidateCaches()

	s.finishJob(nil, cancelled, fmt.Sprintf("%s cancelled (%d/%d toggled)", cancelNoun, processed, total), fmt.Sprintf("%s for %d image(s).", successNoun, processed))
}

// batchCollection adds or removes a collection label across every image
// in `scope=search` (q + sort + order) or every checked id in
// `scope=selection`. `mode=add` (default) files each image under the
// label, keeping any other memberships; `mode=remove` drops the label.
// One indexed write per 500-row chunk.
func (s *Server) batchCollection(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	collectionVal := strings.TrimSpace(r.FormValue("collection"))
	if collectionVal == "" {
		flashStatus(w, http.StatusBadRequest, "Collection label required.")
		return
	}
	if len(collectionVal) > maxExternalSourceLen {
		flashStatus(w, http.StatusBadRequest, "Collection label too long.")
		return
	}
	mode := r.FormValue("mode")
	if mode != "remove" {
		mode = "add"
	}

	s.startScopedJob(w, r, "batch-collection", models.JobTypeTag, func(ids []int64) {
		s.runBatchCollection(ids, collectionVal, mode)
	})
}

// runBatchCollection adds or removes the label across the supplied id
// list in chunks. Add keeps existing memberships (a row with no home
// adopts the label); remove drops the membership and promotes another to
// home (or clears the mirror) for rows whose home was the removed label.
func (s *Server) runBatchCollection(ids []int64, label, mode string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	remove := mode == "remove"
	verb := "adding to collection"
	if remove {
		verb = "removing from collection"
	}

	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, verb, func(chunk []int64) error {
		placeholders, chunkArgs := db.InPlaceholders(chunk)
		labelArgs := append([]any{label}, chunkArgs...)
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if remove {
			if _, err := tx.Exec(
				`DELETE FROM image_collections WHERE name = ? AND image_id IN (`+placeholders+`)`,
				labelArgs...,
			); err != nil {
				return err
			}
			// Rebind the home mirror for rows whose home was the removed label.
			if _, err := tx.Exec(
				`UPDATE images SET
				   series = COALESCE((SELECT name FROM image_collections c WHERE c.image_id = images.id
				                      ORDER BY c.position IS NULL, c.position, c.name LIMIT 1), ''),
				   series_order = (SELECT position FROM image_collections c WHERE c.image_id = images.id
				                   ORDER BY c.position IS NULL, c.position, c.name LIMIT 1)
				 WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
				labelArgs...,
			); err != nil {
				return err
			}
			return tx.Commit()
		}
		if _, err := tx.Exec(
			`INSERT INTO image_collections (image_id, name, position)
			 SELECT id, ?, NULL FROM images WHERE id IN (`+placeholders+`)
			 ON CONFLICT(image_id, name) DO NOTHING`,
			labelArgs...,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE images SET series = ?, series_order = NULL
			 WHERE series = '' AND id IN (`+placeholders+`)`,
			labelArgs...,
		); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("collection cancelled (%d/%d processed)", processed, total))
		return
	}
	if remove {
		s.jobs.Complete(fmt.Sprintf("Removed %d image(s) from collection.", processed))
		return
	}
	s.jobs.Complete(fmt.Sprintf("Added %d image(s) to collection.", processed))
}

// batchLookup fans one of three unrelated operations across `scope=search`
// or `scope=selection`: mode=refresh re-fetches every declared source,
// mode=hash discovers new ones by file hash (ptr / booru pick the backends,
// unsourced narrows the scope to the images that could gain a source), and
// mode=schedule sets the per-image opt-in the nightly run reads. The action
// is hidden unless monloader is paired.
func (s *Server) batchLookup(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	opts := batchLookupOpts{
		mode:      r.FormValue("mode"),
		ptr:       r.FormValue("ptr") == "1",
		booru:     r.FormValue("booru") == "1",
		unsourced: r.FormValue("unsourced") == "1",
		on:        r.FormValue("on") == "1",
	}
	switch opts.mode {
	case "refresh", "schedule":
	case "hash":
		if !opts.ptr && !opts.booru {
			http.Error(w, "pick at least one lookup backend", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "unknown lookup mode", http.StatusBadRequest)
		return
	}
	s.startScopedJob(w, r, "batch-lookup", models.JobTypeTag, func(ids []int64) {
		s.runBatchLookup(opts, ids)
	})
}

// batchLookupOpts is one dialog submission.
type batchLookupOpts struct {
	mode       string
	ptr, booru bool
	unsourced  bool
	on         bool
}

// batchLookupCount answers the dialog's scope split so its live summary can
// price each operation. The refresh branch is only worth firing for the
// images that already carry a source, and the hash branch only for the ones
// that do not; without the split the dialog would have to guess.
func (s *Server) batchLookupCount(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids, ok := s.resolveBatchScope(w, r, "batch-lookup-count")
	if !ok {
		return
	}
	// The dialog opens on the whole visible library often enough that a
	// chunked EXISTS probe means thousands of round trips before the
	// operator has committed to anything. Read the sourced ids once
	// instead - the set is bounded by how many images carry a source,
	// not by the scope - and intersect in memory.
	withURL, err := db.QueryIDs(s.db().Read,
		`SELECT DISTINCT image_id FROM image_sources WHERE url <> ''`)
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, "Count failed.")
		return
	}
	slices.Sort(withURL)
	sourced := 0
	for _, id := range ids {
		if _, found := slices.BinarySearch(withURL, id); found {
			sourced++
		}
	}
	writePairJSON(w, http.StatusOK, map[string]int{"total": len(ids), "sourced": sourced, "unsourced": len(ids) - sourced})
}

// runBatchLookup applies one dialog submission across the scope, stopping
// early if monloader becomes unreachable (each enqueue is a bounded LAN call,
// so this stays a foreground-light background job). Images without the needed
// key - a source url, a readable file to md5 - are skipped and counted.
//
// A hash lookup here takes the background lane but is never budgeted: bulk
// work whoever started it, so it belongs behind anything a person is waiting
// on, but it is also a deliberate operator action, so the nightly budget must
// not refuse it. The PTR half rides the batch endpoint like the scheduled
// phase and enqueues nothing at all.
func (s *Server) runBatchLookup(opts batchLookupOpts, ids []int64) {
	ctx := s.jobs.Context()
	// Snapshot the gallery once so every row read and enqueue in this job
	// stays consistent even if a switch is attempted concurrently.
	cx := s.Active()
	if cx == nil {
		s.jobs.Fail("no active gallery")
		return
	}
	if opts.mode == "schedule" {
		s.runBatchSchedule(ctx, cx, opts.on, ids)
		return
	}
	if opts.mode == "hash" && opts.unsourced {
		ids = s.unsourcedOnly(cx, ids)
	}

	enqueued, skipped, matched := 0, 0, 0
	ptrRefused := false
	total := len(ids)
	s.jobs.Update(0, total, "looking up…")
	if opts.mode == "hash" && opts.ptr {
		var err error
		if matched, err = s.batchPTRLookup(ctx, cx, ids); err != nil {
			if !errors.Is(err, errPTRUnavailable) && !errors.Is(err, errPTRBatchUnsupported) {
				s.jobs.Fail("monloader unreachable: " + err.Error())
				return
			}
			// monloader is the authority on whether it has an index to ask;
			// the images were not skipped, the backend was simply not there.
			ptrRefused = true
		}
	}
	// A PTR-only hash lookup was finished by the batch call above, so the
	// per-image walk - one row read each - would have nothing to do with what
	// it read.
	if opts.booru || opts.mode == "refresh" {
		for i, id := range ids {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("Lookup cancelled after queueing %d.", enqueued))
				return
			}
			if (i+1)%25 == 0 || i == total-1 {
				s.jobs.Update(i+1, total, "looking up…")
			}
			var canonPath, sha string
			if err := cx.DB.Read.QueryRow(
				`SELECT canonical_path, sha256 FROM images WHERE id = ?`, id,
			).Scan(&canonPath, &sha); err != nil {
				continue
			}
			var err error
			switch opts.mode {
			case "refresh":
				queued, qErr := s.enqueueSourceFetches(ctx, cx, id, sha)
				if qErr != nil {
					if isPeerStatusErr(qErr) {
						skipped++
						continue
					}
					s.jobs.Fail("monloader unreachable: " + qErr.Error())
					return
				}
				if queued == 0 {
					skipped++
				} else {
					enqueued += queued
				}
				continue
			default:
				md5, hashErr := gallery.Md5File(canonPath)
				if hashErr != nil {
					skipped++
					continue
				}
				var jobID int64
				if jobID, err = s.EnqueueHashLookup(ctx, id, cx.Name, lookup.BackendBooru, md5, sha, true, false); err == nil {
					s.recordLookupEnqueued(cx, id, lookup.BackendBooru, jobID)
				}
			}
			if err != nil {
				// A per-request refusal (a bad hash) skips the row; only a
				// transport failure means monloader is truly unreachable.
				if isPeerStatusErr(err) {
					skipped++
					continue
				}
				s.jobs.Fail("monloader unreachable: " + err.Error())
				return
			}
			enqueued++
		}
	}
	cx.InvalidateCaches()
	if opts.mode == "refresh" {
		s.jobs.Complete(fmt.Sprintf("Queued %d source fetch(es) on monloader; skipped %d without a fetchable source.", enqueued, skipped))
		return
	}
	parts := []string{}
	switch {
	case ptrRefused:
		parts = append(parts, "the PTR is unavailable on monloader")
	case opts.ptr:
		parts = append(parts, fmt.Sprintf("%d matched in the PTR", matched))
	}
	if opts.booru {
		parts = append(parts, fmt.Sprintf("%d queued on monloader", enqueued))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	s.jobs.Complete(fmt.Sprintf("Looked up %d image(s): %s.", total, strings.Join(parts, ", ")))
}

// unsourcedOnly narrows a scope to the images that could still gain a
// source, which is what makes "Find tags" over a big search affordable.
func (s *Server) unsourcedOnly(cx *galleryCtx, ids []int64) []int64 {
	var out []int64
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		placeholders, args := db.InPlaceholders(chunk)
		found, err := db.QueryIDs(cx.DB.Read,
			`SELECT i.id FROM images i WHERE i.id IN (`+placeholders+`)
			   AND NOT EXISTS (SELECT 1 FROM image_sources s WHERE s.image_id = i.id AND s.url <> '')`,
			args...)
		if err != nil {
			logx.Warnf("batch lookup scope narrowing: %v", err)
			return ids
		}
		out = append(out, found...)
	}
	return out
}

// batchPTRLookup runs the scope through monloader's batch PTR endpoint in
// chunks and applies every hit, recording each hash's outcome. Returns how
// many matched.
func (s *Server) batchPTRLookup(ctx context.Context, cx *galleryCtx, ids []int64) (int, error) {
	g, ok := s.apiResolver(cx.Name)
	if !ok {
		return 0, errors.New("no active gallery")
	}
	matched := 0
	for start := 0; start < len(ids) && ctx.Err() == nil; start += ptrLookupChunk {
		chunk := ids[start:min(start+ptrLookupChunk, len(ids))]
		placeholders, args := db.InPlaceholders(chunk)
		hashes, err := db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (ptrLookupImage, error) {
			var v ptrLookupImage
			err := rows.Scan(&v.ImageID, &v.SHA256)
			return v, err
		}, `SELECT id, sha256 FROM images WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			return matched, err
		}
		byHash := make(map[string]int64, len(hashes))
		images := make([]ptrLookupImage, 0, len(hashes))
		for _, h := range hashes {
			byHash[h.SHA256] = h.ImageID
			images = append(images, h)
		}
		if len(images) == 0 {
			continue
		}
		results, cursor, err := s.ptrBatchLookup(ctx, cx.Name, false, images)
		if err != nil {
			return matched, err
		}
		// A record failure is logged inside and does not abort the batch: the
		// scope is bounded and the operator asked for all of it.
		hits, _, _ := applyPTRResults(g, cx, byHash, results, cursor)
		matched += hits
	}
	return matched, nil
}

// runBatchSchedule sets the per-image scheduled-lookup opt-in across the
// scope. Turning it on resets the ladder too, so this is the bulk equivalent
// of the detail page's [look again] over a `lookup:exhausted` search. Both
// backends move together: the per-backend choice belongs to the one image an
// operator is looking at, not to a scope they picked in bulk.
func (s *Server) runBatchSchedule(ctx context.Context, cx *galleryCtx, on bool, ids []int64) {
	flag := 0
	if on {
		flag = 1
	}
	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, 500, "setting scheduled lookup", func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		if _, err := cx.DB.Write.Exec(
			`UPDATE images SET scheduled_lookup = ?, scheduled_lookup_ptr = ? WHERE id IN (`+placeholders+`)`,
			append([]any{flag, flag}, args...)...); err != nil {
			return err
		}
		if !on {
			return nil
		}
		return lookup.ResetMany(cx.DB.Write, placeholders, args, time.Now())
	})
	cx.InvalidateCaches()
	switch {
	case err != nil:
		s.jobs.Fail(err.Error())
	case cancelled:
		s.jobs.Complete(fmt.Sprintf("Scheduled lookup cancelled (%d/%d set)", processed, len(ids)))
	case on:
		s.jobs.Complete(fmt.Sprintf("Scheduled lookup turned on for %d image(s).", processed))
	default:
		s.jobs.Complete(fmt.Sprintf("Scheduled lookup turned off for %d image(s).", processed))
	}
}

// enqueueSourceFetches queues one metadata refetch per declared origin url of
// the image, plus a PTR hash lookup for a url-less "ptr" origin. Duplicate
// urls collapse to one fetch; a PTR-unavailable answer skips that origin
// rather than failing the batch. Returns how many jobs were queued.
func (s *Server) enqueueSourceFetches(ctx context.Context, cx *galleryCtx, id int64, sha string) (int, error) {
	galleryName := cx.Name
	type origin struct{ site, url string }
	// Fetching the origins a partial read did reach would queue less work than
	// the summary claims, with nothing saying so.
	origins, err := db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (origin, error) {
		var o origin
		err := rows.Scan(&o.site, &o.url)
		return o, err
	}, `SELECT site, url FROM image_sources WHERE image_id = ? ORDER BY rowid`, id)
	if err != nil {
		logx.Warnf("source fetch origins for image %d: %v", id, err)
		return 0, nil
	}

	queued := 0
	seenURL := map[string]bool{}
	ptrDone := false
	for _, o := range origins {
		switch {
		case strings.TrimSpace(o.url) != "":
			u := strings.TrimSpace(o.url)
			if seenURL[u] {
				continue
			}
			seenURL[u] = true
			if err := s.EnqueueMetadataFetch(ctx, id, galleryName, u); err != nil {
				return queued, err
			}
		case strings.EqualFold(strings.TrimSpace(o.site), lookup.BackendPTR):
			if ptrDone {
				continue
			}
			ptrDone = true
			jobID, err := s.EnqueueHashLookup(ctx, id, galleryName, lookup.BackendPTR, "", sha, true, false)
			if err != nil {
				if errors.Is(err, errPTRUnavailable) {
					continue
				}
				return queued, err
			}
			s.recordLookupEnqueued(cx, id, lookup.BackendPTR, jobID)
		default:
			continue
		}
		queued++
	}
	return queued, nil
}

func (s *Server) deleteFolderPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	folderPath := r.FormValue("folder")

	if folderPath == "" {
		http.Error(w, "invalid folder path", http.StatusBadRequest)
		return
	}

	// Reuse the gallery-root validator from the upload path: filepath.Rel
	// rejects sibling directories that share the gallery prefix (e.g.
	// `/data/gallery_backup`) without false-positiving on `foo..bar`.
	absPath, err := gallery.ResolveSubdir(s.galleryPath(), folderPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.Remove(absPath); err != nil {
		// Treat "already gone" as success so a stale UI can re-issue the
		// delete without an error toast. ENOTEMPTY (raised by Linux when
		// the directory still has children) maps to the same 409 the UI
		// already surfaces. Anything else is a real failure - permission
		// denied, busy, etc. - and must not silently masquerade as a
		// successful redirect.
		switch {
		case os.IsNotExist(err):
			// nothing to do - fall through to the success redirect
		case errors.Is(err, syscall.ENOTEMPTY):
			http.Error(w, "directory not empty", http.StatusConflict)
			return
		default:
			logx.Warnf("delete folder %q: %v", absPath, err)
			http.Error(w, "could not delete folder: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	hxRedirect(w, r, "/")
}
