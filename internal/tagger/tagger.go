//go:build tagger

package tagger

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tags"
	ort "github.com/yalue/onnxruntime_go"
)

// IsAvailable reports whether at least one enabled tagger has its files.
func IsAvailable(cfg *config.Config) bool {
	return len(EnabledTaggers(cfg)) > 0
}

// buildSupportsInference is true in the tagger build, false in the noop
// build.
func buildSupportsInference() bool { return true }

// UnavailableReason explains why auto-tagging can't run, mirroring the
// reason shown in Settings → Auto-Tagger. Returns "" when IsAvailable.
func UnavailableReason(cfg *config.Config) string {
	if IsAvailable(cfg) {
		return ""
	}
	taggers := DiscoverTaggers(cfg)
	if len(taggers) == 0 {
		return "no tagger subfolders found under paths.model_path"
	}
	for _, t := range taggers {
		if t.Enabled && !t.Available {
			return t.Reason
		}
	}
	return "no enabled tagger"
}

// CheckProviderAvailable probes whether the ONNX Runtime library can
// initialize the requested execution provider. The settings handler calls
// it before persisting a non-CPU provider so the operator sees a library
// or device issue immediately rather than at tagger-job time.
func CheckProviderAvailable(provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || provider == "cpu" {
		return nil
	}
	if !config.IsValidExecutionProvider(provider) {
		return fmt.Errorf("unsupported execution provider %q", provider)
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	defer func() { _ = ort.DestroyEnvironment() }()

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return fmt.Errorf("ort session options: %w", err)
	}
	defer func() { _ = opts.Destroy() }()

	cleanup, err := appendExecutionProvider(opts, provider, "")
	if cleanup != nil {
		cleanup()
	}
	if err != nil {
		return fmt.Errorf("libonnxruntime does not support %s: %w", provider, err)
	}

	// A CUDA-capable library says nothing about the device: in a container
	// without the GPU passed in, session creation only fails at job time.
	if provider == "cuda" && runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/nvidia0"); err != nil {
			return fmt.Errorf("no NVIDIA GPU device found (pass the GPU into the container, e.g. Podman AddDevice=nvidia.com/gpu=all)")
		}
	}
	return nil
}

// AvailableTaggers returns every known tagger with availability set.
func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	return DiscoverTaggers(cfg)
}

// Status snapshots the registered backend's cache state for the
// operator UI.
func Status() CacheStatus {
	if b := activeBackend(); b != nil {
		return b.Status()
	}
	return CacheStatus{}
}

// ReleaseIdle tears down the cached session set when it has been idle
// for at least `after`.
func ReleaseIdle(after time.Duration) bool {
	if b := activeBackend(); b != nil {
		return b.ReleaseIdle(after)
	}
	return false
}

// ReleaseAll unconditionally tears down the cached session set.
func ReleaseAll() {
	if b := activeBackend(); b != nil {
		b.ReleaseAll()
	}
}

// RunWithTaggers tags ids through the supplied taggers, merging
// results so each image ends up with one row per unique tag. Callers
// must pass only enabled+available taggers. provider overrides
// cfg.Tagger.ExecutionProvider so per-request callers can keep single-image
// runs on the CPU. mangaCacheDir is the per-gallery <data_path>/
// <gallery>/manga directory used to extract and cache cbz pages on
// demand; pass "" to fall back to a per-image temp directory.
// Returns the count of submitted ids left without auto_tagged_at.
func RunWithTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, provider string, mangaCacheDir string) (int, error) {
	if len(taggers) == 0 {
		return 0, fmt.Errorf("no tagger is enabled or available")
	}
	backend := activeBackend()
	if backend == nil {
		return 0, fmt.Errorf("auto-tagger disabled (no backend registered)")
	}

	// Loaded ahead of the backend so a fresh session set picks up the
	// current tag_categories rows when LoadDispatch resolves rule
	// targets. Reused below for the rating / wd14 / inferred chains.
	type catRow struct {
		id   int64
		name string
	}
	catIDs := map[string]int64{}
	cats, err := db.QueryAllContext(ctx, database.Read, func(rows *sql.Rows) (catRow, error) {
		var c catRow
		err := rows.Scan(&c.id, &c.name)
		return c, err
	}, `SELECT id, name FROM tag_categories`)
	if err != nil {
		logx.Warnf("tagger: read tag_categories: %v", err)
	}
	for _, c := range cats {
		catIDs[c.name] = c.id
	}
	generalCatID := catIDs["general"]

	// Inference map for taggers whose category scheme can't tell apart
	// general from categorised counterparts (joytag's single_general,
	// camie when its category is "general"). Maps tag name → catID for
	// an existing non-general non-meta categorised tag. Ambiguous names
	// (multiple categorised variants) are dropped and fall back to
	// general. Lets joytag's `hakurei_reimu` attach to a pre-existing
	// `character:hakurei_reimu` instead of going under general.
	inferredCats := map[string]int64{}
	hasSingleGeneral := false
	for _, t := range taggers {
		profile, perr := ResolveProfile(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		if perr == nil && profile.CategoryScheme == "single_general" {
			hasSingleGeneral = true
			break
		}
	}
	if hasSingleGeneral && generalCatID != 0 {
		// Skip names whose general counterpart already carries a manual
		// image_tag - that's an explicit user choice.
		inferred, err := db.QueryAllContext(ctx, database.Read, func(rows *sql.Rows) (catRow, error) {
			var c catRow
			err := rows.Scan(&c.name, &c.id)
			return c, err
		}, `
			SELECT t.name, t.category_id
			FROM tags t
			JOIN tag_categories tc ON tc.id = t.category_id
			WHERE t.is_alias = 0
			  AND tc.name NOT IN ('general', 'meta')
			  AND NOT EXISTS (
			      SELECT 1 FROM tags g
			      JOIN image_tags it ON it.tag_id = g.id
			      WHERE g.name = t.name
			        AND g.category_id = ?
			        AND g.is_alias = 0
			        AND it.is_auto = 0
			  )`, generalCatID)
		if err == nil {
			ambiguous := map[string]bool{}
			for _, r := range inferred {
				if ambiguous[r.name] {
					continue
				}
				if existing, ok := inferredCats[r.name]; ok && existing != r.id {
					ambiguous[r.name] = true
					delete(inferredCats, r.name)
					continue
				}
				inferredCats[r.name] = r.id
			}
		}
	}

	parallel := min(max(1, cfg.Tagger.Parallel), len(ids))

	// jobs.Manager carries a single status string; parallel workers
	// writing into it would each clobber the others' progress, making
	// the displayed message hop between mangas. Per-worker slots plus
	// a serialising mutex turn every emission into a single combined
	// snapshot - workers see and write the same view of all peers, so
	// the displayed message is always consistent regardless of which
	// goroutine fired the update.
	total := len(ids)
	var completed atomic.Int64
	var statusMu sync.Mutex
	workerStatus := make([]string, parallel)
	// Cap the number of per-worker entries the status bar shows; at
	// parallel=8 with every worker on a long cbz the joined string
	// otherwise overflows the flash slot.
	const maxVisibleWorkers = 3
	emitStatus := func(workerIdx int, msg string) {
		statusMu.Lock()
		defer statusMu.Unlock()
		workerStatus[workerIdx] = msg
		active := slices.DeleteFunc(slices.Clone(workerStatus), func(s string) bool { return s == "" })
		out := "tagging images"
		if len(active) > 0 {
			shown := active
			if len(shown) > maxVisibleWorkers {
				shown = shown[:maxVisibleWorkers]
			}
			out = strings.Join(shown, " · ")
			if extra := len(active) - len(shown); extra > 0 {
				out = fmt.Sprintf("%s (+%d more)", out, extra)
			}
		}
		mgr.Update(int(completed.Load()), total, out)
	}

	taggerNames := make([]string, 0, len(taggers))
	for _, t := range taggers {
		taggerNames = append(taggerNames, t.Name)
	}

	// Build the batch payload: look up each id's canonical path and
	// file type, extract frames (videos, cbz pages), and ship the
	// resolved paths to the backend. Frame cleanup runs after the
	// backend returns this image's slot in the response.
	var skipped atomic.Int64
	requests := make([]BackendImageRequest, 0, len(ids))
	cleanups := make([]func(), 0, len(ids))
	for _, imageID := range ids {
		if ctx.Err() != nil {
			break
		}
		var canonPath, fileType string
		if err := database.Read.QueryRowContext(ctx,
			`SELECT canonical_path, file_type FROM images WHERE id = ?`, imageID,
		).Scan(&canonPath, &fileType); err != nil {
			logx.Warnf("tagger: skip image %d: lookup failed: %v", imageID, err)
			skipped.Add(1)
			continue
		}
		framePaths, cleanup := framesForTagging(canonPath, fileType, mangaCacheDir, imageID)
		if len(framePaths) == 0 {
			logx.Warnf("tagger: skip image %d: no frames available (missing file, archive, or ffmpeg)", imageID)
			skipped.Add(1)
			cleanup()
			continue
		}
		requests = append(requests, BackendImageRequest{
			ID:            imageID,
			FramePaths:    framePaths,
			MangaProgress: fileType == "cbz" && len(framePaths) > 1,
		})
		cleanups = append(cleanups, cleanup)
	}
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	resp, err := backend.Run(ctx, RunRequest{
		Cfg:            cfg,
		Taggers:        taggers,
		Provider:       provider,
		CatIDs:         catIDs,
		GeneralCatID:   generalCatID,
		InferredCats:   inferredCats,
		MinHitFraction: cfg.Tagger.Aggregation.MinHitFraction,
		Parallel:       parallel,
		Images:         requests,
		OnProgress: func(workerIdx int, msg string) {
			// The backend's per-image-done convention is OnProgress
			// with an empty msg; non-empty msg is per-page cbz
			// status. Use the empty-msg event to drive the live
			// counter so the flash shows N/total during the run
			// instead of jumping from 0 to total at completion.
			if msg == "" {
				completed.Add(1)
			}
			emitStatus(workerIdx, msg)
		},
	})
	if err != nil {
		return int(skipped.Load()), err
	}

	for _, r := range resp.Results {
		if r.Err != "" {
			skipped.Add(1)
			continue
		}
		if r.Tags == nil {
			// Cancelled mid-image - skip writing partial state.
			continue
		}
		if storeErr := storeResults(ctx, database, r.ID, r.Tags, taggerNames, catIDs["rating"]); storeErr != nil {
			logx.Warnf("tagger: store results for image %d: %v", r.ID, storeErr)
			skipped.Add(1)
		}
	}

	// Final status update so the progress bar reaches total when the
	// last image is the cancelled / skipped tail.
	mgr.Update(int(completed.Load()), total, "tagging images")
	return int(skipped.Load()), ctx.Err()
}

// framesForTagging returns the file paths to feed the tagger plus a
// cleanup func. Branches by file type:
//   - static images: [canonPath], no-op cleanup.
//   - videos: up to five frames sampled via ffmpeg, removed by cleanup.
//   - cbz manga: every page extracted into the per-gallery manga cache
//     (or a temp directory when mangaCacheDir is empty); the cache
//     entries are deliberately left on disk so idle reclaim handles
//     eviction five minutes after the last use, mirroring the
//     reader's serve path.
//
// With ffmpeg missing or failing, videos yield no frames and the
// caller skips the asset; an unreadable archive does the same.
func framesForTagging(canonPath, fileType, mangaCacheDir string, imageID int64) ([]string, func()) {
	switch fileType {
	case "mp4", "webm":
		positions := []float64{0.10, 0.30, 0.50, 0.70, 0.90}
		frames, _ := gallery.ExtractVideoFrames(canonPath, os.TempDir(), positions)
		cleanup := func() {
			for _, p := range frames {
				_ = os.Remove(p)
			}
		}
		return frames, cleanup
	case "cbz":
		archive, err := gallery.OpenManga(canonPath)
		if err != nil {
			logx.Warnf("tagger: open manga %q: %v", canonPath, err)
			return nil, func() {}
		}
		pageCount := len(archive.Pages)
		_ = archive.Close()
		cacheRoot := mangaCacheDir
		var tempDir string
		if cacheRoot == "" {
			tempDir, err = os.MkdirTemp("", "manga-frames-*")
			if err != nil {
				logx.Warnf("tagger: temp dir for manga frames: %v", err)
				return nil, func() {}
			}
			cacheRoot = tempDir
		}
		paths := make([]string, 0, pageCount)
		for i := 1; i <= pageCount; i++ {
			path, err := gallery.EnsureMangaPageInCache(cacheRoot, canonPath, imageID, i)
			if err != nil {
				logx.Warnf("tagger: extract page %d of %q: %v", i, canonPath, err)
				continue
			}
			paths = append(paths, path)
		}
		cleanup := func() {
			if tempDir != "" {
				_ = os.RemoveAll(tempDir)
			}
		}
		return paths, cleanup
	}
	return []string{canonPath}, func() {}
}

// storeResults commits the merged auto-tag set for one image and keeps
// usage_count in sync. The replace step is scoped to taggerNames so
// other taggers' rows survive. ratingCatID gates the highest-rank-wins
// rating prune that fires when any of merged's tags is a rating-category
// row; pass 0 to skip (pre-bootstrap DB).
func storeResults(
	ctx context.Context, database *db.DB,
	imageID int64, merged map[TagKey]Scored, taggerNames []string, ratingCatID int64,
) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Resolve each desired tag to a tag_id, creating new rows as
	// needed. Alias rows redirect to their canonical so we never
	// attach an alias to an image (matches GetOrCreateTag). Two labels
	// that collapse onto the same canonical keep the higher score.
	type target struct {
		score      float32
		taggerName string
	}
	targets := make(map[int64]target, len(merged))
	for k, s := range merged {
		var tagID int64
		var isAlias int
		var canonicalID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT id, is_alias, canonical_tag_id FROM tags WHERE name = ? AND category_id = ?`, k.Name, k.CatID,
		).Scan(&tagID, &isAlias, &canonicalID)
		if err == sql.ErrNoRows {
			res, err2 := tx.ExecContext(ctx,
				`INSERT INTO tags (name, category_id, usage_count, origin) VALUES (?, ?, 0, ?)`, k.Name, k.CatID, s.TaggerName)
			if err2 != nil {
				return fmt.Errorf("insert tag %q (cat=%d): %w", k.Name, k.CatID, err2)
			}
			tagID, _ = res.LastInsertId()
		} else if err != nil {
			return fmt.Errorf("lookup tag %q (cat=%d): %w", k.Name, k.CatID, err)
		} else if isAlias == 1 && canonicalID.Valid {
			tagID = canonicalID.Int64
		}
		if prev, ok := targets[tagID]; !ok || s.Score > prev.score {
			targets[tagID] = target{score: s.Score, taggerName: s.TaggerName}
		}
	}

	type rowInfo struct {
		isAuto     bool
		taggerName string
	}
	current := map[int64]rowInfo{}
	type tagRow struct {
		id int64
		rowInfo
	}
	existing, err := db.QueryAllContext(ctx, tx, func(rows *sql.Rows) (tagRow, error) {
		var r tagRow
		var isAuto int
		var tname sql.NullString
		err := rows.Scan(&r.id, &isAuto, &tname)
		r.isAuto, r.taggerName = isAuto == 1, tname.String
		return r, err
	}, `SELECT tag_id, is_auto, tagger_name FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}
	for _, r := range existing {
		current[r.id] = r.rowInfo
	}

	toRemove := map[int64]struct{}{}
	if len(taggerNames) > 0 {
		scope := make(map[string]struct{}, len(taggerNames))
		for _, n := range taggerNames {
			scope[n] = struct{}{}
		}
		for tid, info := range current {
			if !info.isAuto {
				continue
			}
			if _, ok := scope[info.taggerName]; !ok {
				continue
			}
			if _, keep := targets[tid]; keep {
				continue
			}
			toRemove[tid] = struct{}{}
		}
	}
	toAdd := map[int64]target{}
	for tid, t := range targets {
		if _, exists := current[tid]; !exists {
			toAdd[tid] = t
		}
	}

	for tid := range toRemove {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ? AND is_auto = 1`, imageID, tid); err != nil {
			return fmt.Errorf("remove auto tag %d: %w", tid, err)
		}
		if err := tags.DropTagUsageTx(tx, tid, imageID); err != nil {
			return fmt.Errorf("decrement usage for tag %d: %w", tid, err)
		}
	}

	for tid, t := range targets {
		info, exists := current[tid]
		if !exists || !info.isAuto {
			continue
		}
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_tags SET confidence = ?, tagger_name = ? WHERE image_id = ? AND tag_id = ? AND is_auto = 1`,
			t.score, tname, imageID, tid); err != nil {
			return fmt.Errorf("refresh attribution for tag %d: %w", tid, err)
		}
	}

	// Every emitted tag records the tagger in the source ledger - the
	// fresh inserts below and the tags already on the image alike, since
	// re-confirming an existing row is what the ledger captures.
	for tid, t := range targets {
		if err := tags.RecordTagSourceTx(tx, imageID, tid, t.taggerName); err != nil {
			return fmt.Errorf("record tag source %d: %w", tid, err)
		}
	}

	for tid, t := range toAdd {
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, 1, 0, ?, ?)`,
			imageID, tid, t.score, tname)
		if err != nil {
			return fmt.Errorf("insert auto tag %d: %w", tid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		// Through the tags helpers so the is_missing guard holds: a
		// missing image is not in usage_count, and auto-tagging one
		// must not inflate it.
		if err := tags.BumpTagUsageTx(tx, tid, imageID); err != nil {
			return fmt.Errorf("increment usage for tag %d: %w", tid, err)
		}
		if err := tags.ApplyImpliedFanoutTx(tx, imageID, tid, ratingCatID, true); err != nil {
			return fmt.Errorf("fan out implications for tag %d: %w", tid, err)
		}
	}

	// WD14 emits every rating label that beats its threshold, so a
	// single image can pick up `sensitive` and `questionable` in one
	// pass. Sweep lower-rank rating rows so highest-rank wins matches
	// what search resolves to anyway.
	if ratingCatID != 0 {
		hasRating := false
		for k := range merged {
			if k.CatID == ratingCatID {
				hasRating = true
				break
			}
		}
		if hasRating {
			if err := tags.PruneLowerRatingsTx(tx, ratingCatID, imageID); err != nil {
				return fmt.Errorf("prune lower ratings: %w", err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE images SET auto_tagged_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), imageID); err != nil {
		return fmt.Errorf("stamp auto_tagged_at on image %d: %w", imageID, err)
	}

	return tx.Commit()
}
