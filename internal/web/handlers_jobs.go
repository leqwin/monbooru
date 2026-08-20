package web

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/models"
)

func (s *Server) jobDismissPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Dismiss()
	w.WriteHeader(http.StatusNoContent)
}

// jobCancelPost aborts the running job by cancelling its context. Workers
// observing ctx.Done() wrap up and call Complete/Fail themselves.
func (s *Server) jobCancelPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) jobStatusHandler(w http.ResponseWriter, r *http.Request) {
	// Mark before Get so the first render of a completed state starts the
	// short post-view dismiss timer. Subsequent views don't re-arm it.
	s.jobs.MarkViewed()
	state := s.jobs.Get()
	s.renderTemplate(w, "partials/job_status.html", state)
}

func (s *Server) syncTrigger(w http.ResponseWriter, r *http.Request) {
	if cx := s.Active(); cx == nil || cx.Degraded {
		// Same escaped-fragment shape as the busy-job refusal below it:
		// the topbar swaps this body straight into #sync-flash.
		flashStatus(w, http.StatusServiceUnavailable, "Sync unavailable: gallery path is unreadable.")
		return
	}
	if !s.startJob(w, models.JobTypeSync) {
		return
	}
	// Snapshot the active gallery's state under the request's RLock so the
	// background goroutine is not racing a subsequent swap. The IsRunning
	// guard in SwitchGallery refuses swaps while the sync runs, so these
	// handles stay valid for the job's lifetime.
	cx := s.Active()
	maxFileSizeMB := s.maxFileSizeMB()
	go func() {
		ctx := s.jobs.Context()
		// cx.Sync wraps gallery.Sync + InvalidateCaches so this caller
		// can't drift from the contract (a future code path that
		// returned early between the two would leave caches stale).
		result, err := cx.Sync(ctx, maxFileSizeMB, s.ingestNaming(cx.Name), s.jobs.Update)
		if ctx.Err() != nil {
			s.jobs.Complete("sync cancelled")
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(result.Summary())
	}()

	redirectTo := sameOriginReferer(r)
	if isHTMXRequest(r) {
		// Signal the client to reload the gallery when the job finishes.
		w.Header().Set("HX-Trigger", "syncStarted")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}
