package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/models"
)

// ptrLookupChunk is how many hashes one batch call carries, matching
// monloader's per-request cap.
const ptrLookupChunk = 100

// lookupCandidate is one image the phases work on.
type lookupCandidate struct {
	id            int64
	sha256        string
	md5           string // "" until the row is backfilled; the phase hashes and stores one then
	canonicalPath string
}

// lookupMD5 answers the digest a booru lookup is keyed on. Boorus index
// posts by md5, so every lookup needs one; reading the stored column
// keeps a scheduled run from re-reading the whole candidate set off disk
// the way it did before the column existed. A row that predates it is
// hashed once here and keeps the result.
func lookupMD5(ctx context.Context, cx *galleryCtx, imageID int64, stored string) (string, error) {
	if stored != "" {
		return stored, nil
	}
	return gallery.ComputeAndStoreMD5(ctx, cx.DB, imageID)
}

// onlineLookupChunk is how many due images one selection page carries. Rows
// drop out of the next page as they are enqueued, so the phase walks the
// whole backlog a page at a time and stops when monloader refuses.
const onlineLookupChunk = 100

// runPTRLookupPhase checks every due image against monloader's local index in
// batches. Free per image, so it has no budget and no pacing beyond the
// ladder: what keeps a large library from a full pass every night is the
// delay, and what makes a caught-up idle index cost nothing is the cursor.
//
// A transport failure ends the phase with what it managed and leaves the
// unprocessed rows untouched, so the next run retries them.
func (s *Server) runPTRLookupPhase(ctx context.Context, cx *galleryCtx) (checked, matched int, err error) {
	cursor, cerr := s.ptrIndexCursor(ctx)
	if cerr != nil {
		return 0, 0, cerr
	}
	lookup.PTRCursor.Store(cursor)

	for {
		if ctx.Err() != nil {
			return checked, matched, nil
		}
		batch, berr := s.dueLookupCandidates(cx, lookup.BackendPTR, ptrLookupChunk)
		if berr != nil {
			return checked, matched, berr
		}
		if len(batch) == 0 {
			break
		}
		images := make([]ptrLookupImage, len(batch))
		for i, c := range batch {
			images[i] = ptrLookupImage{ImageID: c.id, SHA256: c.sha256}
		}
		results, answered, lerr := s.ptrBatchLookup(ctx, cx.Name, true, images)
		if lerr != nil {
			return checked, matched, lerr
		}
		if answered > 0 {
			cursor = answered
			lookup.PTRCursor.Store(cursor)
		}
		byHash := make(map[string]int64, len(batch))
		for _, c := range batch {
			byHash[c.sha256] = c.id
		}
		hits, recorded, aerr := applyPTRResults(cx, byHash, results, cursor)
		matched += hits
		checked += recorded
		if aerr != nil {
			return checked, matched, aerr
		}
		// The paging has no offset, so a page nothing was recorded against
		// comes back identical; end the phase with what it managed.
		if recorded == 0 {
			break
		}
		s.jobs.Update(checked, checked+len(batch), fmt.Sprintf("[%s] PTR lookup…", cx.Name))
	}
	// Every recorded outcome moves `lookup:` membership, so the match-id
	// snapshots that pre-date the pass have to go.
	cx.InvalidateCaches()
	return checked, matched, nil
}

// applyPTRResults applies every hit in one batch answer and records each hash's
// outcome against the cursor it was answered at. A row whose apply fails is
// left unrecorded so the next pass retries it, which is why recorded - not the
// batch length - is what tells a caller paging through the backlog that the
// page moved. Returns the first record failure, having tried the rest.
func applyPTRResults(cx *galleryCtx, byHash map[string]int64, results map[string][]string, cursor uint64) (matched, recorded int, err error) {
	for sha, id := range byHash {
		tags, hit := results[sha]
		result := lookup.ResultMiss
		if hit {
			if aerr := gallery.ApplyPTRTags(cx.DB, cx.TagSvc, id, tags); aerr != nil {
				logx.Warnf("ptr apply for image %d: %v", id, aerr)
				continue
			}
			result = lookup.ResultHit
			matched++
		}
		if rerr := lookup.Record(cx.DB, id, lookup.BackendPTR, result, cursor, time.Now()); rerr != nil {
			logx.Warnf("ptr record for image %d: %v", id, rerr)
			if err == nil {
				err = rerr
			}
			continue
		}
		recorded++
	}
	return matched, recorded, err
}

// ptrIndexCursor reads monloader's applied-update position once per run, so
// the due gate sees a fresh value even on a box nobody has a page open on.
func (s *Server) ptrIndexCursor(ctx context.Context) (uint64, error) {
	resp, err := s.monloader().Do(ctx, http.MethodGet, "/api/v1/ptr/status", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, peerStatusError{monloaderApp, resp.Status}
	}
	var out struct {
		Progress struct {
			UpdateIndex uint64 `json:"update_index"`
		} `json:"progress"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Progress.UpdateIndex, nil
}

// dueLookupCandidates reads the next page of images due on a backend. Rows
// drop out of the next page as they are recorded, so a plain LIMIT walks the
// whole set without an offset.
func (s *Server) dueLookupCandidates(cx *galleryCtx, backend string, limit int) ([]lookupCandidate, error) {
	due, args := lookup.DueClause(backend, time.Now())
	return db.QueryAll(cx.DB.Read, func(rows *sql.Rows) (lookupCandidate, error) {
		var c lookupCandidate
		err := rows.Scan(&c.id, &c.sha256, &c.md5, &c.canonicalPath)
		return c, err
	}, `SELECT i.id, i.sha256, i.md5, i.canonical_path
		 FROM images i
		 LEFT JOIN image_lookups l ON l.image_id = i.id AND l.backend = ?
		 WHERE `+lookup.CandidateClause(backend)+` AND `+due+`
		 ORDER BY l.next_due_at IS NOT NULL, l.next_due_at, i.id
		 LIMIT ?`,
		append(append([]any{backend}, args...), limit)...)
}

// dueLookupCount is how many images a backend still owes, for the summaries.
func (s *Server) dueLookupCount(cx *galleryCtx, backend string) int {
	due, args := lookup.DueClause(backend, time.Now())
	var n int
	if err := cx.DB.Read.QueryRow(
		`SELECT COUNT(*) FROM images i WHERE `+lookup.CandidateClause(backend)+` AND `+due, args...,
	).Scan(&n); err != nil {
		logx.Warnf("lookup due count %q: %v", cx.Name, err)
	}
	return n
}

// onlineLookupResult is what one online phase managed.
type onlineLookupResult struct {
	queued       int
	skipped      int
	stillDue     int
	budgetSpent  bool
	resolved     int
	inconclusive int
}

// runOnlineLookupPhase enqueues a hash lookup on monloader for every due
// image, on the background lane and against the daily budget, until the
// budget is spent or the backlog runs out.
//
// It reconciles first so the selection sees the truth rather than rows stuck
// from a previous run, and orders never-tried images ahead of retries and the
// longest-overdue retry ahead of a fresher one. That ordering is what makes
// an inconclusive result self-limiting: it re-dates the row to now, which
// sorts behind everything that was already overdue.
func (s *Server) runOnlineLookupPhase(ctx context.Context, cx *galleryCtx) (onlineLookupResult, error) {
	var res onlineLookupResult
	res.resolved, res.inconclusive = s.reconcileLookups(ctx, cx)
	for ctx.Err() == nil {
		batch, err := s.dueLookupCandidates(cx, lookup.BackendBooru, onlineLookupChunk)
		if err != nil {
			return res, err
		}
		if len(batch) == 0 {
			break
		}
		queuedBefore := res.queued
		for _, c := range batch {
			if ctx.Err() != nil {
				break
			}
			md5, herr := lookupMD5(ctx, cx, c.id, c.md5)
			if herr != nil {
				// An unreadable file is skipped and counted, never recorded
				// as a miss: nothing was looked up.
				res.skipped++
				continue
			}
			jobID, eerr := s.EnqueueHashLookup(ctx, c.id, cx.Name, lookup.BackendBooru, md5, c.sha256, true, true)
			switch {
			case errors.Is(eerr, errLookupBudgetSpent):
				res.budgetSpent = true
				res.stillDue = s.dueLookupCount(cx, lookup.BackendBooru)
				cx.InvalidateCaches()
				return res, nil
			case isPeerStatusErr(eerr):
				res.skipped++
				continue
			case eerr != nil:
				return res, eerr
			}
			s.recordLookupEnqueued(cx, c.id, lookup.BackendBooru, jobID)
			res.queued++
			s.jobs.Update(res.queued, res.queued+len(batch), fmt.Sprintf("[%s] online lookup…", cx.Name))
		}
		// The paging has no offset, so a page nothing was enqueued from -
		// unreadable files, a monloader refusing every row - comes back
		// identical; end the phase with its skip count.
		if res.queued == queuedBefore {
			break
		}
	}
	res.stillDue = s.dueLookupCount(cx, lookup.BackendBooru)
	cx.InvalidateCaches()
	return res, nil
}

// scheduledOnlineLookup is the scheduler's online phase.
func (s *Server) scheduledOnlineLookup(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypeLookup, "online lookup", cx.Name)
	if err != nil {
		return err
	}
	res, err := s.runOnlineLookupPhase(ctx, cx)
	if err != nil {
		s.jobs.Fail(err.Error())
		return err
	}
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] online lookup cancelled (%d queued)", cx.Name, res.queued))
		return nil
	}
	summary := fmt.Sprintf("[%s] Online lookup: %d queued on monloader.", cx.Name, res.queued)
	if res.budgetSpent {
		summary += fmt.Sprintf(" Daily budget reached; %d still due.", res.stillDue)
	} else if res.stillDue > 0 {
		summary += fmt.Sprintf(" %d still due.", res.stillDue)
	}
	s.jobs.Complete(summary)
	s.recordLookupRun(summary)
	// A monloader dropping most of a night's work is an uptime problem the
	// operator has to be told about, not a phase that quietly achieves
	// nothing.
	if res.resolved > 0 && res.inconclusive*2 > res.resolved {
		s.recordLookupRun(fmt.Sprintf("[%s] Online lookup: monloader dropped %d of the %d lookups queued last run.",
			cx.Name, res.inconclusive, res.resolved))
	}
	return nil
}

// lookupDuePost is the Maintenance row's "do it now": it runs the enabled
// phases on the active gallery, honouring the ladder and the budget. A "do it
// now" button, not a "do it again" one, so it cannot be leaned on to burn a
// day's quota; forcing a specific slice is what the `lookup:` filter plus the
// batch action are for.
func (s *Server) lookupDuePost(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil {
		writeInlineFlash(w, "err", "no active gallery")
		return
	}
	if !s.monloaderUsable() {
		writeInlineFlash(w, "err", "monloader is unreachable")
		return
	}
	s.cfgMu.RLock()
	sched := s.cfg.Schedule
	s.cfgMu.RUnlock()
	if !sched.LookupPTR && !sched.LookupBooru {
		writeInlineFlash(w, "err", "no lookup is enabled in Settings > Schedule")
		return
	}
	if !s.startJob(w, models.JobTypeLookup) {
		return
	}
	go func() {
		ctx := s.jobs.Context()
		var parts []string
		if sched.LookupPTR {
			if !s.monloaderPTRReady() {
				parts = append(parts, "PTR lookup skipped: the index is not ready on monloader.")
			} else {
				checked, matched, err := s.runPTRLookupPhase(ctx, cx)
				switch {
				case err != nil && !errors.Is(err, errPTRBatchUnsupported):
					s.jobs.Fail(err.Error())
					return
				case checked == 0:
					parts = append(parts, "PTR lookup: nothing is due.")
				default:
					parts = append(parts, fmt.Sprintf("PTR lookup: %d checked, %d matched.", checked, matched))
				}
			}
		}
		if sched.LookupBooru && ctx.Err() == nil {
			res, err := s.runOnlineLookupPhase(ctx, cx)
			if err != nil {
				s.jobs.Fail(err.Error())
				return
			}
			line := fmt.Sprintf("Online lookup: %d queued on monloader.", res.queued)
			if res.budgetSpent {
				line += fmt.Sprintf(" Daily budget reached; %d still due.", res.stillDue)
			}
			parts = append(parts, line)
		}
		s.jobs.Complete(strings.Join(parts, " "))
	}()
	writeInlineFlash(w, "ok", "Lookup started. Watch the status bar.")
}

// scheduledPTRLookup is the scheduler's PTR phase: one job so the bar shows a
// real count and Cancel works.
func (s *Server) scheduledPTRLookup(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypeLookup, "ptr lookup", cx.Name)
	if err != nil {
		return err
	}
	checked, matched, err := s.runPTRLookupPhase(ctx, cx)
	summary := fmt.Sprintf("[%s] PTR lookup: %d checked, %d matched, %d no match.",
		cx.Name, checked, matched, checked-matched)
	switch {
	case errors.Is(err, errPTRBatchUnsupported):
		s.jobs.Complete(fmt.Sprintf("[%s] PTR lookup skipped: %s.", cx.Name, err.Error()))
		return nil
	case err != nil:
		s.jobs.Fail(err.Error())
		return err
	case ctx.Err() != nil:
		s.jobs.Complete(fmt.Sprintf("[%s] PTR lookup cancelled (%d checked, %d matched)", cx.Name, checked, matched))
		return nil
	case checked == 0:
		summary = fmt.Sprintf("[%s] PTR lookup: nothing to check; the index has not moved since the last run.", cx.Name)
	}
	s.jobs.Complete(summary)
	s.recordLookupRun(summary)
	return nil
}
