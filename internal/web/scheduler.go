package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tagger"
)

// runScheduler is a background goroutine that fires once per day at
// cfg.Schedule.Time and runs the enabled actions sequentially on every
// configured gallery. Started from NewServer; exits when s.done is closed.
func (s *Server) runScheduler() {
	for {
		next, ok := s.nextScheduledFire(time.Now())
		if !ok {
			// No action enabled (or invalid time). Sleep an hour then re-check
			// so Settings edits pick up without a server restart - and wake
			// early when a save signals via schedReload.
			select {
			case <-s.done:
				return
			case <-s.schedReload:
				continue
			case <-time.After(time.Hour):
				continue
			}
		}
		d := max(time.Until(next), 0)
		logx.Infof("scheduler: next run at %s (in %s)", next.Format(time.RFC3339), d.Round(time.Second))
		select {
		case <-s.done:
			return
		case <-s.schedReload:
			continue
		case <-time.After(d):
			s.runScheduledActions()
		}
	}
}

// nextScheduledFire returns the next local time cfg.Schedule.Time will hit.
// Returns ok=false when no schedule flag is enabled or the time is unparseable.
func (s *Server) nextScheduledFire(now time.Time) (time.Time, bool) {
	s.cfgMu.RLock()
	sched := s.cfg.Schedule
	s.cfgMu.RUnlock()
	if !schedHasAnyEnabled(sched) {
		return time.Time{}, false
	}
	t, err := parseScheduleTime(sched.Time)
	if err != nil {
		return time.Time{}, false
	}
	year, month, day := now.Date()
	fire := time.Date(year, month, day, t.hour, t.minute, 0, 0, now.Location())
	if !fire.After(now) {
		// time.Date normalises components, so passing day+1 walks the
		// calendar through DST transitions correctly. Add(24h) would
		// slip the local fire time by an hour twice a year.
		fire = time.Date(year, month, day+1, t.hour, t.minute, 0, 0, now.Location())
	}
	return fire, true
}

type schedTime struct{ hour, minute int }

func parseScheduleTime(v string) (schedTime, error) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return schedTime{}, fmt.Errorf("bad time %q", v)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return schedTime{}, fmt.Errorf("bad hour in %q", v)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return schedTime{}, fmt.Errorf("bad minute in %q", v)
	}
	return schedTime{hour: h, minute: m}, nil
}

func schedHasAnyEnabled(sc config.ScheduleConfig) bool {
	return sc.SyncGallery || sc.RemoveOrphans || sc.RunAutoTaggers || sc.FindRelationPairs ||
		sc.LookupPTR || sc.LookupBooru
}

// runScheduledActions iterates every configured gallery and runs the enabled
// maintenance actions in a fixed order: sync → remove orphans → autotag →
// find-pairs → PTR lookup → online lookup.
// The lookup phases come last because they are the ones an external limit can
// cut short, and cutting them short must not cost the rest of the run. After
// autotag deliberately: the tag write and the lookup write on the same row
// are better not interleaved.
// Skips the whole run when a user-triggered job is already holding the
// job manager. The reservation blocks user-triggered Start() calls for
// the duration so the lock-less phases (RemoveOrphans) can't be raced
// by external handlers.
func (s *Server) runScheduledActions() {
	if err := s.jobs.BeginSchedule(); err != nil {
		logx.Warnf("scheduler: skipping run (a job is already running)")
		return
	}
	defer s.jobs.EndSchedule()

	started := time.Now()
	s.clearLookupRun()
	var failures []string
	cancelled := false
	defer func() {
		info := "OK"
		switch {
		case len(failures) > 0:
			info = strings.Join(failures, "; ")
		case cancelled:
			info = "cancelled"
		}
		s.recordScheduleRun(started, time.Since(started), info)
	}()

	s.cfgMu.RLock()
	sched := s.cfg.Schedule
	s.cfgMu.RUnlock()

	s.ctxMu.RLock()
	names := make([]string, 0, len(s.contexts))
	for name := range s.contexts {
		names = append(names, name)
	}
	s.ctxMu.RUnlock()
	// Sorted, then rotated to the gallery after the one the last run stopped
	// at. A daily budget too small to cover gallery 1 would otherwise starve
	// every other gallery permanently, and map order gives no rotation to
	// build on. Losing the offset on a restart costs one unfair run.
	slices.Sort(names)
	names = rotateStrings(names, s.nextScheduleOffset(len(names)))

	// User cancel mid-run clears scheduleHeld (Manager.Cancel does this)
	// so the outer loop can bail at the next phase boundary. Without
	// the gate, the cancelled phase would observe ctx.Err and complete,
	// then the next phase's StartScheduled would fire and run normally,
	// so one user click would cancel exactly one phase rather than the
	// remaining run.
	abort := func() bool {
		if !s.jobs.IsScheduleHeld() {
			logx.Infof("scheduler: run cancelled mid-flight; remaining phases skipped")
			cancelled = true
			return true
		}
		return false
	}

	for _, name := range names {
		if abort() {
			return
		}
		cx := s.Get(name)
		if cx == nil {
			continue
		}
		logx.Infof("scheduler: running actions on gallery %q", name)

		if sched.SyncGallery && !cx.Degraded {
			if err := s.scheduledSync(cx); err != nil {
				failures = append(failures, "sync "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if sched.RemoveOrphans {
			if err := s.scheduledRemoveOrphans(cx); err != nil {
				failures = append(failures, "remove-orphans "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if sched.RunAutoTaggers && tagger.IsAvailable(s.cfgSnapshot()) {
			if err := s.scheduledAutotag(cx); err != nil {
				failures = append(failures, "autotag "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if sched.FindRelationPairs {
			if err := s.scheduledFindRelationPairs(cx); err != nil {
				failures = append(failures, "find-pairs "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		// A saved checkbox survives monloader going away: the phase is
		// skipped with a line in the summary, never silently unset.
		if sched.LookupPTR {
			switch {
			case !s.monloaderUsable():
				s.recordLookupRun("[" + name + "] Lookup skipped: monloader unreachable.")
			case !s.monloaderPTRReady():
				s.recordLookupRun("[" + name + "] PTR lookup skipped: monloader's index is not ready.")
			default:
				if err := s.scheduledPTRLookup(cx); err != nil {
					failures = append(failures, "ptr-lookup "+name+": "+err.Error())
				}
			}
			if abort() {
				return
			}
		}
		if sched.LookupBooru {
			if !s.monloaderUsable() {
				s.recordLookupRun("[" + name + "] Online lookup skipped: monloader unreachable.")
			} else if err := s.scheduledOnlineLookup(cx); err != nil {
				failures = append(failures, "online-lookup "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
	}
}

// monloaderPTRReady reports the cached PTR capability, so a phase can skip a
// backend monloader would only 409 anyway.
func (s *Server) monloaderPTRReady() bool {
	_, _, ready, _, _ := s.monloaderStatusSeed()
	return ready
}

// startScheduledPhase claims the job lane for one scheduled phase and returns
// the context it runs under. phase and gallery name the warning a refused
// claim logs.
func (s *Server) startScheduledPhase(jobType, phase, gallery string) (context.Context, error) {
	if err := s.jobs.StartScheduled(jobType); err != nil {
		logx.Warnf("scheduler %s %q: %v", phase, gallery, err)
		return nil, err
	}
	return s.jobs.Context(), nil
}

// nextScheduleOffset returns where this run starts in the gallery list and
// advances the stored position for the next one.
func (s *Server) nextScheduleOffset(n int) int {
	if n <= 1 {
		return 0
	}
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	start := s.schedGalleryOffset % n
	s.schedGalleryOffset = (start + 1) % n
	return start
}

// rotateStrings returns names starting at offset and wrapping around.
func rotateStrings(names []string, offset int) []string {
	if offset <= 0 || offset >= len(names) {
		return names
	}
	return append(append([]string(nil), names[offset:]...), names[:offset]...)
}

func (s *Server) scheduledFindRelationPairs(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypeRelations, "find-pairs", cx.Name)
	if err != nil {
		return err
	}
	s.cfgMu.RLock()
	tagPairs := s.cfg.Relations.TagPairs
	tagPairThreshold := s.cfg.Relations.TagPairThreshold
	s.cfgMu.RUnlock()
	opts := relations.FindPairsOptions{
		Distance:         int(relations.IncrementalProbeDistance.Load()),
		Replace:          false,
		ThumbnailsPath:   cx.ThumbnailsPath,
		TagPairs:         tagPairs,
		TagPairThreshold: config.ClampTagPairThreshold(tagPairThreshold),
	}
	added, err := relations.FindPairs(ctx, cx.DB, cx.bkTree, opts, s.jobs.Update)
	if err == context.Canceled || ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] find-pairs cancelled (%d added)", cx.Name, added))
		return nil
	}
	if err != nil {
		s.jobs.Fail(err.Error())
		return err
	}
	s.jobs.Complete(fmt.Sprintf("[%s] find-pairs added %d candidate(s).", cx.Name, added))
	return nil
}

func (s *Server) scheduledSync(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypeSync, "sync", cx.Name)
	if err != nil {
		return err
	}
	result, err := cx.Sync(ctx, s.maxFileSizeMB(), s.ingestNaming(cx.Name), s.jobs.Update)
	// Match the user-trigger handlers' shape: ctx cancellation produces
	// a clean Complete summary, only real failures fall to Fail().
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] sync cancelled (%s)", cx.Name, result.Summary()))
		return nil
	}
	if err != nil {
		s.jobs.Fail(err.Error())
		logx.Warnf("scheduler sync %q: %v", cx.Name, err)
		return err
	}
	s.jobs.Complete(fmt.Sprintf("[%s] %s", cx.Name, result.Summary()))
	return nil
}

func (s *Server) scheduledRemoveOrphans(cx *galleryCtx) error {
	ctx, err := s.startScheduledPhase(models.JobTypePruneThumbs, "orphans", cx.Name)
	if err != nil {
		return err
	}
	removed, processed, total, err := s.runOrphanSweep(ctx, cx)
	if err != nil {
		s.jobs.Fail(err.Error())
		logx.Warnf("scheduler orphans %q: %v", cx.Name, err)
		return err
	}
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] orphan sweep cancelled (%d/%d scanned, %d removed)", cx.Name, processed, total, removed))
		return nil
	}
	s.jobs.Complete(fmt.Sprintf("[%s] removed %d orphaned thumbnail(s)", cx.Name, removed))
	logx.Infof("scheduler: [%s] removed %d orphaned thumbnail(s)", cx.Name, removed)
	return nil
}

// runOrphanSweep walks the thumbnails directory and unlinks files
// whose id no longer matches a row in images. ctx aborts the sweep at
// the next entry; the returned counts reflect partial progress so the
// caller's cancelled summary stays accurate. Shared by the scheduler
// (StartScheduled wrapper) and the user-triggered prune handler
// (Start + goroutine wrapper) so the actual sweep lives in one place.
//
// Returns (removed, processed, total, err): removed is the number of
// orphan files unlinked, processed is the number of directory entries
// inspected (including non-thumbnail bystanders that are kept), total
// is the entry count from the initial ReadDir, err is set only when
// the prerequisite reads (ReadDir, the SELECT id FROM images cursor)
// fail. A truncated cursor returns err so the sweep doesn't delete
// legit thumbnails as orphans.
func (s *Server) runOrphanSweep(ctx context.Context, cx *galleryCtx) (removed, processed, total int, err error) {
	entries, err := os.ReadDir(cx.ThumbnailsPath)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read thumbnails dir: %w", err)
	}
	total = len(entries)
	ids, err := db.QueryIDsContext(ctx, cx.DB.Read, `SELECT id FROM images`)
	if err != nil {
		return 0, 0, total, err
	}
	known := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		known[id] = struct{}{}
	}

	s.jobs.Update(0, total, fmt.Sprintf("[%s] pruning…", cx.Name))
	for i, e := range entries {
		if ctx.Err() != nil {
			return removed, processed, total, nil
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var idStr string
		switch {
		case strings.HasSuffix(name, "_hover.webp"):
			idStr = strings.TrimSuffix(name, "_hover.webp")
		case strings.HasSuffix(name, "_view.jpg"):
			idStr = strings.TrimSuffix(name, "_view.jpg")
		case strings.HasSuffix(name, ".jpg"):
			idStr = strings.TrimSuffix(name, ".jpg")
		default:
			continue
		}
		id, parseErr := strconv.ParseInt(idStr, 10, 64)
		if parseErr != nil {
			continue
		}
		processed++
		if _, ok := known[id]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(cx.ThumbnailsPath, name)); err == nil {
			removed++
		}
		if (i+1)%50 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, fmt.Sprintf("[%s] pruning…", cx.Name))
		}
	}
	return removed, processed, total, nil
}

func (s *Server) scheduledAutotag(cx *galleryCtx) error {
	ids, err := db.QueryIDs(cx.DB.Read,
		`SELECT i.id FROM images i WHERE i.is_missing = 0
		 AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.is_auto = 1)`,
	)
	if err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	cfg := s.cfgSnapshot()
	enabled := tagger.EnabledTaggersForGallery(cfg, cx.Name)
	if len(enabled) == 0 {
		return nil
	}
	ctx, err := s.startScheduledPhase(models.JobTypeAutotag, "autotag", cx.Name)
	if err != nil {
		return err
	}
	baseline := readVmRSS()
	skipped, err := tagger.RunWithTaggers(ctx, cx.DB, cfg, ids, enabled, s.jobs, cfg.Tagger.ExecutionProvider, cx.MangaCacheDir())
	err = s.completeAutotagRun(cx, ctx, "["+cx.Name+"] ", "",
		"scheduled "+cx.Name, len(ids), skipped, baseline, err)
	if err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
	}
	return err
}

// recordScheduleRun stores the completion of a scheduler run so the Schedule
// settings section can show "Last run: ... (OK, 3m12s)". info is a short
// status string ("OK" or a failure summary).
func (s *Server) recordScheduleRun(started time.Time, dur time.Duration, info string) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	s.schedLastRun = started
	s.schedLastDur = dur
	s.schedLastInfo = info
}

// recordLookupRun keeps a lookup phase's summary for the Schedule section's
// own status line, which the run-level "OK" cannot carry: the operator wants
// to know what the night found, not only that it finished. Lines accumulate
// across the run's phases and galleries; clearLookupRun starts the next run
// from empty.
func (s *Server) recordLookupRun(summary string) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	s.schedLookupInfo = append(s.schedLookupInfo, summary)
}

func (s *Server) clearLookupRun() {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	s.schedLookupInfo = nil
}

// ScheduleStatus reports the last recorded scheduler run plus the next fire
// time. Used by the Schedule settings section.
type ScheduleStatus struct {
	LastRun  time.Time     // zero value when no run has happened yet
	LastDur  time.Duration // zero when LastRun is zero
	LastInfo string        // "OK" or a short failure summary; empty when never run
	NextRun  time.Time     // zero when no schedule action is enabled
	// LookupInfo is what the last run's lookup phases found, one line per
	// phase and gallery; empty when neither phase ran.
	LookupInfo []string
}

// ScheduleStatus returns the current scheduler status for the settings page.
func (s *Server) ScheduleStatus() ScheduleStatus {
	s.schedMu.Lock()
	st := ScheduleStatus{
		LastRun: s.schedLastRun, LastDur: s.schedLastDur, LastInfo: s.schedLastInfo,
		LookupInfo: append([]string(nil), s.schedLookupInfo...),
	}
	s.schedMu.Unlock()
	if next, ok := s.nextScheduledFire(time.Now()); ok {
		st.NextRun = next
	}
	return st
}
