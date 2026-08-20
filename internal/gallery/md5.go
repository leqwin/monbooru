package gallery

import (
	"context"
	"crypto/md5"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

// MD5BackfillProgress reports the walk's position to the caller-side job
// manager. Keeps the helper testable without internal/jobs.
type MD5BackfillProgress func(processed, total int, message string)

// ComputeAndStoreMD5 hashes the image's canonical file and writes the
// digest back, returning what it stored. The lazy detail-page fill and
// the backfill job both land here, and so does any lookup that finds the
// column empty: the digest a booru is queried with has to come from the
// local bytes, never from what a source claims.
func ComputeAndStoreMD5(ctx context.Context, database *db.DB, imageID int64) (string, error) {
	var canonPath string
	if err := database.Read.QueryRowContext(ctx,
		`SELECT canonical_path FROM images WHERE id = ?`, imageID,
	).Scan(&canonPath); err != nil {
		return "", err
	}
	sum, err := hashFileWith(ctx, canonPath, md5.New())
	if err != nil {
		return "", err
	}
	if _, err := database.Write.ExecContext(ctx,
		`UPDATE images SET md5 = ? WHERE id = ?`, sum, imageID,
	); err != nil {
		return "", err
	}
	return sum, nil
}

// BackfillMD5s computes images.md5 for every non-missing row that still
// has none. Reads the candidate ids up front so the read cursor isn't
// held open for what is the longest job monbooru runs: unlike the phash
// walk, which reads small cached thumbnails, this one reads every
// original byte in the gallery.
//
// Honours ctx cancellation between rows. Re-running once every row is
// hashed is an empty walk.
func BackfillMD5s(ctx context.Context, database *db.DB, progress MD5BackfillProgress) (processed, updated int, err error) {
	ids, err := db.QueryIDs(database.Read,
		`SELECT id FROM images WHERE md5 = '' AND is_missing = 0 ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}

	total := len(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return processed, updated, ctx.Err()
		}
		if progress != nil {
			progress(processed, total, "")
		}
		if _, err := ComputeAndStoreMD5(ctx, database, id); err != nil {
			// A file that vanished or turned unreadable between the id
			// scan and the read leaves the row empty for the next run.
			logx.Debugf("md5 backfill image %d: %v", id, err)
			processed++
			continue
		}
		processed++
		updated++
	}
	if progress != nil {
		progress(processed, total, "MD5…")
	}
	return processed, updated, nil
}
