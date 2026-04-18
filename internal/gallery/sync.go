// Monbooru is a Linux-only deployment; path handling assumes forward slashes.
package gallery

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tags"
)

// SyncResult summarizes the outcome of a gallery sync.
type SyncResult struct {
	Added      int
	Removed    int
	Moved      int
	Duplicates int
}

// FolderNode represents a folder in the gallery tree.
type FolderNode struct {
	Path     string
	Name     string
	Count    int
	Depth    int
	Children []FolderNode
}

// Sync performs a full 3-phase gallery sync.
// progress is called with (processed, total, message) tuples, matching
// jobs.Manager.Update so the handler can forward the call verbatim.
// Processed advances during Phase 2 so clients polling /internal/job/status
// can surface reconcile progress and refresh the gallery mid-run.
// galleryPath and thumbnailsPath target one gallery; maxFileSizeMB is the
// shared per-install cap (<= 0 disables the cap).
func Sync(ctx context.Context, database *db.DB, galleryPath, thumbnailsPath string, maxFileSizeMB int, progress func(processed, total int, message string)) (SyncResult, error) {
	var result SyncResult

	maxBytes := int64(maxFileSizeMB) * 1024 * 1024

	// Phase 1: Walk filesystem, build map of path → sha256
	progress(0, 0, "Phase 1: scanning filesystem…")
	type fileInfo struct {
		path     string
		sha256   string
		fileType string
		size     int64
	}
	var found []fileInfo

	// Preload known (path, size, sha256) so the walk can skip hashing files
	// whose size hasn't changed since the last sync. Hashing every file on a
	// 10k+ image library dominates sync time even when nothing has changed.
	type knownEntry struct {
		size   int64
		sha256 string
	}
	known := map[string]knownEntry{}
	if krows, kerr := database.Read.Query(
		`SELECT ip.path, i.file_size, i.sha256 FROM image_paths ip JOIN images i ON i.id = ip.image_id`,
	); kerr == nil {
		for krows.Next() {
			var p, sha string
			var sz int64
			if err := krows.Scan(&p, &sz, &sha); err == nil {
				known[p] = knownEntry{size: sz, sha256: sha}
			}
		}
		krows.Close()
	}

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

		ft, typeErr := DetectFileType(path)
		if typeErr != nil {
			return nil // unsupported type, skip
		}

		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if maxBytes > 0 && info.Size() > maxBytes {
			return nil // too large
		}

		var hash string
		if k, ok := known[path]; ok && k.size == info.Size() {
			// Same path and size - assume unchanged content. A same-size
			// in-place replacement will be missed here; run manually with a
			// DB reset to force re-hashing if you suspect one.
			hash = k.sha256
		} else {
			h, hashErr := HashFile(path)
			if hashErr != nil {
				logx.Warnf("hash failed for %q: %v", path, hashErr)
				return nil
			}
			hash = h
			// Only claim ownership when we just hashed (new file or its size
			// changed). Files reused from `known` were already chowned by a
			// previous sync, so skipping the syscall here saves N chowns per
			// idle re-sync - the dominant Phase-1 cost on large libraries.
			ClaimOwnership(path)
		}

		found = append(found, fileInfo{path: path, sha256: hash, fileType: ft, size: info.Size()})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("walking gallery: %w", err)
	}

	// Phase 2: Reconcile
	total := len(found)
	progress(0, total, "Phase 2: reconciling…")

	// Build set of all found paths and SHAs
	foundPaths := map[string]struct{}{}
	for _, fi := range found {
		foundPaths[fi.path] = struct{}{}
	}

	// Pre-load every (sha256 → id, canonical_path, is_missing) row so the
	// per-file lookup below is a map hit instead of N indexed SELECTs through
	// the read pool. One full scan beats 25k single-row queries.
	type bySHARow struct {
		id            int64
		canonicalPath string
		isMissing     int
	}
	bySHA := map[string]bySHARow{}
	if srows, sErr := database.Read.Query(
		`SELECT id, sha256, canonical_path, is_missing FROM images`,
	); sErr == nil {
		for srows.Next() {
			var r bySHARow
			var sha string
			if err := srows.Scan(&r.id, &sha, &r.canonicalPath, &r.isMissing); err == nil {
				bySHA[sha] = r
			}
		}
		srows.Close()
	}

	// reactivated counts silent is_missing=0 updates that don't bump any
	// SyncResult counter. Needed to decide whether tag counts must be
	// recalculated at the end of the sync.
	reactivated := 0

	for i, fi := range found {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		// Throttle progress emissions so Update's lock traffic stays bounded
		// on large libraries while still letting the client poll see a live
		// X/Y counter advance once per tick. The message keeps no count -
		// the status bar's progress span already renders Processed/Total.
		if i%50 == 0 || i == total-1 {
			progress(i, total, "Phase 2: reconciling…")
		}

		row, ok := bySHA[fi.sha256]
		if ok {
			imgID := row.id
			canonPath := row.canonicalPath
			isMissing := row.isMissing

			if canonPath == fi.path {
				if isMissing == 1 {
					if _, wErr := database.Write.Exec(`UPDATE images SET is_missing = 0 WHERE id = ?`, imgID); wErr != nil {
						logx.Warnf("sync: reactivate %d: %v", imgID, wErr)
					}
					reactivated++
				}
				continue
			}

			// image_paths.path is UNIQUE, so a path belongs to at most one
			// image; `known` was preloaded from image_paths JOIN images at
			// the top of Phase 1, so a matching sha here is equivalent to
			// the per-file SELECT COUNT this replaces.
			if k, knownAlias := known[fi.path]; knownAlias && k.sha256 == fi.sha256 {
				// Known alias path - check if canonical is still present
				_, canonErr := os.Stat(canonPath)
				if canonErr != nil {
					// Canonical is gone - promote this alias to canonical
					newFolder := FolderPath(galleryPath, fi.path)
					if _, wErr := database.Write.Exec(
						`UPDATE images SET canonical_path = ?, folder_path = ?, is_missing = 0 WHERE id = ?`,
						fi.path, newFolder, imgID,
					); wErr != nil {
						logx.Warnf("sync: promote alias %d: %v", imgID, wErr)
					}
					if _, wErr := database.Write.Exec(
						`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`,
						imgID, fi.path,
					); wErr != nil {
						logx.Warnf("sync: set canonical path %d: %v", imgID, wErr)
					}
					if _, wErr := database.Write.Exec(
						`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ? AND path = ?`,
						imgID, canonPath,
					); wErr != nil {
						logx.Warnf("sync: clear old canonical %d: %v", imgID, wErr)
					}
					result.Moved++
				} else if isMissing == 1 {
					if _, wErr := database.Write.Exec(`UPDATE images SET is_missing = 0 WHERE id = ?`, imgID); wErr != nil {
						logx.Warnf("sync: reactivate %d: %v", imgID, wErr)
					}
					reactivated++
				}
				continue
			}

			// New path for an existing SHA: if the canonical file is gone it's a move,
			// otherwise it's another copy/alias.
			_, canonErr := os.Stat(canonPath)
			if canonErr != nil {
				// Canonical missing - this is a move
				newFolder := FolderPath(galleryPath, fi.path)
				if _, wErr := database.Write.Exec(
					`UPDATE images SET canonical_path = ?, folder_path = ?, is_missing = 0 WHERE id = ?`,
					fi.path, newFolder, imgID,
				); wErr != nil {
					logx.Warnf("sync: move %d: %v", imgID, wErr)
				}
				if _, wErr := database.Write.Exec(
					`UPDATE image_paths SET path = ?, is_canonical = 1 WHERE image_id = ? AND is_canonical = 1`,
					fi.path, imgID,
				); wErr != nil {
					logx.Warnf("sync: update canonical path %d: %v", imgID, wErr)
				}
				if _, wErr := database.Write.Exec(
					`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)`,
					imgID, fi.path,
				); wErr != nil {
					logx.Warnf("sync: insert canonical path %d: %v", imgID, wErr)
				}
				result.Moved++
			} else {
				// Canonical still exists - this is a duplicate/alias
				if _, wErr := database.Write.Exec(
					`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
					imgID, fi.path,
				); wErr != nil {
					logx.Warnf("sync: insert alias path %d: %v", imgID, wErr)
				}
				result.Duplicates++
			}
		} else {
			// Not found - new file. Reuse the SHA computed in Phase 1 so the
			// file isn't hashed a second time inside Ingest; on a fresh dump of
			// 25k images that double-hash was the dominant cost of Phase 2.
			img, _, ingestErr := ingestWithHash(database, galleryPath, thumbnailsPath, fi.path, fi.fileType, fi.sha256)
			if ingestErr != nil {
				logx.Warnf("ingest failed for %q: %v", fi.path, ingestErr)
				continue
			}
			result.Added++
			// Surface the new row in bySHA so a later file in the same walk
			// that shares this SHA falls into the existing-SHA branch and is
			// counted as a Duplicate, matching the original per-file SELECT.
			if img != nil {
				bySHA[fi.sha256] = bySHARow{id: img.ID, canonicalPath: fi.path, isMissing: 0}
			}
		}
	}

	// Honour cancellation between phases so the watcher's "sync running"
	// pause (see internal/gallery/watcher.go) releases promptly on cancel
	// instead of waiting for Phase 3 to finish on large libraries.
	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	// Mark missing: DB entries whose canonical path was not found
	rows, err := database.Read.Query(
		`SELECT id, canonical_path FROM images WHERE is_missing = 0`,
	)
	if err != nil {
		return result, fmt.Errorf("querying existing images: %w", err)
	}
	var toMark []int64
	for rows.Next() {
		var id int64
		var path string
		rows.Scan(&id, &path)
		if _, seen := foundPaths[path]; !seen {
			toMark = append(toMark, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("iterating existing images: %w", err)
	}

	// Chunked IN-list UPDATE: per-id UPDATEs through the single-writer pool
	// dominated Phase 3 on large libraries where many files had gone away.
	const chunkSize = 500
	for start := 0; start < len(toMark); start += chunkSize {
		if ctx.Err() != nil {
			result.Removed = start
			return result, ctx.Err()
		}
		end := start + chunkSize
		if end > len(toMark) {
			end = len(toMark)
		}
		chunk := toMark[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if _, wErr := database.Write.Exec(
			`UPDATE images SET is_missing = 1 WHERE id IN (`+placeholders+`)`, args...,
		); wErr != nil {
			logx.Warnf("sync: mark missing chunk: %v", wErr)
		}
	}
	result.Removed = len(toMark)

	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	// Recalculate tag usage counts only when the reconcile touched something
	// that could have changed them (Duplicates alone never do - adding an
	// alias doesn't change image membership). Idle syncs on large libraries
	// spent the bulk of their time in this step even though nothing had
	// changed.
	if result.Added > 0 || result.Removed > 0 || result.Moved > 0 || reactivated > 0 {
		progress(0, 0, "Recalculating tag counts…")
		tags.RecalcAndPruneDB(database)
	}

	// Phase 3: Report
	progress(0, 0, fmt.Sprintf("Done: %d added, %d removed, %d moved, %d duplicates",
		result.Added, result.Removed, result.Moved, result.Duplicates))

	return result, nil
}

// DeleteEmptyFolderIfEmpty deletes the folder at folderPath (relative to gallery root)
// if it is empty, then walks up the parent chain removing any ancestors that become
// empty as a result. Stops at the gallery root.
func DeleteEmptyFolderIfEmpty(galleryPath, folderPath string) {
	if folderPath == "" {
		return // never delete gallery root
	}
	root := galleryPath
	cur := folderPath
	for cur != "" && cur != "." {
		absPath := filepath.Join(root, cur)
		entries, err := os.ReadDir(absPath)
		if err != nil {
			return
		}
		if len(entries) != 0 {
			return // not empty, stop climbing
		}
		if removeErr := os.Remove(absPath); removeErr != nil {
			logx.Warnf("removing empty folder %q: %v", absPath, removeErr)
			return
		}
		cur = filepath.Dir(cur)
		if cur == "." {
			cur = ""
		}
	}
}

// FolderTree returns the folder tree from DB image records.
// Parent folders with no direct images are included so the full arborescence is visible.
func FolderTree(database *db.DB) ([]FolderNode, error) {
	rows, err := database.Read.Query(
		`SELECT COALESCE(folder_path, ''), COUNT(*) FROM images WHERE is_missing=0 GROUP BY folder_path ORDER BY folder_path`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type folderCount struct {
		path  string
		count int
	}
	var flat []folderCount
	totalCount := 0

	for rows.Next() {
		var fc folderCount
		if err := rows.Scan(&fc.path, &fc.count); err != nil {
			return nil, fmt.Errorf("scanning folder row: %w", err)
		}
		totalCount += fc.count
		flat = append(flat, fc)
	}

	// Add intermediate ancestor paths that have no direct images so the
	// full folder arborescence is visible in the sidebar.
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

	// Build folder tree using pointer map so parent-child wiring works across mutations.
	type pnode struct {
		FolderNode
		children []*pnode
	}

	rootP := &pnode{FolderNode: FolderNode{Path: "", Name: "(root)", Count: totalCount, Depth: 0}}
	pnodeMap := map[string]*pnode{"": rootP}

	// Process paths in lexicographic order so parents are always created before children.
	slices.SortFunc(flat, func(a, b folderCount) int {
		return cmp.Compare(a.path, b.path)
	})

	for _, fc := range flat {
		if fc.path == "" {
			continue
		}
		n := &pnode{FolderNode: FolderNode{
			Path:  fc.path,
			Name:  filepath.Base(fc.path),
			Count: fc.count,
			Depth: countSlashes(fc.path) + 1,
		}}
		pnodeMap[fc.path] = n

		parentPath := filepath.Dir(fc.path)
		if parentPath == "." {
			parentPath = ""
		}
		parent, ok := pnodeMap[parentPath]
		if !ok {
			parent = rootP
		}
		parent.children = append(parent.children, n)
	}

	// Convert pointer tree → value tree (deep copy).
	var toValue func(p *pnode) FolderNode
	toValue = func(p *pnode) FolderNode {
		n := p.FolderNode
		for _, c := range p.children {
			n.Children = append(n.Children, toValue(c))
		}
		return n
	}

	return []FolderNode{toValue(rootP)}, nil
}

func countSlashes(s string) int {
	n := 0
	for _, c := range s {
		if c == '/' {
			n++
		}
	}
	return n
}
