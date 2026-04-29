package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tagger"
)

// runScheduler is a background goroutine that fires once per day at
// cfg.Schedule.Time and runs the enabled actions sequentially on every
// configured gallery. Started from NewServer; exits when s.done is closed.
func (s *Server) runScheduler() {
	for {
		next, ok := s.nextScheduledFire(time.Now())
		if !ok {
			// No action enabled (or invalid time). Sleep an hour then re-check
			// so Settings edits pick up without a server restart.
			select {
			case <-s.done:
				return
			case <-time.After(time.Hour):
				continue
			}
		}
		d := time.Until(next)
		if d < 0 {
			d = 0
		}
		logx.Infof("scheduler: next run at %s (in %s)", next.Format(time.RFC3339), d.Round(time.Second))
		select {
		case <-s.done:
			return
		case <-time.After(d):
			s.runScheduledActions()
		}
	}
}

// nextScheduledFire returns the next local time cfg.Schedule.Time will hit.
// Returns ok=false when no schedule flag is enabled or the time is unparseable.
func (s *Server) nextScheduledFire(now time.Time) (time.Time, bool) {
	s.cfgMu.Lock()
	sched := s.cfg.Schedule
	s.cfgMu.Unlock()
	if !schedHasAnyEnabled(sched) {
		return time.Time{}, false
	}
	t, err := parseScheduleTime(sched.Time)
	if err != nil {
		return time.Time{}, false
	}
	year, month, day := now.Date()
	today := time.Date(year, month, day, t.hour, t.minute, 0, 0, now.Location())
	if !today.After(now) {
		today = today.Add(24 * time.Hour)
	}
	return today, true
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
	return sc.SyncGallery || sc.RemoveOrphans || sc.RunAutoTaggers ||
		sc.RecomputeTags || sc.MergeGeneralTags || sc.VacuumDB
}

// runScheduledActions iterates every configured gallery and runs the enabled
// maintenance actions in a fixed order: sync → remove orphans → autotag →
// recompute tags → merge general tags → vacuum. Skips the whole run when a
// user-triggered job is already holding the job manager. The reservation
// blocks user-triggered Start() calls for the duration so the lock-less
// phases below (RemoveOrphans, RecalcTags, MergeGeneral, Vacuum) can't be
// raced by external handlers.
func (s *Server) runScheduledActions() {
	if err := s.jobs.BeginSchedule(); err != nil {
		logx.Warnf("scheduler: skipping run (a job is already running)")
		return
	}
	defer s.jobs.EndSchedule()

	started := time.Now()
	defer func() { s.recordScheduleRun(started, time.Since(started), "OK") }()

	s.cfgMu.Lock()
	sched := s.cfg.Schedule
	s.cfgMu.Unlock()

	s.ctxMu.RLock()
	names := make([]string, 0, len(s.contexts))
	for name := range s.contexts {
		names = append(names, name)
	}
	s.ctxMu.RUnlock()

	for _, name := range names {
		cx := s.Get(name)
		if cx == nil {
			continue
		}
		logx.Infof("scheduler: running actions on gallery %q", name)

		if sched.SyncGallery && !cx.Degraded {
			s.scheduledSync(cx)
		}
		if sched.RemoveOrphans {
			s.scheduledRemoveOrphans(cx)
		}
		if sched.RunAutoTaggers && tagger.IsAvailable(s.cfg) {
			s.scheduledAutotag(cx)
		}
		if sched.RecomputeTags {
			s.scheduledRecalcTags(cx)
		}
		if sched.MergeGeneralTags {
			s.scheduledMergeGeneral(cx)
		}
		if sched.VacuumDB {
			s.scheduledVacuum(cx)
		}
	}
}

func (s *Server) scheduledSync(cx *galleryCtx) {
	if err := s.jobs.StartScheduled("sync"); err != nil {
		logx.Warnf("scheduler sync %q: %v", cx.Name, err)
		return
	}
	ctx := s.jobs.Context()
	result, err := gallery.Sync(ctx, cx.DB, cx.GalleryPath, cx.ThumbnailsPath,
		s.cfg.Gallery.MaxFileSizeMB, s.jobs.Update)
	cx.InvalidateCaches()
	// Match the user-trigger handlers' shape: ctx cancellation produces
	// a clean Complete summary, only real failures fall to Fail().
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] sync cancelled (%d added, %d missing, %d moved)",
			cx.Name, result.Added, result.Removed, result.Moved))
		return
	}
	if err != nil {
		s.jobs.Fail(err.Error())
		logx.Warnf("scheduler sync %q: %v", cx.Name, err)
		return
	}
	s.jobs.Complete(fmt.Sprintf("[%s] %d added, %d missing, %d moved",
		cx.Name, result.Added, result.Removed, result.Moved))
}

func (s *Server) scheduledRemoveOrphans(cx *galleryCtx) {
	entries, err := os.ReadDir(cx.ThumbnailsPath)
	if err != nil {
		logx.Warnf("scheduler orphans %q: read thumbnails dir: %v", cx.Name, err)
		return
	}
	known := map[int64]struct{}{}
	rows, err := cx.DB.Read.Query(`SELECT id FROM images`)
	if err != nil {
		logx.Warnf("scheduler orphans %q: %v", cx.Name, err)
		return
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			known[id] = struct{}{}
		}
	}
	rows.Close()
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var idStr string
		switch {
		case strings.HasSuffix(name, "_hover.webp"):
			idStr = strings.TrimSuffix(name, "_hover.webp")
		case strings.HasSuffix(name, ".jpg"):
			idStr = strings.TrimSuffix(name, ".jpg")
		default:
			continue
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(cx.ThumbnailsPath, name)); err == nil {
			removed++
		}
	}
	logx.Infof("scheduler: [%s] removed %d orphaned thumbnail(s)", cx.Name, removed)
}

func (s *Server) scheduledAutotag(cx *galleryCtx) {
	var ids []int64
	rows, err := cx.DB.Read.Query(
		`SELECT i.id FROM images i WHERE i.is_missing = 0
		 AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.is_auto = 1)`,
	)
	if err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	enabled := tagger.EnabledTaggers(s.cfg)
	if len(enabled) == 0 {
		return
	}
	if err := s.jobs.StartScheduled("autotag"); err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return
	}
	ctx := s.jobs.Context()
	skipped, err := tagger.RunWithTaggers(ctx, cx.DB, s.cfg, ids, enabled, s.jobs, s.cfg.Tagger.UseCUDA)
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] auto-tagging cancelled (%d image(s) queued)", cx.Name, len(ids)))
		return
	}
	if err != nil {
		s.jobs.Fail(err.Error())
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return
	}
	if skipped > 0 {
		s.jobs.Complete(fmt.Sprintf("[%s] auto-tagged %d of %d image(s), %d skipped", cx.Name, len(ids)-skipped, len(ids), skipped))
		return
	}
	s.jobs.Complete(fmt.Sprintf("[%s] auto-tagged %d image(s)", cx.Name, len(ids)))
}

func (s *Server) scheduledRecalcTags(cx *galleryCtx) {
	updated, pruned := cx.TagSvc.RecalcAndPruneCount()
	logx.Infof("scheduler: [%s] recalculated %d tag(s), pruned %d unused", cx.Name, updated, pruned)
}

func (s *Server) scheduledMergeGeneral(cx *galleryCtx) {
	merged, err := cx.TagSvc.MergeGeneralIntoCategorized()
	if err != nil {
		logx.Warnf("scheduler merge-general %q: %v", cx.Name, err)
		return
	}
	logx.Infof("scheduler: [%s] merged %d general tag(s)", cx.Name, merged)
}

func (s *Server) scheduledVacuum(cx *galleryCtx) {
	if _, err := cx.DB.Write.Exec(`VACUUM`); err != nil {
		logx.Warnf("scheduler vacuum %q: %v", cx.Name, err)
		return
	}
	if _, err := cx.DB.Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		logx.Warnf("scheduler vacuum wal_checkpoint %q: %v", cx.Name, err)
	}
	logx.Infof("scheduler: [%s] vacuumed database", cx.Name)
}

// recordScheduleRun stores the completion of a scheduler run so the Schedule
// settings section can show "Last run: … (OK, 3m12s)". info is a short
// status string ("OK" or a failure summary).
func (s *Server) recordScheduleRun(started time.Time, dur time.Duration, info string) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	s.schedLastRun = started
	s.schedLastDur = dur
	s.schedLastInfo = info
}

// ScheduleStatus reports the last recorded scheduler run plus the next fire
// time. Used by the Schedule settings section.
type ScheduleStatus struct {
	LastRun  time.Time     // zero value when no run has happened yet
	LastDur  time.Duration // zero when LastRun is zero
	LastInfo string        // "OK" or a short failure summary; empty when never run
	NextRun  time.Time     // zero when no schedule action is enabled
}

// ScheduleStatus returns the current scheduler status for the settings page.
func (s *Server) ScheduleStatus() ScheduleStatus {
	s.schedMu.Lock()
	st := ScheduleStatus{LastRun: s.schedLastRun, LastDur: s.schedLastDur, LastInfo: s.schedLastInfo}
	s.schedMu.Unlock()
	if next, ok := s.nextScheduledFire(time.Now()); ok {
		st.NextRun = next
	}
	return st
}
