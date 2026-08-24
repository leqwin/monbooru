// Monbooru is a Linux-only deployment; path handling assumes forward slashes.
package gallery

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/tags"
)

// SyncResult summarizes the outcome of a gallery sync.
type SyncResult struct {
	Added      int
	Removed    int
	Moved      int
	Duplicates int
	// Conflicts counts paths whose rewritten bytes already belong to
	// another row, which no reconcile arm can apply.
	Conflicts int
}

// Summary renders the run for the job status bar. Conflicts only appear
// when there are any: the operator has to act on those, unlike the counts
// that describe work the sync already did.
func (r SyncResult) Summary() string {
	out := fmt.Sprintf("%d added, %d missing, %d moved", r.Added, r.Removed, r.Moved)
	if r.Conflicts > 0 {
		out += fmt.Sprintf(", %d conflicted", r.Conflicts)
	}
	return out
}

// FolderNode represents a folder in the gallery tree.
type FolderNode struct {
	Path     string
	Name     string
	Count    int
	Depth    int
	Children []FolderNode
}

// SourceLabelCount is one row of the sidebar's Sources section: a site
// label from image_sources and the count of non-missing images carrying
// it as any origin, matching the any-membership `source:` filter.
type SourceLabelCount struct {
	Source string
	Count  int
}

// SourceLabelCountsQuery returns the top site labels (by image count
// desc, then alphabetical) with no rating ceiling applied.
func SourceLabelCountsQuery(database *db.DB, limit int) ([]SourceLabelCount, error) {
	return SourceLabelCountsUnderQuery(database, limit, nil)
}

// syncFileInfo is one walk-result entry: the on-disk path plus the SHA
// either taken from the (size+mtime)-unchanged shortcut or freshly
// hashed during the walk.
type syncFileInfo struct {
	path      string
	sha256    string
	md5       string // "" when the unchanged-shortcut reused the stored sha instead of re-hashing
	size      int64
	mtime     int64
	mtimeNano int64
}

// syncKnownEntry is one image_paths row preloaded for the unchanged-
// shortcut. The Phase 1 walker keys on (size, mtime); a hit avoids the
// re-hash on every untouched file.
type syncKnownEntry struct {
	size      int64
	sha256    string
	mtime     int64
	mtimeNano int64
}

// unchanged reports whether the recorded stamp still describes the file.
// The nanosecond one decides when the row carries it; a row written before
// that column falls back to the second, which cannot tell an edit that
// landed in the same second the file was last observed.
func (k syncKnownEntry) unchanged(size, mtime, mtimeNano int64) bool {
	if k.size != size {
		return false
	}
	if k.mtimeNano != 0 {
		return k.mtimeNano == mtimeNano
	}
	return k.mtime != 0 && k.mtime == mtime
}

// syncBySHARow is one images row preloaded for the reconcile lookup.
// One full scan beats N indexed SELECTs on a 25k-image library.
type syncBySHARow struct {
	id            int64
	canonicalPath string
	isMissing     int
}

// Sync runs the three-phase gallery sync (walk, reconcile, mark-missing).
// progress receives (processed, total, message) tuples shaped to match
// jobs.Manager.Update so the handler can forward the call verbatim.
// maxFileSizeMB <= 0 disables the per-file cap.
func Sync(ctx context.Context, database *db.DB, galleryPath, thumbnailsPath string, maxFileSizeMB int, naming Naming, progress func(processed, total int, message string)) (SyncResult, error) {
	var result SyncResult

	// The root is only probed for degraded mode when a gallery is
	// opened, and the walk below skips unreadable directories rather
	// than failing. Without this a root that went away since - an
	// unmounted drive, a dropped bind mount - reads as an empty tree
	// and phase 3 flags the whole library missing.
	if _, err := os.ReadDir(galleryPath); err != nil {
		return result, fmt.Errorf("gallery path is unreadable: %w", err)
	}

	progress(0, 0, "Phase 1: scanning filesystem...")
	known, err := loadKnownPaths(database)
	if err != nil {
		return result, err
	}
	found, err := walkGalleryFiles(ctx, galleryPath, int64(maxFileSizeMB)*1024*1024, known)
	if err != nil {
		return result, err
	}

	total := len(found)
	progress(0, total, "Phase 2: reconciling...")

	foundPaths := make(map[string]struct{}, total)
	for _, fi := range found {
		foundPaths[fi.path] = struct{}{}
	}

	bySHA, err := loadImagesBySHA(database)
	if err != nil {
		return result, err
	}

	// A library nothing has ingested yet is adopted where it stands. Every
	// file is new on that run, so filing them would reorganise a tree the
	// operator arranged before monbooru ever saw it.
	adopting := len(bySHA) == 0

	reactivated := 0
	var ingested []int64
	for i, fi := range found {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// Throttle progress emissions so Update's lock traffic stays
		// bounded on large libraries.
		if i%50 == 0 || i == total-1 {
			progress(i, total, "Phase 2: reconciling...")
		}
		if id := reconcileFile(database, galleryPath, thumbnailsPath, fi, known, bySHA, &result, &reactivated); id != 0 {
			ingested = append(ingested, id)
		}
	}

	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	if adopting && !naming.Empty() && len(ingested) > 0 {
		logx.Infof("sync: first sync of this library, %d file(s) left where they are", len(ingested))
	} else {
		nameIngested(ctx, database, galleryPath, naming, ingested, foundPaths)
	}

	toMark, err := selectImagesToMarkMissing(database, foundPaths)
	if err != nil {
		return result, err
	}
	removed, err := markImagesMissingChunked(ctx, database, toMark)
	result.Removed = removed
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	// Drop non-canonical paths whose file the walk didn't find, so a move
	// or a deleted copy can't leave a phantom duplicate. Gated on a
	// non-empty walk so a not-yet-mounted gallery can't wipe live aliases;
	// existence comes from foundPaths, not a per-row stat.
	if len(found) > 0 {
		if err := pruneStaleAliasPaths(ctx, database, foundPaths); err != nil {
			return result, err
		}
	}

	// Recompute tag usage counts only when the reconcile touched something
	// that could change them. Duplicates alone never do, so an idle sync on
	// a large library skips this step.
	if result.Added > 0 || result.Removed > 0 || result.Moved > 0 || reactivated > 0 {
		progress(0, 0, "Recalculating tag counts...")
		tags.RecalcDB(database)
	}

	// Phase 3: report. Files gone from disk are flagged is_missing rather
	// than deleted, so "missing" reflects what sync actually did.
	progress(0, 0, fmt.Sprintf("Done: %d added, %d missing, %d moved, %d duplicates",
		result.Added, result.Removed, result.Moved, result.Duplicates))

	return result, nil
}

// nameIngested applies the operator's naming to the rows this run
// created. It runs before the missing-file sweep and adds each new path
// to foundPaths, or the sweep would flag every file it just renamed as
// gone. Rows the run only touched keep their names: a sync reconciles the
// filesystem, it does not re-file the library.
func nameIngested(ctx context.Context, database *db.DB, galleryPath string, naming Naming, ids []int64, foundPaths map[string]struct{}) {
	if naming.Empty() {
		return
	}
	for _, id := range ids {
		newPath, err := naming.Apply(ctx, database, galleryPath, id, "", "")
		if err != nil {
			logx.Warnf("sync: name image %d: %v", id, err)
			continue
		}
		if newPath != "" {
			foundPaths[newPath] = struct{}{}
		}
	}
}

// loadKnownPaths preloads (path, size, sha256, mtime) for every
// image_paths row, used by the walker's unchanged-shortcut.
func loadKnownPaths(database *db.DB) (map[string]syncKnownEntry, error) {
	known := map[string]syncKnownEntry{}
	rows, err := database.Read.Query(
		`SELECT ip.path, i.file_size, i.sha256, ip.mtime_unix, ip.mtime_nsec
		   FROM image_paths ip JOIN images i ON i.id = ip.image_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("preloading known paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p, sha string
		var sz, mt, mtNano int64
		if err := rows.Scan(&p, &sz, &sha, &mt, &mtNano); err != nil {
			return nil, fmt.Errorf("scanning known paths: %w", err)
		}
		known[p] = syncKnownEntry{size: sz, sha256: sha, mtime: mt, mtimeNano: mtNano}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating known paths: %w", err)
	}
	return known, nil
}

// walkGalleryFiles walks galleryPath and returns one syncFileInfo per
// supported file. Hashes are taken from the known-shortcut when the
// (path, size, mtime) tuple matches; otherwise the file is hashed and
// ownership claimed.
func walkGalleryFiles(ctx context.Context, galleryPath string, maxBytes int64, known map[string]syncKnownEntry) ([]syncFileInfo, error) {
	var found []syncFileInfo
	err := filepath.WalkDir(galleryPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if _, typeErr := DetectFileType(path); typeErr != nil {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if maxBytes > 0 && info.Size() > maxBytes {
			return nil
		}
		mtimeUnix, mtimeNano := info.ModTime().Unix(), info.ModTime().UnixNano()
		var hash, sum string
		if k, ok := known[path]; ok && k.unchanged(info.Size(), mtimeUnix, mtimeNano) {
			// Same path, size, and mtime: assume unchanged content. The
			// mtime gate catches the same-size in-place edit case the
			// size-only check missed; rows that predate the mtime columns
			// (mtime=0) re-hash once and persist the real mtime back.
			// The row keeps whatever md5 it already has; an empty one is
			// left to the backfill rather than re-reading every file here.
			hash = k.sha256
		} else {
			h, m, hashErr := HashFileDigests(path)
			if hashErr != nil {
				logx.Warnf("hash failed for %q: %v", path, hashErr)
				return nil
			}
			hash, sum = h, m
			// Only chown when we just hashed; files reused from `known`
			// were already claimed by a previous sync.
			ClaimOwnership(path)
		}
		found = append(found, syncFileInfo{path: path, sha256: hash, md5: sum, size: info.Size(), mtime: mtimeUnix, mtimeNano: mtimeNano})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("walking gallery: %w", err)
	}
	return found, nil
}

// loadImagesBySHA preloads (sha256 → id, canonical_path, is_missing) for
// every row in images. Phase 2 then looks up each walked file with one
// map hit instead of N indexed SELECTs.
func loadImagesBySHA(database *db.DB) (map[string]syncBySHARow, error) {
	bySHA := map[string]syncBySHARow{}
	rows, err := database.Read.Query(
		`SELECT id, sha256, canonical_path, is_missing FROM images`,
	)
	if err != nil {
		return nil, fmt.Errorf("preloading SHA index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r syncBySHARow
		var sha string
		if err := rows.Scan(&r.id, &sha, &r.canonicalPath, &r.isMissing); err != nil {
			return nil, fmt.Errorf("scanning SHA index: %w", err)
		}
		bySHA[sha] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating SHA index: %w", err)
	}
	return bySHA, nil
}

// reconcileFile decides what to do with one walked file: in-place edit,
// SHA-match (same canonical, alias promotion, move, or duplicate), or
// brand-new ingest. Mutates result and maintains the known / bySHA
// maps so a later same-SHA walk entry falls into the right branch.
// Returns the id of a row this walk entry created, so the caller can name
// it once the reconcile pass is done; 0 for every other branch.
func reconcileFile(database *db.DB, galleryPath, thumbnailsPath string, fi syncFileInfo, known map[string]syncKnownEntry, bySHA map[string]syncBySHARow, result *SyncResult, reactivated *int) int64 {
	// In-place edit: same path on disk, but the freshly hashed SHA
	// differs from what image_paths last saw. The mtime gate forced
	// a re-hash; apply the new SHA / size / dimensions / metadata to
	// the existing image row so tags survive the rewrite.
	if k, knownPath := known[fi.path]; knownPath && k.sha256 != fi.sha256 {
		// images.sha256 is UNIQUE, so an edit onto bytes the library
		// already holds cannot be applied in place. Report it rather than
		// running an UPDATE that aborts the transaction on every sync;
		// which of the two rows the operator wants is not ours to guess.
		if other, held := bySHA[fi.sha256]; held {
			logx.Warnf("sync: %q was rewritten with the bytes of image %d at %q; row left as it is",
				fi.path, other.id, other.canonicalPath)
			result.Conflicts++
			return 0
		}
		if err := applyInPlaceEdit(database, thumbnailsPath, fi.path, fi.sha256, fi.md5, fi.mtime, fi.mtimeNano, fi.size); err != nil {
			logx.Warnf("sync: in-place edit %q: %v", fi.path, err)
			return 0
		}
		// Refresh bySHA: the old sha row may be gone or repointed; the
		// new sha now anchors at this path so a later same-SHA file
		// falls into duplicate.
		var imgID int64
		if err := database.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, fi.sha256).Scan(&imgID); err == nil {
			bySHA[fi.sha256] = syncBySHARow{id: imgID, canonicalPath: fi.path, isMissing: 0}
		}
		delete(bySHA, k.sha256)
		known[fi.path] = syncKnownEntry{size: fi.size, sha256: fi.sha256, mtime: fi.mtime}
		return 0
	}

	row, ok := bySHA[fi.sha256]
	if !ok {
		return reconcileNewFile(database, galleryPath, thumbnailsPath, fi, bySHA, result)
	}
	reconcileExistingSHA(database, galleryPath, fi, row, bySHA, known, result, reactivated)
	return 0
}

// reconcileNewFile handles the new-SHA branch: a fresh ingest reusing
// the Phase-1 hash so Ingest doesn't hash twice. Returns the new row's id.
func reconcileNewFile(database *db.DB, galleryPath, thumbnailsPath string, fi syncFileInfo, bySHA map[string]syncBySHARow, result *SyncResult) int64 {
	img, _, ingestErr := ingestWithHash(database, galleryPath, thumbnailsPath, fi.path, fi.sha256, fi.md5, "")
	if ingestErr != nil {
		logx.Warnf("ingest failed for %q: %v", fi.path, ingestErr)
		return 0
	}
	result.Added++
	if img == nil {
		return 0
	}
	bySHA[fi.sha256] = syncBySHARow{id: img.ID, canonicalPath: fi.path, isMissing: 0}
	return img.ID
}

// reconcileExistingSHA routes a SHA hit to one of the four sub-cases:
// same path (touch / reactivate), known alias (promote or reactivate),
// new path with vanished canonical (move), or new copy (duplicate).
// A move rewrites the bySHA entry so a later file with the same SHA sees
// the fresh canonical instead of re-entering the move branch and
// deleting the row the first move just installed.
func reconcileExistingSHA(database *db.DB, galleryPath string, fi syncFileInfo, row syncBySHARow, bySHA map[string]syncBySHARow, known map[string]syncKnownEntry, result *SyncResult, reactivated *int) {
	// Persist the freshly-observed mtime on the touched row so the next
	// sync's unchanged-shortcut can fire.
	if _, wErr := database.Write.Exec(
		`UPDATE image_paths SET mtime_unix = ?, mtime_nsec = ? WHERE path = ?`, fi.mtime, fi.mtimeNano, fi.path,
	); wErr != nil {
		logx.Warnf("sync: persist mtime for %q: %v", fi.path, wErr)
	}

	if row.canonicalPath == fi.path {
		if row.isMissing == 1 {
			reactivateImage(database, row.id)
			*reactivated++
		}
		return
	}

	// image_paths.path is UNIQUE, so a known-path entry with a matching
	// SHA is unambiguously this image's alias.
	if k, knownAlias := known[fi.path]; knownAlias && k.sha256 == fi.sha256 {
		if _, canonErr := os.Stat(row.canonicalPath); canonErr != nil {
			promoteAliasToCanonical(database, galleryPath, fi.path, row)
			bySHA[fi.sha256] = syncBySHARow{id: row.id, canonicalPath: fi.path, isMissing: 0}
			result.Moved++
		} else if row.isMissing == 1 {
			reactivateImage(database, row.id)
			*reactivated++
		}
		return
	}

	// New path for an existing SHA: a move if the canonical file is gone,
	// otherwise another copy / alias.
	if _, canonErr := os.Stat(row.canonicalPath); canonErr != nil {
		moveCanonical(database, galleryPath, fi.path, row)
		bySHA[fi.sha256] = syncBySHARow{id: row.id, canonicalPath: fi.path, isMissing: 0}
		result.Moved++
		return
	}
	if _, wErr := database.Write.Exec(
		`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical, mtime_unix, mtime_nsec) VALUES (?, ?, 0, ?, ?)`,
		row.id, fi.path, fi.mtime, fi.mtimeNano,
	); wErr != nil {
		logx.Warnf("sync: insert alias path %d: %v", row.id, wErr)
	}
	result.Duplicates++
}

// reactivateImage clears is_missing on a row that has just been
// re-observed on disk. Errors are logged - the sync still has Phase 3's
// mark-missing pass to land on, and a failed UPDATE here would be
// picked up by the next run.
func reactivateImage(database *db.DB, imageID int64) {
	if _, wErr := database.Write.Exec(`UPDATE images SET is_missing = 0 WHERE id = ?`, imageID); wErr != nil {
		logx.Warnf("sync: reactivate %d: %v", imageID, wErr)
	}
}

// promoteAliasToCanonical fires when an alias path's file is still on
// disk but the row's canonical_path is gone. The image row is repointed,
// the alias becomes canonical, and the vanished old canonical row is
// dropped so it can't resurface as a phantom duplicate.
func promoteAliasToCanonical(database *db.DB, galleryPath, newCanonical string, row syncBySHARow) {
	if err := repointCanonical(database.Write, row.id, newCanonical,
		FolderPath(galleryPath, newCanonical), row.canonicalPath); err != nil {
		logx.Warnf("sync: promote alias %d: %v", row.id, err)
	}
}

// moveCanonical points the image row at a new on-disk path when the
// previous canonical file has vanished. The vanished path is dropped
// from image_paths rather than kept as an alias, so it can't resurface
// as a phantom duplicate. The drop names the vanished path rather than
// the canonical flag, so a row carrying two canonicals (a crash mid-swap)
// loses only the path this move replaced.
func moveCanonical(database *db.DB, galleryPath, newCanonical string, row syncBySHARow) {
	if err := repointCanonical(database.Write, row.id, newCanonical,
		FolderPath(galleryPath, newCanonical), row.canonicalPath); err != nil {
		logx.Warnf("sync: move %d: %v", row.id, err)
	}
}

// selectImagesToMarkMissing returns the ids of non-missing rows whose
// canonical_path wasn't seen by Phase 1's walker.
func selectImagesToMarkMissing(database *db.DB, foundPaths map[string]struct{}) ([]int64, error) {
	rows, err := database.Read.Query(
		`SELECT id, canonical_path FROM images WHERE is_missing = 0`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying existing images: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var toMark []int64
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			// Skip rows that fail to scan rather than silently appending a
			// zero id. A zero id never matches a real row but the Removed
			// count would drift up by one per scan failure, hiding driver
			// issues.
			logx.Warnf("sync: scan existing image row: %v", err)
			continue
		}
		if _, seen := foundPaths[path]; !seen {
			toMark = append(toMark, id)
		}
	}
	if err := rows.Err(); err != nil {
		return toMark, fmt.Errorf("iterating existing images: %w", err)
	}
	return toMark, nil
}

// markImagesMissingChunked flips is_missing=1 for the supplied ids in
// 500-row chunks. Per-id UPDATEs through the single-writer pool used
// to dominate Phase 3 on libraries where many files had gone away.
// Returns the number of rows the database actually flipped: sums
// RowsAffected per chunk so a writer-contention error on a single
// chunk doesn't drift the user-visible "N missing" count out from
// under the operator. The first chunk-level error short-circuits the
// loop and surfaces alongside the partial total.
func markImagesMissingChunked(ctx context.Context, database *db.DB, ids []int64) (int, error) {
	marked := 0
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		placeholders, args := db.InPlaceholders(chunk)
		res, wErr := database.Write.Exec(
			`UPDATE images SET is_missing = 1 WHERE id IN (`+placeholders+`)`, args...,
		)
		if wErr != nil {
			logx.Warnf("sync: mark missing chunk: %v", wErr)
			return fmt.Errorf("mark missing chunk: %w", wErr)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			marked += int(n)
		}
		return nil
	})
	return marked, err
}

// pruneStaleAliasPaths deletes non-canonical image_paths rows whose file
// is gone from disk, in 500-row chunks. foundPaths is the fast path: a
// path the walk observed is kept without a stat. A path it didn't observe
// is stat'd and removed only when genuinely absent, so a file merely
// skipped this pass (over the size cap, undetectable, transiently
// unreadable) keeps its row. Canonical rows are left to the is_missing pass.
func pruneStaleAliasPaths(ctx context.Context, database *db.DB, foundPaths map[string]struct{}) error {
	type aliasPath struct {
		id   int64
		path string
	}
	// Read the list before stat'ing any of it: the walk is one syscall per
	// unobserved path and must not run with the cursor held open.
	aliases, err := db.QueryAll(database.Read, func(rows *sql.Rows) (aliasPath, error) {
		var a aliasPath
		err := rows.Scan(&a.id, &a.path)
		return a, err
	}, `SELECT id, path FROM image_paths WHERE is_canonical = 0`)
	if err != nil {
		return fmt.Errorf("listing alias paths: %w", err)
	}
	var staleIDs []int64
	for _, a := range aliases {
		if _, ok := foundPaths[a.path]; ok {
			continue
		}
		if _, statErr := os.Stat(a.path); os.IsNotExist(statErr) {
			staleIDs = append(staleIDs, a.id)
		}
	}

	return db.Chunked(staleIDs, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		placeholders, args := db.InPlaceholders(chunk)
		if _, wErr := database.Write.Exec(
			`DELETE FROM image_paths WHERE id IN (`+placeholders+`)`, args...,
		); wErr != nil {
			return fmt.Errorf("prune alias paths chunk: %w", wErr)
		}
		return nil
	})
}

// applyInPlaceEdit handles the case where sync re-hashed a known path
// and observed a different SHA. The image_id stays so user-curated tags
// survive; sha, size, dimensions, source_type, and side-table metadata
// (sd_metadata, comfyui_metadata) are refreshed to match the new bytes
// on disk, and the thumbnail is regenerated. The mtime gate at the top
// of the walk is what triggers entry; the corresponding image_paths
// row's mtime is updated here so the next sync's shortcut can fire.
func applyInPlaceEdit(database *db.DB, thumbnailsPath, path, newSHA, newMD5 string, newMtime, newMtimeNano, newSize int64) error {
	var imageID int64
	if err := database.Read.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ?`, path,
	).Scan(&imageID); err != nil {
		return fmt.Errorf("locate image for path %q: %w", path, err)
	}

	// The rewrite can change the type under a name that never moved, and
	// bytes that are no longer media leave the row describing what the file
	// used to be rather than a row with no dimensions and no thumbnail.
	fileType, err := detectMagicType(path)
	if err != nil {
		return fmt.Errorf("contents of %q are not a supported media type: %w", path, err)
	}

	var imgWidth, imgHeight *int
	var pageCount *int
	if fileType == "cbz" {
		archive, openErr := OpenManga(path)
		if openErr == nil {
			if w, h, dimErr := archive.CoverDimensions(); dimErr == nil {
				imgWidth, imgHeight = &w, &h
			}
			pcVal := len(archive.Pages)
			pageCount = &pcVal
			_ = archive.Close()
		}
	} else if IsVideoType(fileType) {
		if w, h, ok := ProbeVideoDimensions(path); ok {
			imgWidth, imgHeight = &w, &h
		}
	} else {
		imgWidth, imgHeight = decodeImageDimensions(path)
	}
	sdMeta, comfyMeta, sourceType := extractGenerationMeta(path, fileType)

	tx, err := database.Write.Begin()
	if err != nil {
		return fmt.Errorf("begin in-place edit tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE images SET sha256 = ?, md5 = ?, file_size = ?, file_type = ?, width = ?, height = ?, page_count = ?, source_type = ? WHERE id = ?`,
		newSHA, newMD5, newSize, fileType, toNullInt(imgWidth), toNullInt(imgHeight), toNullInt(pageCount), sourceType, imageID,
	); err != nil {
		return fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET mtime_unix = ?, mtime_nsec = ? WHERE path = ?`, newMtime, newMtimeNano, path,
	); err != nil {
		return fmt.Errorf("update image_paths mtime: %w", err)
	}
	// The recorded lookup misses are about bytes this row no longer has.
	if err := lookup.DeleteForImage(tx, imageID); err != nil {
		return fmt.Errorf("clear lookup history: %w", err)
	}
	if err := ReplaceGenerationMetadata(context.Background(), tx, imageID, sdMeta, comfyMeta); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit in-place edit: %w", err)
	}

	// A cbz whose bytes changed can have a different page count or order,
	// so the lazily-extracted raw page cache for the old contents is stale
	// (the reader serves an existing page file without revalidating it).
	// Drop the whole per-image cache; Generate below re-renders the page
	// thumbnails and the raw pages re-extract on the next read.
	if fileType == "cbz" {
		RemoveMangaCache(thumbnailsPath, imageID)
	}

	RegenerateDerived(database, thumbnailsPath, path, imageID, fileType, "in-place edit")
	logx.Infof("in-place edit: image id=%d at %q now carries sha %s", imageID, path, newSHA)
	return nil
}

// FolderTree builds the folder tree from images. Each node's Count rolls
// up its own images plus every descendant's, so a parent with only
// subfolder content still shows a non-zero figure. Empty intermediate
// folders are included so the arborescence is complete.
func FolderTree(database *db.DB) ([]FolderNode, error) {
	flat, err := db.QueryAll(database.Read, scanFolderRow,
		`SELECT COALESCE(folder_path, ''), COUNT(*) FROM images WHERE is_missing=0 GROUP BY folder_path ORDER BY folder_path`)
	if err != nil {
		return nil, err
	}
	return buildFolderTree(flat), nil
}

// buildFolderTree turns a flat (path, count) list from the database
// into the rolled-up tree the sidebar renders.
func buildFolderTree(flat []folderCount) []FolderNode {
	// Add intermediate ancestor paths so the full arborescence shows.
	known := map[string]bool{"": true}
	for _, fc := range flat {
		known[fc.path] = true
	}
	var toAdd []folderCount
	for _, fc := range flat {
		if fc.path == "" {
			continue
		}
		segments := strings.Split(fc.path, "/")
		for i := 1; i < len(segments); i++ {
			ancestor := strings.Join(segments[:i], "/")
			if !known[ancestor] {
				known[ancestor] = true
				toAdd = append(toAdd, folderCount{path: ancestor, count: 0})
			}
		}
	}
	flat = append(flat, toAdd...)

	// Pointer-tree intermediate so parent-child wiring survives mutations.
	type pnode struct {
		FolderNode
		children []*pnode
	}

	rootP := &pnode{FolderNode: FolderNode{Path: "", Name: "(root)", Depth: 0}}
	pnodeMap := map[string]*pnode{"": rootP}

	// Sort lexicographically so parents always exist before children.
	slices.SortFunc(flat, func(a, b folderCount) int {
		return cmp.Compare(a.path, b.path)
	})

	for _, fc := range flat {
		if fc.path == "" {
			rootP.Count = fc.count
			continue
		}
		// folder_path is "/"-separated regardless of platform; filepath.Dir
		// would normalize the separators to native on Windows and miss the
		// parent in pnodeMap, flattening the tree.
		name := fc.path
		parentPath := ""
		if i := strings.LastIndex(fc.path, "/"); i >= 0 {
			name = fc.path[i+1:]
			parentPath = fc.path[:i]
		}
		n := &pnode{FolderNode: FolderNode{
			Path:  fc.path,
			Name:  name,
			Count: fc.count,
			Depth: strings.Count(fc.path, "/") + 1,
		}}
		pnodeMap[fc.path] = n

		parent, ok := pnodeMap[parentPath]
		if !ok {
			parent = rootP
		}
		parent.children = append(parent.children, n)
	}

	// Post-order: roll descendant counts into each ancestor.
	var rollup func(p *pnode)
	rollup = func(p *pnode) {
		for _, c := range p.children {
			rollup(c)
			p.Count += c.Count
		}
	}
	rollup(rootP)

	// Pointer tree to value tree (deep copy).
	var toValue func(p *pnode) FolderNode
	toValue = func(p *pnode) FolderNode {
		n := p.FolderNode
		for _, c := range p.children {
			n.Children = append(n.Children, toValue(c))
		}
		return n
	}

	return []FolderNode{toValue(rootP)}
}
