package lookup

import (
	"database/sql"
	"time"

	"github.com/monbooru/monbooru/internal/db"
)

// Row is one backend's recorded history for an image, as the detail page and
// the API render it.
type Row struct {
	Backend    string
	Attempts   int
	QueuedAt   time.Time
	LastAt     time.Time
	LastResult string
	NextDueAt  time.Time
}

// InFlight reports an attempt still waiting on monloader, for the reconcile
// sweep.
type InFlight struct {
	ImageID int64
	Backend string
	JobID   int64
}

// Enqueued stamps an attempt as in flight. jobID is the id monloader returned
// in its 202; it is what makes an attempt whose callback goes missing
// resolvable at all, so a zero id (an older monloader, or a reply we could
// not read) still records the attempt but leaves it to the grace sweep.
func Enqueued(database *db.DB, imageID int64, backends []string, jobID int64, now time.Time) error {
	for _, backend := range backends {
		if _, err := database.Write.Exec(
			`INSERT INTO image_lookups (image_id, backend, queued_at, job_id)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(image_id, backend) DO UPDATE SET queued_at = excluded.queued_at, job_id = excluded.job_id`,
			imageID, backend, stamp(now), nullableID(jobID),
		); err != nil {
			return err
		}
	}
	return nil
}

// Record concludes one attempt. Only a hit or a miss is evidence about the
// image: they stamp last_at / last_result and move the ladder. An error, and
// the inconclusive result the reconcile sweep produces, are evidence about
// the plumbing - they clear the in-flight state and leave the image due now,
// so a monloader that is down for six ladder rungs never walks an image to
// "nothing found" without a single lookup having run.
//
// cursor is monloader's index position and is stored on a PTR miss so the
// retry can skip an index that has not moved; pass 0 elsewhere.
func Record(database *db.DB, imageID int64, backend, result string, cursor uint64, now time.Time) error {
	if result != ResultHit && result != ResultMiss {
		_, err := database.Write.Exec(
			`INSERT INTO image_lookups (image_id, backend, next_due_at) VALUES (?, ?, ?)
			 ON CONFLICT(image_id, backend) DO UPDATE SET
			   queued_at = NULL, job_id = NULL, next_due_at = excluded.next_due_at`,
			imageID, backend, stamp(now))
		return err
	}
	attempts, err := nextAttempts(database, imageID, backend, result)
	if err != nil {
		return err
	}
	var due any
	if result == ResultMiss {
		if t, ok := NextDue(now, backend, attempts); ok {
			due = stamp(t)
		}
	}
	var ptrCursor any
	if backend == BackendPTR && cursor > 0 {
		ptrCursor = int64(cursor)
	}
	_, err = database.Write.Exec(
		`INSERT INTO image_lookups (image_id, backend, attempts, last_at, last_result, next_due_at, ptr_cursor)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(image_id, backend) DO UPDATE SET
		   queued_at = NULL, job_id = NULL, attempts = excluded.attempts,
		   last_at = excluded.last_at, last_result = excluded.last_result,
		   next_due_at = excluded.next_due_at, ptr_cursor = excluded.ptr_cursor`,
		imageID, backend, attempts, stamp(now), result, due, ptrCursor)
	return err
}

// nextAttempts is the miss counter after this outcome: a hit resets it, a
// miss advances the stored count.
func nextAttempts(database *db.DB, imageID int64, backend, result string) (int, error) {
	if result == ResultHit {
		return 0, nil
	}
	var attempts int
	err := database.Read.QueryRow(
		`SELECT attempts FROM image_lookups WHERE image_id = ? AND backend = ?`, imageID, backend).Scan(&attempts)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return attempts + 1, nil
}

// RecordInFlight concludes whatever attempts the image currently has in
// flight. The callbacks carry an image, not a job, so this is how an enrich
// or a fetch-status report reaches the right rows; backend narrows it when
// the callback implies one. A row nobody is waiting on is left alone, so a
// plain source refetch cannot conclude a lookup that never ran.
func RecordInFlight(database *db.DB, imageID int64, backend, result string, now time.Time) error {
	backends, err := db.QueryStrings(database.Read,
		`SELECT backend FROM image_lookups
		 WHERE image_id = ? AND queued_at IS NOT NULL AND (? = '' OR backend = ?)`,
		imageID, backend, backend)
	if err != nil {
		return err
	}
	for _, b := range backends {
		if err := Record(database, imageID, b, result, 0, now); err != nil {
			return err
		}
	}
	return nil
}

// Waiting lists the attempts in flight since before cutoff, for the reconcile
// sweep. The partial in-flight index keeps this proportional to the rows
// actually waiting.
func Waiting(database *db.DB, cutoff time.Time) ([]InFlight, error) {
	return db.QueryAll(database.Read, func(rows *sql.Rows) (InFlight, error) {
		var f InFlight
		err := rows.Scan(&f.ImageID, &f.Backend, &f.JobID)
		return f, err
	}, `SELECT image_id, backend, COALESCE(job_id, 0) FROM image_lookups
		 WHERE queued_at IS NOT NULL AND queued_at <= ?`, stamp(cutoff))
}

// Reset puts an image back in the running on one backend: the ladder is
// zeroed and the backend is due immediately, while last_at and last_result
// stay so the detail page can still say when it was last looked up. The whole
// point of [look again] is that the history survives it.
func Reset(database *db.DB, imageID int64, backend string, now time.Time) error {
	_, err := database.Write.Exec(
		`UPDATE image_lookups SET attempts = 0, next_due_at = ?, ptr_cursor = NULL
		 WHERE image_id = ? AND backend = ?`, stamp(now), imageID, backend)
	return err
}

// ResetMany is Reset across a set of images on every backend at once, for the
// bulk opt-in. whereIDs is the caller's `image_id IN (...)` placeholder list
// and args its binds.
func ResetMany(e execer, whereIDs string, args []any, now time.Time) error {
	_, err := e.Exec(
		`UPDATE image_lookups SET attempts = 0, next_due_at = ?, ptr_cursor = NULL
		 WHERE image_id IN (`+whereIDs+`)`, append([]any{stamp(now)}, args...)...)
	return err
}

// execer is the write surface both *sql.DB and *sql.Tx satisfy, so a caller
// already inside a transaction drops the rows in it.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// DeleteForImage drops an image's recorded attempts. Called where the file's
// bytes change: the misses are about bytes the image no longer has, so
// keeping them as history would be a lie rather than a record.
func DeleteForImage(e execer, imageID int64) error {
	_, err := e.Exec(`DELETE FROM image_lookups WHERE image_id = ?`, imageID)
	return err
}

// ForImage reads an image's recorded attempts, keyed by backend.
func ForImage(database *db.DB, imageID int64) (map[string]Row, error) {
	rows, err := database.Read.Query(
		`SELECT backend, attempts, queued_at, last_at, last_result, next_due_at
		 FROM image_lookups WHERE image_id = ?`, imageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Row{}
	for rows.Next() {
		var r Row
		var queued, last, due sql.NullString
		if err := rows.Scan(&r.Backend, &r.Attempts, &queued, &last, &r.LastResult, &due); err != nil {
			return nil, err
		}
		r.QueuedAt, r.LastAt, r.NextDueAt = parseStamp(queued), parseStamp(last), parseStamp(due)
		out[r.Backend] = r
	}
	return out, rows.Err()
}

// Exhausted reports whether the online ladder has given up on the image, the
// state the detail page reads as "nothing found". Derived rather than stored:
// a third value in images.scheduled_lookup would let a schedule that has
// never run display a state the ladder never produced.
func (r Row) Exhausted() bool {
	return r.Backend == BackendBooru && r.LastResult == ResultMiss && r.NextDueAt.IsZero()
}

func parseStamp(v sql.NullString) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", v.String)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
