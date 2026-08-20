package relations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

// IncrementalProbeDistance is the Hamming distance the on-ingest
// probe (fired from gallery.PhashHooks.OnStored) walks the BK-tree
// for. Atomic so the config-edit path can flip it without a restart.
var IncrementalProbeDistance atomic.Int32

// IncrementalProbeEnabled toggles the on-ingest probe. Default true;
// operators who'd rather batch can disable it through the same
// config block.
var IncrementalProbeEnabled atomic.Bool

func init() {
	IncrementalProbeDistance.Store(4)
	IncrementalProbeEnabled.Store(true)
}

// FindPairsOptions controls one find-pairs invocation. Distance clamps
// to 0..12 by the caller; Replace=true wipes the queue before re-scanning,
// otherwise existing rows survive and only new candidates are added.
type FindPairsOptions struct {
	Distance       int
	Replace        bool
	ThumbnailsPath string
	// TagPairs runs the tag-similarity pass after the phash walk.
	// TagPairThreshold is the admission score it applies.
	TagPairs         bool
	TagPairThreshold float64
}

// FindPairsProgress is the per-row callback the caller's job manager
// uses to drive the status-bar progress percentage. Phase strings:
// "phashing" while the lazy phash backfill runs, "probing" while the
// BK-tree scan runs.
type FindPairsProgress func(processed, total int, phase string)

// FindPairs walks every visible image, computes any missing phash
// inline (the lazy compute documented in §5.2), probes the per-gallery
// BK-tree for candidates within opts.Distance, and inserts canonicalised
// (a, b, distance) rows into potential_relation_pairs.
//
// Skips already-related pairs and pairs in not_related_pairs / the
// existing queue (unless opts.Replace wipes the queue first). 500-row
// transactions on insert match the implication-fanout cadence.
func FindPairs(ctx context.Context, database *db.DB, tree *BKTree, opts FindPairsOptions, progress FindPairsProgress) (added int, err error) {
	if tree == nil {
		return 0, errors.New("relations: nil bk-tree")
	}
	if opts.Replace {
		if _, err := database.Write.ExecContext(ctx, `DELETE FROM potential_relation_pairs`); err != nil {
			return 0, fmt.Errorf("wipe queue: %w", err)
		}
		tree.Reset()
	}

	// Pull the full id + phash list. NULL phashes get computed inline
	// during the walk; the tree picks them up via the OnStored hook.
	// Archives ride the tree (search still probes them) but stay out of
	// the pair queue: their phash is only the cover page.
	type row struct {
		id       int64
		phash    sql.NullInt64
		fileType string
	}
	entries, err := db.QueryAll(database.Read, func(rows *sql.Rows) (row, error) {
		var r row
		err := rows.Scan(&r.id, &r.phash, &r.fileType)
		return r, err
	}, `SELECT id, phash, file_type FROM images WHERE is_missing = 0 ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("load image ids: %w", err)
	}
	archives := make(map[int64]bool)
	for _, r := range entries {
		if r.fileType == models.FileTypeCBZ {
			archives[r.id] = true
		}
	}

	if err := tree.EnsureBuilt(database); err != nil {
		return 0, fmt.Errorf("bk-tree build: %w", err)
	}

	total := len(entries)
	now := time.Now().UTC().Format(time.RFC3339)
	const txChunk = 500
	var pending []pairToInsert

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := database.Write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, p := range pending {
			if _, err := stmt.Exec(p.a, p.b, p.distance, now); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				return err
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	// Pass 1: compute every missing phash up front so the tree holds all
	// rows before any probe. Probing inline as phashes were computed
	// missed pairs whose higher-id member hadn't been inserted yet - the
	// lower-id member was probed first with the higher id still absent,
	// and when the higher-id member was later probed the lower id was
	// dropped by the a < b canonicalisation in pass 2.
	for idx := range entries {
		if ctx.Err() != nil {
			return added, ctx.Err()
		}
		if entries[idx].phash.Valid {
			continue
		}
		if progress != nil {
			progress(idx, total, "phashing")
		}
		if err := gallery.RecomputeAndStorePhash(ctx, database, entries[idx].id, opts.ThumbnailsPath); err != nil {
			logx.Debugf("find-pairs phash %d: %v", entries[idx].id, err)
			continue
		}
		var phash sql.NullInt64
		if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, entries[idx].id).Scan(&phash); err != nil {
			logx.Debugf("find-pairs reread %d: %v", entries[idx].id, err)
			continue
		}
		if !phash.Valid {
			continue
		}
		entries[idx].phash = phash
		// The OnStored hook already inserts into a built tree; Insert is
		// idempotent on the id, so this also covers a tree not wired to
		// the hook (no registry entry, e.g. in tests).
		tree.Insert(entries[idx].id, phash.Int64)
	}

	// Pass 2: probe every row against the now fully-populated tree.
	for idx, e := range entries {
		if ctx.Err() != nil {
			if flushErr := flush(); flushErr != nil {
				logx.Debugf("find-pairs flush during cancel: %v", flushErr)
			}
			return added, ctx.Err()
		}
		if !e.phash.Valid {
			continue // phash compute failed in pass 1
		}
		if archives[e.id] {
			continue // archives are paired by tag similarity, not phash
		}
		if progress != nil && idx%64 == 0 {
			progress(idx, total, "probing")
		}
		candidates, _ := tree.SearchWithinDistance(e.phash.Int64, opts.Distance)
		for _, cid := range candidates {
			if cid <= e.id {
				continue // canonicalise a < b; the symmetric pair surfaces when we reach a
			}
			if archives[cid] {
				continue
			}
			// Skip if pair already carries a real relation or is on
			// the not-related list. Cheap correlated COUNTs that ride
			// covering indexes; well under a millisecond apiece.
			already, err := pairAlreadyKnown(ctx, database, e.id, cid)
			if err != nil {
				return added, err
			}
			if already {
				continue
			}
			pending = append(pending, pairToInsert{
				a: e.id, b: cid, distance: hammingDistance(e.phash.Int64, lookupPhashFromTree(tree, cid)),
			})
			added++
			if len(pending) >= txChunk {
				if err := flush(); err != nil {
					return added, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return added, err
	}
	if progress != nil {
		progress(total, total, "probing")
	}
	if opts.TagPairs {
		tagAdded, err := findTagPairs(ctx, database, opts.TagPairThreshold, progress)
		added += tagAdded
		if err != nil {
			return added, err
		}
	}
	return added, nil
}

type pairToInsert struct {
	a, b     int64
	distance int
}

// pairAlreadyKnown reports whether (a, b) is already represented by any
// relation table or by not_related_pairs / the existing queue. The
// queue check is intentional: re-running find-pairs without replace=true
// shouldn't insert duplicate canonical rows.
func pairAlreadyKnown(ctx context.Context, database *db.DB, a, b int64) (bool, error) {
	tx, err := database.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// pairSettledTx only does SELECTs, so the read pool is fine;
	// `not_related` blocks find-pairs from resurfacing rejections.
	if got, err := pairSettledTx(tx, a, b); err != nil {
		return false, err
	} else if got {
		return true, nil
	}
	lo, hi := canonicalPair(a, b)
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// imageIsArchive reports whether the image is a cbz/zip archive. The
// batch walk reads file_type in bulk instead; this single-row lookup
// serves the on-ingest probe, which sees one id at a time.
func imageIsArchive(database *db.DB, id int64) (bool, error) {
	var fileType string
	if err := database.Read.QueryRow(`SELECT file_type FROM images WHERE id = ?`, id).Scan(&fileType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return fileType == models.FileTypeCBZ, nil
}

// lookupPhashFromTree retrieves an image's phash through the tree's
// id->phash index. Used to compute the stored Hamming distance for
// queue rows without re-querying SQLite per candidate.
func lookupPhashFromTree(tree *BKTree, id int64) int64 {
	tree.mu.RLock()
	defer tree.mu.RUnlock()
	return tree.idIndex[id]
}

// incrementalProbe runs one BK-tree probe for the just-stored (id,
// phash) row and inserts canonicalised pairs into the queue. Skips
// already-related / not-related / queued pairs. Best-effort: an
// error short-circuits the rest of the candidates rather than
// failing the surrounding ingest.
func incrementalProbe(database *db.DB, tree *BKTree, id, phash int64, distance int) error {
	// Archives are paired by tag similarity, not phash: the stored hash is
	// only the cover page, so a cover match says nothing about the book.
	if archive, err := imageIsArchive(database, id); err != nil || archive {
		return err
	}
	candidates, _ := tree.SearchWithinDistance(phash, distance)
	if len(candidates) == 0 {
		return nil
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, cid := range candidates {
		if cid == id {
			continue
		}
		if archive, err := imageIsArchive(database, cid); err != nil {
			return err
		} else if archive {
			continue
		}
		lo, hi := canonicalPair(id, cid)
		known, err := pairAlreadyKnown(ctx, database, lo, hi)
		if err != nil {
			return err
		}
		if known {
			continue
		}
		other := lookupPhashFromTree(tree, cid)
		if _, err := database.Write.Exec(
			`INSERT OR IGNORE INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, ?)`,
			lo, hi, hammingDistance(phash, other), now,
		); err != nil {
			return err
		}
	}
	return nil
}
