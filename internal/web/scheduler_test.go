package web

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/gallery"
)

// --- nextScheduledFire -----------------------------------------------------

func TestNextScheduledFire_TodayStillAhead(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time:        "23:59",
		SyncGallery: true,
	}
	srv.cfgMu.Unlock()

	// "Now" at 08:00 → today 23:59 is still in the future.
	now := time.Date(2026, 4, 19, 8, 0, 0, 0, time.Local)
	got, ok := srv.nextScheduledFire(now)
	if !ok {
		t.Fatal("expected ok=true with SyncGallery enabled")
	}
	want := time.Date(2026, 4, 19, 23, 59, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
}

func TestNextScheduledFire_TodayPassedRollsToTomorrow(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", VacuumDB: true}
	srv.cfgMu.Unlock()

	// "Now" at 10:00 → today's 01:00 is behind; should roll to tomorrow 01:00.
	now := time.Date(2026, 4, 19, 10, 0, 0, 0, time.Local)
	got, ok := srv.nextScheduledFire(now)
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := time.Date(2026, 4, 20, 1, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
}

func TestNextScheduledFire_NothingEnabledReturnsFalse(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00"} // all flags false
	srv.cfgMu.Unlock()
	if _, ok := srv.nextScheduledFire(time.Now()); ok {
		t.Error("expected ok=false when no schedule flag is set")
	}
}

func TestNextScheduledFire_InvalidTimeReturnsFalse(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "not-a-time", SyncGallery: true}
	srv.cfgMu.Unlock()
	if _, ok := srv.nextScheduledFire(time.Now()); ok {
		t.Error("expected ok=false for unparseable schedule.time")
	}
}

// --- parseScheduleTime -----------------------------------------------------

func TestParseScheduleTime_Table(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in          string
		wantHour    int
		wantMinute  int
		wantErr     bool
		description string
	}{
		{"00:00", 0, 0, false, "midnight"},
		{"23:59", 23, 59, false, "last minute of day"},
		{"01:30", 1, 30, false, "mid-morning"},
		{"24:00", 0, 0, true, "hour out of range"},
		{"12:60", 0, 0, true, "minute out of range"},
		{"noon", 0, 0, true, "not a time"},
		{"12", 0, 0, true, "missing colon"},
		{"", 0, 0, true, "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			got, err := parseScheduleTime(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("%q: expected error", tc.in)
				}
				return
			}
			if err != nil {
				t.Errorf("%q: unexpected error %v", tc.in, err)
			}
			if got.hour != tc.wantHour || got.minute != tc.wantMinute {
				t.Errorf("%q: got {%d,%d}, want {%d,%d}", tc.in, got.hour, got.minute, tc.wantHour, tc.wantMinute)
			}
		})
	}
}

// --- scheduledRemoveOrphans ------------------------------------------------

// seedImage inserts a minimal image row through gallery.Ingest so the Go
// ingestion pipeline (including thumbnail generation) runs.
func seedImage(t *testing.T, srv *Server, name string, w, h int) int64 {
	t.Helper()
	cx := srv.Active()
	if cx == nil {
		t.Fatal("no active gallery")
	}
	path := filepath.Join(cx.GalleryPath, name)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	rec, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, path, "png", "")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return rec.ID
}

func TestScheduledRemoveOrphans_RemovesStrayFiles(t *testing.T) {
	srv := newTestServer(t)
	// Seed one real image so its thumbnail is kept.
	id := seedImage(t, srv, "keeper.png", 10, 10)

	cx := srv.Active()
	// Drop orphans: both patterns the sweep checks.
	orphanJpg := filepath.Join(cx.ThumbnailsPath, "999999.jpg")
	orphanHover := filepath.Join(cx.ThumbnailsPath, "999999_hover.webp")
	for _, p := range []string{orphanJpg, orphanHover} {
		if err := os.WriteFile(p, []byte("orphan"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated file that doesn't match either pattern - must stay.
	bystander := filepath.Join(cx.ThumbnailsPath, "note.txt")
	if err := os.WriteFile(bystander, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv.scheduledRemoveOrphans(cx)

	if _, err := os.Stat(orphanJpg); !os.IsNotExist(err) {
		t.Error("orphan .jpg should have been removed")
	}
	if _, err := os.Stat(orphanHover); !os.IsNotExist(err) {
		t.Error("orphan _hover.webp should have been removed")
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Error("bystander file (not matching the pattern) should be kept")
	}
	// Real thumbnail is still on disk.
	thumb := filepath.Join(cx.ThumbnailsPath, gallery.ThumbnailPath(cx.ThumbnailsPath, id)[len(cx.ThumbnailsPath)+1:])
	if _, err := os.Stat(thumb); err != nil {
		t.Errorf("real thumbnail should survive sweep: %v", err)
	}
}

// --- scheduledRecalcTags ---------------------------------------------------

func TestScheduledRecalcTags_FixesBadCounts(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "recalc.png", 12, 12)

	cx := srv.Active()
	var catID int64
	cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&catID)
	tag, err := cx.TagSvc.GetOrCreateTag("recalc_tag", catID)
	if err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.AddTagToImage(id, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	// Corrupt the usage_count so recalc has real work.
	if _, err := cx.DB.Write.Exec(`UPDATE tags SET usage_count = 99 WHERE id = ?`, tag.ID); err != nil {
		t.Fatal(err)
	}

	srv.scheduledRecalcTags(cx)

	got, err := cx.TagSvc.GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageCount != 1 {
		t.Errorf("recalc expected to fix usage_count=99 → 1, got %d", got.UsageCount)
	}
}

// --- scheduledMergeGeneral -------------------------------------------------

func TestScheduledMergeGeneral_MergesAutoGeneralIntoCategorized(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "merge.png", 10, 10)

	cx := srv.Active()
	var generalID, characterID int64
	cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
	cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='character'`).Scan(&characterID)
	gen, _ := cx.TagSvc.GetOrCreateTag("auto_only_name", generalID)
	cx.TagSvc.GetOrCreateTag("auto_only_name", characterID) // unique counterpart
	conf := 0.7
	if err := cx.TagSvc.AddTagToImage(id, gen.ID, true, &conf); err != nil {
		t.Fatal(err)
	}

	srv.scheduledMergeGeneral(cx)

	got, err := cx.TagSvc.GetTag(gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsAlias {
		t.Error("scheduler merge-general should have aliased the general tag into its character counterpart")
	}
}

// --- scheduledVacuum -------------------------------------------------------

func TestScheduledVacuum_Runs(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "vac.png", 8, 8)
	cx := srv.Active()

	// Just confirm it runs without error on a live DB. Reclaimed-bytes
	// accounting is covered by the `vacuum-db` handler's Playwright spec.
	srv.scheduledVacuum(cx)

	// A subsequent query must still succeed (DB file intact).
	var count int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&count); err != nil {
		t.Fatalf("DB unusable after VACUUM: %v", err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1", count)
	}
}

// --- scheduledSync ---------------------------------------------------------

func TestScheduledSync_IngestsNewFiles(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()

	// Drop a file directly on disk - no API / handler layer involved. The
	// scheduler's sync phase should walk the dir and ingest it.
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	path := filepath.Join(cx.GalleryPath, "dropped.png")
	f, _ := os.Create(path)
	png.Encode(f, img)
	f.Close()

	srv.scheduledSync(cx)

	var count int
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE canonical_path = ?`, path).Scan(&count)
	if count != 1 {
		t.Errorf("scheduled sync did not ingest dropped file, count = %d", count)
	}
}

// --- ScheduleStatus --------------------------------------------------------

// TestScheduleStatus_RecordsLastRun pins that runScheduledActions populates
// schedLastRun so the Schedule settings section can show "Last run: …".
func TestScheduleStatus_RecordsLastRun(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{Time: "01:00", RecomputeTags: true}
	srv.cfgMu.Unlock()

	if got := srv.ScheduleStatus(); !got.LastRun.IsZero() {
		t.Fatalf("fresh server: LastRun should be zero, got %v", got.LastRun)
	}
	srv.runScheduledActions()
	got := srv.ScheduleStatus()
	if got.LastRun.IsZero() {
		t.Error("after runScheduledActions: LastRun should be populated")
	}
	if got.LastInfo != "OK" {
		t.Errorf("LastInfo = %q, want OK", got.LastInfo)
	}
}

// --- runScheduledActions dispatch order ------------------------------------

// TestRunScheduledActions_SkipsWhenJobRunning pins the guard in
// scheduler.go:99 that refuses to fire when a user-triggered job is already
// holding the job manager. Without this, a manual sync could be racing a
// scheduled run.
func TestRunScheduledActions_SkipsWhenJobRunning(t *testing.T) {
	srv := newTestServer(t)
	// Turn every schedule flag on so the actions would run if reached.
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time:             "01:00",
		SyncGallery:      true,
		RemoveOrphans:    true,
		RecomputeTags:    true,
		MergeGeneralTags: true,
		VacuumDB:         true,
	}
	srv.cfgMu.Unlock()

	// Start a user job to poison the BeginSchedule call.
	if err := srv.jobs.Start("sync"); err != nil {
		t.Fatal(err)
	}
	defer srv.jobs.Complete("test")

	// Drop an orphan thumbnail that scheduledRemoveOrphans *would* remove
	// if it fired. After the call, the file must still be there - proof the
	// schedule short-circuited.
	cx := srv.Active()
	orphan := filepath.Join(cx.ThumbnailsPath, "77777.jpg")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv.runScheduledActions()

	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("orphan should still exist when scheduler declines: %v", err)
	}
}

// TestRunScheduledActions_ExecutesEnabledPhases seeds one of every
// maintenance condition, fires the scheduler, and asserts the expected
// side-effects landed.
func TestRunScheduledActions_ExecutesEnabledPhases(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Schedule = config.ScheduleConfig{
		Time:             "01:00",
		SyncGallery:      true,
		RemoveOrphans:    true,
		RunAutoTaggers:   true, // no-op in non-tagger builds (noop.IsAvailable returns false)
		RecomputeTags:    true,
		MergeGeneralTags: true,
		VacuumDB:         true,
	}
	srv.cfgMu.Unlock()

	cx := srv.Active()

	// Phase 1 (sync): drop a file on disk and expect it ingested.
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	syncPath := filepath.Join(cx.GalleryPath, "phase_sync.png")
	f, _ := os.Create(syncPath)
	png.Encode(f, img)
	f.Close()

	// Phase 2 (orphan sweep): seed one unreferenced thumb.
	orphan := filepath.Join(cx.ThumbnailsPath, "424242.jpg")
	os.MkdirAll(cx.ThumbnailsPath, 0o755)
	if err := os.WriteFile(orphan, []byte("zz"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv.runScheduledActions()

	// Phase 1 should have ingested the file.
	var count int
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE canonical_path = ?`, syncPath).Scan(&count)
	if count != 1 {
		t.Error("scheduler's sync phase didn't ingest phase_sync.png")
	}
	// Phase 2 should have removed the orphan.
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("scheduler's orphan sweep didn't remove the orphan thumbnail")
	}
}
