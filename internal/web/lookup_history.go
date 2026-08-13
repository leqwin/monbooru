package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
)

// lookupGrace is how long an attempt may sit in flight before the reconcile
// sweep resolves it against monloader's queue. Comfortably past a real
// backlog, so a busy queue is never mistaken for a lost callback.
const lookupGrace = 6 * time.Hour

// lookupBackendsFor expands a lookup's backend into the history rows it
// writes: "all" hits both, so both are recorded and each concludes on its
// own callback.
func lookupBackendsFor(backend string) []string {
	if backend == "all" {
		return []string{lookup.BackendPTR, lookup.BackendBooru}
	}
	return []string{backend}
}

// recordLookupEnqueued stamps an accepted enqueue as in flight. A failure to
// write history must not fail the lookup itself - the job is already queued
// on monloader - so it is logged and the attempt resolves through the sweep.
func (s *Server) recordLookupEnqueued(cx *galleryCtx, imageID int64, backend string, jobID int64) {
	if err := lookup.Enqueued(cx.DB, imageID, lookupBackendsFor(backend), jobID, time.Now()); err != nil {
		logx.Warnf("lookup history for image %d: %v", imageID, err)
	}
}

// lookupView is the detail page's read of an image's lookup state: the
// history line under the lookup button, and one on / off / nothing-found
// control per scheduled backend in the Sources field.
type lookupView struct {
	ImageID int64
	// LastAt / LastResult are the most recent concluded outcome across the
	// backends; QueuedAt is set instead while an attempt is in flight. The
	// history line under the manual Lookup button is about the image, not
	// about a phase, so it stays merged.
	LastAt     time.Time
	LastResult string
	QueuedAt   time.Time
	// ScheduleOn reports whether either phase is on in Settings, so an image
	// marked up before the schedule is ever switched on says so.
	ScheduleOn bool
	// Unsourced gates the control: an image that already has an origin with
	// a URL is not a candidate, so the control has nothing to say about it.
	Unsourced bool
	// Backends carries one entry per phase enabled in Settings, in the order
	// the run works them.
	Backends []lookupBackendView
}

// lookupBackendView is one backend's scheduled state on an image: the
// operator's opt-in for that phase and what its own ladder has recorded.
type lookupBackendView struct {
	Backend string
	Label   string
	// Enabled is the backend's images column; Exhausted is derived from the
	// online ladder having given up, never stored, so a schedule that has
	// never run cannot display a state the ladder never produced.
	Enabled    bool
	Exhausted  bool
	Attempts   int
	LastAt     time.Time
	LastResult string
	NextDueAt  time.Time
}

// Tried reports whether anything has ever been recorded for the image.
func (v lookupView) Tried() bool { return !v.LastAt.IsZero() || !v.QueuedAt.IsZero() }

// lookupViewFor assembles the detail page's lookup state.
func (s *Server) lookupViewFor(cx *galleryCtx, imageID int64) lookupView {
	v := lookupView{ImageID: imageID}
	s.cfgMu.RLock()
	sched := s.cfg.Schedule
	s.cfgMu.RUnlock()
	v.ScheduleOn = sched.LookupPTR || sched.LookupBooru

	ptrOn, booruOn := true, true
	var sourced int
	if err := cx.DB.Read.QueryRow(
		`SELECT i.scheduled_lookup_ptr, i.scheduled_lookup,
		        EXISTS (SELECT 1 FROM image_sources s WHERE s.image_id = i.id AND s.url <> '')
		 FROM images i WHERE i.id = ?`, imageID,
	).Scan(&ptrOn, &booruOn, &sourced); err != nil {
		logx.Warnf("lookup view for image %d: %v", imageID, err)
		return v
	}
	v.Unsourced = sourced == 0

	rows, err := lookup.ForImage(cx.DB, imageID)
	if err != nil {
		logx.Warnf("lookup view for image %d: %v", imageID, err)
		return v
	}
	for _, r := range rows {
		if r.LastAt.After(v.LastAt) {
			v.LastAt, v.LastResult = r.LastAt, r.LastResult
		}
		if !r.QueuedAt.IsZero() && (v.QueuedAt.IsZero() || r.QueuedAt.Before(v.QueuedAt)) {
			v.QueuedAt = r.QueuedAt
		}
	}
	for _, b := range []struct {
		backend, label string
		on, optedIn    bool
	}{
		{lookup.BackendPTR, "PTR", sched.LookupPTR, ptrOn},
		{lookup.BackendBooru, "booru", sched.LookupBooru, booruOn},
	} {
		if !b.on {
			continue
		}
		r := rows[b.backend]
		v.Backends = append(v.Backends, lookupBackendView{
			Backend: b.backend, Label: b.label, Enabled: b.optedIn,
			Exhausted: r.Exhausted(), Attempts: r.Attempts,
			LastAt: r.LastAt, LastResult: r.LastResult, NextDueAt: r.NextDueAt,
		})
	}
	return v
}

// scheduledLookupBackend reads the backend a per-image control posts,
// defaulting to the online one so a form that names none stays on the
// backend the ladder can exhaust.
func scheduledLookupBackend(r *http.Request) string {
	if r.FormValue("backend") == lookup.BackendPTR {
		return lookup.BackendPTR
	}
	return lookup.BackendBooru
}

// scheduledLookupPost flips the operator's per-image opt-out for one backend
// and re-renders the control for an htmx swap.
func (s *Server) scheduledLookupPost(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	cx := s.Active()
	if cx == nil {
		externalErr(w, r, "no active gallery", http.StatusServiceUnavailable)
		return
	}
	backend := scheduledLookupBackend(r)
	on := 0
	if r.FormValue("on") == "1" {
		on = 1
	}
	if _, err := cx.DB.Write.Exec(
		`UPDATE images SET `+lookup.FlagColumn(backend)+` = ? WHERE id = ?`, on, id); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	// Turning it back on an exhausted image is the same act as [look again]:
	// the operator is saying to try this one, and leaving a spent ladder
	// behind would mean nothing happens.
	if on == 1 {
		if err := lookup.Reset(cx.DB, id, backend, time.Now()); err != nil {
			logx.Warnf("lookup reset for image %d: %v", id, err)
		}
	}
	// The `lookup:` filters are membership queries over this flag and the
	// ladder, so the match-id snapshots that pre-date the write have to go.
	cx.InvalidateCaches()
	s.renderScheduledLookup(w, r, cx, id)
}

// scheduledLookupResetPost is [look again]: the ladder is zeroed and the
// image is due immediately, while the history stays so the page can still
// say when it was last looked up.
func (s *Server) scheduledLookupResetPost(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	cx := s.Active()
	if cx == nil {
		externalErr(w, r, "no active gallery", http.StatusServiceUnavailable)
		return
	}
	if err := lookup.Reset(cx.DB, id, scheduledLookupBackend(r), time.Now()); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	cx.InvalidateCaches()
	s.renderScheduledLookup(w, r, cx, id)
}

func (s *Server) renderScheduledLookup(w http.ResponseWriter, r *http.Request, cx *galleryCtx, id int64) {
	s.renderTemplate(w, "partials/scheduled_lookup.html", map[string]any{
		"Lookup":    s.lookupViewFor(cx, id),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	})
}

// reconcileLookups resolves every attempt that has been in flight past
// lookupGrace against monloader's queue. It runs from the reclaim ticker and
// at the start of the online phase, never from a render path: a detail page
// renders what the row says, and a monloader round trip there would spend the
// page's whole latency budget on a link that may be down.
//
// Returns how many rows it resolved and how many of those were inconclusive,
// which is what lets the phase summary call out a monloader that is dropping
// the night's work rather than leaving the operator with a run that appears
// to do nothing.
func (s *Server) reconcileLookups(ctx context.Context, cx *galleryCtx) (resolved, inconclusive int) {
	waiting, err := lookup.Waiting(cx.DB, time.Now().Add(-lookupGrace))
	if err != nil {
		logx.Warnf("lookup reconcile %q: %v", cx.Name, err)
		return 0, 0
	}
	for _, f := range waiting {
		if ctx.Err() != nil {
			return resolved, inconclusive
		}
		result, ok := s.lookupJobOutcome(ctx, f.JobID)
		if !ok {
			continue
		}
		if result == "" {
			// Evidence about the plumbing, not the image: clear the
			// in-flight state, leave the ladder, and make it due now. The
			// phase orders by next_due_at, so a repeatedly-dropped image
			// falls to the back of the backlog instead of re-consuming a
			// budget slot every run.
			inconclusive++
			result = lookup.ResultError
		}
		if err := lookup.Record(cx.DB, f.ImageID, f.Backend, result, 0, time.Now()); err != nil {
			logx.Warnf("lookup reconcile %q image %d: %v", cx.Name, f.ImageID, err)
			continue
		}
		resolved++
	}
	return resolved, inconclusive
}

// lookupJobOutcome asks monloader what became of one job. The second return
// is false while the job is still working, so the row stays in flight; an
// empty result is inconclusive - the job provably never produced an answer
// about the image, so the ladder must not move. Without that rule a monloader
// down for six ladder rungs would walk an image to "nothing found" without a
// single lookup having run.
func (s *Server) lookupJobOutcome(ctx context.Context, jobID int64) (string, bool) {
	if jobID == 0 {
		return "", true // nothing to ask about; an older monloader reported no id
	}
	resp, err := s.monloaderDo(ctx, http.MethodGet, "/api/v1/queue/"+strconv.FormatInt(jobID, 10), nil)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		// Aged out of the finished ring, or never existed.
		return "", true
	}
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var job struct {
		Status string `json:"status"`
		Items  []struct {
			Outcome   string `json:"outcome"`
			ErrorCode string `json:"error_code"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return "", false
	}
	switch job.Status {
	case "queued", "running":
		return "", false
	case "canceled", "interrupted":
		return "", true
	case "failed":
		return lookup.ResultError, true
	}
	// It ran and the callback was lost; the item says what it found.
	for _, it := range job.Items {
		if it.Outcome == "enriched" {
			return lookup.ResultHit, true
		}
		if it.ErrorCode == "hash_not_found" {
			return lookup.ResultMiss, true
		}
	}
	return lookup.ResultError, true
}

// reconcileAllLookups sweeps every open gallery, for the reclaim ticker.
func (s *Server) reconcileAllLookups() {
	if !s.monloaderUsable() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s.ctxMu.RLock()
	ctxs := make([]*galleryCtx, 0, len(s.contexts))
	for _, cx := range s.contexts {
		ctxs = append(ctxs, cx)
	}
	s.ctxMu.RUnlock()
	for _, cx := range ctxs {
		s.reconcileLookups(ctx, cx)
	}
}
