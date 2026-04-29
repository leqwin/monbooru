//go:build tagger

package tagger

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/image/draw"
)

const modelSize = 448

// wd14Category maps WD14 numeric category IDs to Monbooru built-in category names.
var wd14Category = map[int]string{
	0: "general",
	1: "artist",
	4: "character",
	9: "copyright",
}

// wd14RatingTags are WD14 rating labels that should always go in the "meta" category.
var wd14RatingTags = map[string]bool{
	"general": true, "sensitive": true, "questionable": true, "explicit": true,
	"rating:general": true, "rating:sensitive": true, "rating:questionable": true, "rating:explicit": true,
	"rating:safe": true, "rating:nsfw": true,
}

// tagLabel holds a parsed row from selected_tags.csv.
type tagLabel struct {
	name       string
	categoryID int // WD14 category ID
	// placeholder is true when the label-file row had no usable name
	// (e.g. only punctuation) and the slot was filled with an
	// `_unsupported_<idx>` stub to keep the slice index aligned with
	// the model's output channels. Inference must skip these slots so
	// the stub never becomes a real tag.
	placeholder bool
}

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

// CheckCUDAAvailable probes the ONNX Runtime for CUDA support and
// verifies an NVIDIA GPU device file exists. The settings handler calls
// it before persisting use_cuda=true so the user gets an immediate
// error rather than a surprise at tagger-job time.
func CheckCUDAAvailable() error {
	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	defer ort.DestroyEnvironment()

	opts, err := ort.NewCUDAProviderOptions()
	if err != nil {
		return fmt.Errorf("libonnxruntime is not CUDA-capable (use the -cuda Docker image): %w", err)
	}
	opts.Destroy()

	if _, err := os.Stat("/dev/nvidia0"); err != nil {
		return fmt.Errorf("no NVIDIA GPU device found (pass the GPU into the container, e.g. Podman AddDevice=nvidia.com/gpu=all)")
	}
	return nil
}

// AvailableTaggers returns every known tagger with availability set.
func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	return DiscoverTaggers(cfg)
}

// tagKey identifies one (name, category_id) pair so multi-tagger merges
// never insert the same tag twice on the same image.
type tagKey struct {
	name  string
	catID int64
}

// scored carries the highest confidence seen across taggers for one
// tagKey plus the tagger that produced that score, so attribution
// survives multi-tagger merges.
type scored struct {
	score      float32
	taggerName string
}

// loadedTagger pairs a cached ORT session with the per-call config the
// inference loop reads, so threshold edits take effect without
// rebuilding the session.
type loadedTagger struct {
	cfg          config.TaggerInstance
	session      *ort.DynamicAdvancedSession
	labels       []tagLabel
	joytagLayout bool
}

// loadedSession is the cached half of loadedTagger: ORT state keyed by
// tagger name. modelFile and tagsFile gate cache reuse - a TOML edit
// that swaps either invalidates the entry.
type loadedSession struct {
	modelFile    string
	tagsFile     string
	session      *ort.DynamicAdvancedSession
	labels       []tagLabel
	joytagLayout bool
}

// taggerCache holds the warm ORT environment and per-tagger sessions
// across RunWithTaggers calls. Without it the bytes ORT frees on
// teardown stay parked in glibc's arenas; teardown calls mallocTrim to
// hand them back. inUse blocks the idle reaper from racing a run.
type taggerCache struct {
	mu          sync.Mutex
	inUse       bool
	initialized bool
	useCUDA     bool
	sessionOpts *ort.SessionOptions
	cudaOpts    *ort.CUDAProviderOptions
	sessions    map[string]*loadedSession
	lastUsed    time.Time
}

var cache taggerCache

// satisfies returns true when the cached set covers every requested
// tagger with the same execution-provider mode and the same model /
// tags filenames. Caller must hold c.mu.
func (c *taggerCache) satisfies(taggers []TaggerStatus, useCUDA bool) bool {
	if !c.initialized || c.useCUDA != useCUDA {
		return false
	}
	for _, t := range taggers {
		s, ok := c.sessions[t.Name]
		if !ok {
			return false
		}
		if s.modelFile != t.ModelFile || s.tagsFile != t.TagsFile {
			return false
		}
	}
	return true
}

// ensure populates the cache for (taggers, useCUDA). On signature
// mismatch the existing cache is torn down first. Caller must hold
// c.mu.
func (c *taggerCache) ensure(cfg *config.Config, taggers []TaggerStatus, useCUDA bool) error {
	if c.satisfies(taggers, useCUDA) {
		return nil
	}
	if c.initialized {
		c.teardownLocked()
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	c.initialized = true

	if useCUDA {
		opts, err := ort.NewSessionOptions()
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("ort session options: %w", err)
		}
		c.sessionOpts = opts
		cudaOpts, err := ort.NewCUDAProviderOptions()
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("ort cuda options (ensure libonnxruntime was built with CUDA): %w", err)
		}
		c.cudaOpts = cudaOpts
		if err := opts.AppendExecutionProviderCUDA(cudaOpts); err != nil {
			c.teardownLocked()
			return fmt.Errorf("append cuda provider: %w", err)
		}
	}
	c.useCUDA = useCUDA

	c.sessions = make(map[string]*loadedSession, len(taggers))
	for _, t := range taggers {
		onnxPath := filepath.Join(cfg.Paths.ModelPath, t.Name, t.ModelFile)
		tagsPath := filepath.Join(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		labels, err := loadLabels(tagsPath)
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("load labels for %q: %w", t.Name, err)
		}
		inputs, outputs, err := ort.GetInputOutputInfo(onnxPath)
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("inspect ort model for %q: %w", t.Name, err)
		}
		if len(inputs) == 0 || len(outputs) == 0 {
			c.teardownLocked()
			return fmt.Errorf("ort model for %q has no input/output", t.Name)
		}
		session, err := ort.NewDynamicAdvancedSession(onnxPath,
			[]string{inputs[0].Name}, []string{outputs[0].Name}, c.sessionOpts)
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("create ort session for %q: %w", t.Name, err)
		}
		c.sessions[t.Name] = &loadedSession{
			modelFile:    t.ModelFile,
			tagsFile:     t.TagsFile,
			session:      session,
			labels:       labels,
			joytagLayout: !strings.EqualFold(filepath.Ext(t.TagsFile), ".csv"),
		}
	}
	return nil
}

// teardownLocked destroys every cached ORT object and asks glibc to
// return the freed bytes to the kernel. Caller must hold c.mu.
func (c *taggerCache) teardownLocked() {
	for _, s := range c.sessions {
		s.session.Destroy()
	}
	c.sessions = nil
	if c.cudaOpts != nil {
		c.cudaOpts.Destroy()
		c.cudaOpts = nil
	}
	if c.sessionOpts != nil {
		c.sessionOpts.Destroy()
		c.sessionOpts = nil
	}
	if c.initialized {
		ort.DestroyEnvironment()
		c.initialized = false
	}
	c.useCUDA = false
	mallocTrim()
}

// ReleaseIdle tears down the cached session set when it has been idle
// for at least `after` and no run is in flight. Returns true on
// teardown so the caller can log it.
func ReleaseIdle(after time.Duration) bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.inUse || !cache.initialized {
		return false
	}
	if time.Since(cache.lastUsed) < after {
		return false
	}
	cache.teardownLocked()
	return true
}

// ReleaseAll unconditionally tears down the cached session set, e.g.
// on shutdown or when use_cuda flips and the cache must be rebuilt.
func ReleaseAll() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.initialized {
		cache.teardownLocked()
	}
}

// RunWithTaggers tags ids through the supplied taggers, merging results
// so each image ends up with one row per unique tag. Callers must pass
// only enabled+available taggers. useCUDA overrides cfg.Tagger.UseCUDA
// so per-request callers can keep single-image runs on the CPU.
// Returns the count of submitted ids left without auto_tagged_at.
func RunWithTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, useCUDA bool) (int, error) {
	if len(taggers) == 0 {
		return 0, fmt.Errorf("no tagger is enabled or available")
	}

	cache.mu.Lock()
	if err := cache.ensure(cfg, taggers, useCUDA); err != nil {
		cache.mu.Unlock()
		return 0, err
	}
	loaded := make([]loadedTagger, len(taggers))
	for i, t := range taggers {
		s := cache.sessions[t.Name]
		loaded[i] = loadedTagger{
			cfg:          t.TaggerInstance,
			session:      s.session,
			labels:       s.labels,
			joytagLayout: s.joytagLayout,
		}
	}
	cache.inUse = true
	cache.mu.Unlock()

	defer func() {
		cache.mu.Lock()
		cache.inUse = false
		cache.lastUsed = time.Now()
		// idle_release_after_minutes <= 0 disables caching: tear
		// down right after the run so RSS drops back to baseline.
		if cfg.Tagger.IdleReleaseAfterMinutes <= 0 {
			cache.teardownLocked()
		}
		cache.mu.Unlock()
	}()

	// Names of the taggers running this job; used so the replace step
	// only wipes rows produced by these taggers.
	taggerNames := make([]string, 0, len(loaded))
	for _, lt := range loaded {
		taggerNames = append(taggerNames, lt.cfg.Name)
	}

	catIDs := map[string]int64{}
	catRows, err := database.Read.QueryContext(ctx, `SELECT id, name FROM tag_categories`)
	if err == nil {
		for catRows.Next() {
			var id int64
			var name string
			if scanErr := catRows.Scan(&id, &name); scanErr != nil {
				logx.Warnf("tagger: scan tag_categories: %v", scanErr)
				continue
			}
			catIDs[name] = id
		}
		catRows.Close()
	}
	metaCatID := catIDs["meta"]
	generalCatID := catIDs["general"]

	// Inference map for label-only (.txt) taggers: tag name → catID
	// for an existing non-general non-meta categorised tag. Ambiguous
	// names (multiple categorised variants) are dropped and fall back
	// to general. Lets joytag's `hakurei_reimu` attach to a pre-existing
	// `character:hakurei_reimu` instead of going under general.
	inferredCats := map[string]int64{}
	hasTxt := false
	for _, lt := range loaded {
		if lt.joytagLayout {
			hasTxt = true
			break
		}
	}
	if hasTxt && generalCatID != 0 {
		// Skip names whose general counterpart already carries a manual
		// image_tag - that's an explicit user choice.
		infRows, err := database.Read.QueryContext(ctx, `
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
			for infRows.Next() {
				var n string
				var cid int64
				if err := infRows.Scan(&n, &cid); err != nil {
					continue
				}
				if ambiguous[n] {
					continue
				}
				if existing, ok := inferredCats[n]; ok && existing != cid {
					ambiguous[n] = true
					delete(inferredCats, n)
					continue
				}
				inferredCats[n] = cid
			}
			infRows.Close()
		}
	}

	// processOne runs the per-image tagging pipeline. Called from one
	// or more worker goroutines; ORT sessions are safe for concurrent
	// Run calls and the DB write pool serialises storeResults.
	var skipped atomic.Int64
	processOne := func(imageID int64) {
		var canonPath, fileType string
		if err := database.Read.QueryRowContext(ctx,
			`SELECT canonical_path, file_type FROM images WHERE id = ?`, imageID,
		).Scan(&canonPath, &fileType); err != nil {
			logx.Warnf("tagger: skip image %d: lookup failed: %v", imageID, err)
			skipped.Add(1)
			return
		}

		framePaths, cleanup := framesForTagging(canonPath, fileType)
		defer cleanup()
		if len(framePaths) == 0 {
			logx.Warnf("tagger: skip image %d: no frames available (missing file or ffmpeg)", imageID)
			skipped.Add(1)
			return
		}

		merged := map[tagKey]scored{}
		for _, lt := range loaded {
			// Videos keep the highest score per label across the sampled frames.
			best := map[int]float32{}
			for _, fp := range framePaths {
				scores, err := inferImage(lt, fp)
				if err != nil {
					continue
				}
				for idx, score := range scores {
					if score > best[idx] {
						best[idx] = score
					}
				}
			}
			threshold := float32(lt.cfg.ConfidenceThreshold)
			for idx, score := range best {
				if idx >= len(lt.labels) {
					continue
				}
				label := lt.labels[idx]
				if label.placeholder {
					continue
				}
				if score < threshold || score < 0.001 {
					continue
				}
				var catID int64
				if wd14RatingTags[label.name] {
					catID = metaCatID
				} else {
					monbooruCat := wd14Category[label.categoryID]
					if monbooruCat == "" {
						monbooruCat = "general"
					}
					catID = catIDs[monbooruCat]
				}
				// .txt taggers have no category info; if a unique
				// categorised tag with this name already exists, attach
				// to it instead of dropping into general.
				if lt.joytagLayout && catID == generalCatID {
					if inferred, ok := inferredCats[label.name]; ok {
						catID = inferred
					}
				}
				k := tagKey{name: label.name, catID: catID}
				if prev, ok := merged[k]; !ok || score > prev.score {
					merged[k] = scored{score: score, taggerName: lt.cfg.Name}
				}
			}
		}

		if err := storeResults(ctx, database, imageID, merged, taggerNames); err != nil {
			logx.Warnf("tagger: store results for image %d: %v", imageID, err)
			skipped.Add(1)
		}
	}

	parallel := cfg.Tagger.Parallel
	if parallel < 1 {
		parallel = 1
	}
	if parallel > len(ids) {
		parallel = len(ids)
	}

	total := len(ids)
	var completed atomic.Int64
	queue := make(chan int64, parallel)
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for imageID := range queue {
				if ctx.Err() != nil {
					continue
				}
				processOne(imageID)
				done := int(completed.Add(1))
				mgr.Update(done, total, "tagging images")
			}
		}()
	}

	for _, imageID := range ids {
		if ctx.Err() != nil {
			break
		}
		queue <- imageID
	}
	close(queue)
	wg.Wait()

	return int(skipped.Load()), ctx.Err()
}

// framesForTagging returns the file paths to feed the tagger plus a
// cleanup func. Static images return [canonPath]; videos sample up to
// five frames via ffmpeg. With ffmpeg missing or failing, videos
// yield no frames and the caller skips the asset.
func framesForTagging(canonPath, fileType string) ([]string, func()) {
	if fileType != "mp4" && fileType != "webm" {
		return []string{canonPath}, func() {}
	}
	positions := []float64{0.10, 0.30, 0.50, 0.70, 0.90}
	frames, err := gallery.ExtractVideoFrames(canonPath, os.TempDir(), positions)
	cleanup := func() {
		for _, p := range frames {
			os.Remove(p)
		}
	}
	if err != nil {
		return frames, cleanup
	}
	return frames, cleanup
}

// inferImage loads, preprocesses, and runs inference on a single image.
// WD14 wants NHWC f32 BGR 0..255 and emits sigmoid probabilities;
// joytag wants NCHW f32 RGB CLIP-normalised and emits raw logits, so we
// sigmoid the output ourselves.
func inferImage(lt loadedTagger, path string) ([]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}

	processed := padAndResize(img, modelSize)
	tensor, inputShape := buildTensor(processed, lt.joytagLayout)
	inputTensor, err := ort.NewTensor(inputShape, tensor)
	if err != nil {
		return nil, err
	}
	defer inputTensor.Destroy()

	// nil output lets DynamicAdvancedSession allocate it.
	outputs := []ort.Value{nil}
	if err := lt.session.Run([]ort.Value{inputTensor}, outputs); err != nil {
		return nil, err
	}
	defer outputs[0].Destroy()

	outTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output type: %T", outputs[0])
	}
	data := outTensor.GetData()
	if lt.joytagLayout {
		out := make([]float32, len(data))
		for i, v := range data {
			out[i] = float32(1 / (1 + math.Exp(-float64(v))))
		}
		return out, nil
	}
	return data, nil
}

// buildTensor fills the ORT input buffer from the resized RGBA image.
// The two branches diverge on layout, channel order, and value range.
// Reading Pix directly skips the per-pixel image.Image interface
// dispatch that would otherwise keep the GPU idle between inferences.
func buildTensor(img *image.RGBA, joytag bool) ([]float32, ort.Shape) {
	pix := img.Pix
	stride := img.Stride
	if !joytag {
		tensor := make([]float32, 1*modelSize*modelSize*3)
		for y := 0; y < modelSize; y++ {
			row := pix[y*stride:]
			for x := 0; x < modelSize; x++ {
				src := x * 4
				dst := (y*modelSize + x) * 3
				tensor[dst+0] = float32(row[src+2]) // B
				tensor[dst+1] = float32(row[src+1]) // G
				tensor[dst+2] = float32(row[src+0]) // R
			}
		}
		return tensor, ort.NewShape(1, modelSize, modelSize, 3)
	}

	// CLIP mean/std from joytag's preprocess step.
	mean := [3]float32{0.48145466, 0.4578275, 0.40821073}
	std := [3]float32{0.26862954, 0.26130258, 0.27577711}
	plane := modelSize * modelSize
	tensor := make([]float32, 1*3*plane)
	for y := 0; y < modelSize; y++ {
		row := pix[y*stride:]
		for x := 0; x < modelSize; x++ {
			src := x * 4
			off := y*modelSize + x
			tensor[0*plane+off] = (float32(row[src+0])/255 - mean[0]) / std[0]
			tensor[1*plane+off] = (float32(row[src+1])/255 - mean[1]) / std[1]
			tensor[2*plane+off] = (float32(row[src+2])/255 - mean[2]) / std[2]
		}
	}
	return tensor, ort.NewShape(1, 3, modelSize, modelSize)
}

// padAndResize pads src to a white square then resizes to size×size.
// Returns *image.RGBA so the caller can read .Pix directly.
func padAndResize(src image.Image, size int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Max.X-b.Min.X, b.Max.Y-b.Min.Y
	maxDim := w
	if h > maxDim {
		maxDim = h
	}

	square := image.NewRGBA(image.Rect(0, 0, maxDim, maxDim))
	for i := range square.Pix {
		square.Pix[i] = 0xFF
	}
	offX := (maxDim - w) / 2
	offY := (maxDim - h) / 2
	draw.Draw(square, image.Rect(offX, offY, offX+w, offY+h), src, b.Min, draw.Src)

	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), square, square.Bounds(), draw.Src, nil)
	// Force white where alpha is 0 (e.g. transparent PNG corners).
	for i := 3; i < len(dst.Pix); i += 4 {
		if dst.Pix[i] == 0 {
			dst.Pix[i-3] = 0xFF
			dst.Pix[i-2] = 0xFF
			dst.Pix[i-1] = 0xFF
			dst.Pix[i] = 0xFF
		}
	}
	return dst
}

// storeResults commits the merged auto-tag set for one image and keeps
// usage_count in sync. The replace step is scoped to taggerNames so
// other taggers' rows survive.
func storeResults(
	ctx context.Context, database *db.DB,
	imageID int64, merged map[tagKey]scored, taggerNames []string,
) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
			`SELECT id, is_alias, canonical_tag_id FROM tags WHERE name = ? AND category_id = ?`, k.name, k.catID,
		).Scan(&tagID, &isAlias, &canonicalID)
		if err == sql.ErrNoRows {
			res, err2 := tx.ExecContext(ctx,
				`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, k.name, k.catID)
			if err2 != nil {
				return fmt.Errorf("insert tag %q (cat=%d): %w", k.name, k.catID, err2)
			}
			tagID, _ = res.LastInsertId()
		} else if err != nil {
			return fmt.Errorf("lookup tag %q (cat=%d): %w", k.name, k.catID, err)
		} else if isAlias == 1 && canonicalID.Valid {
			tagID = canonicalID.Int64
		}
		if prev, ok := targets[tagID]; !ok || s.score > prev.score {
			targets[tagID] = target{score: s.score, taggerName: s.taggerName}
		}
	}

	// Snapshot tags currently on the image, with attribution.
	type rowInfo struct {
		isAuto     bool
		taggerName string
	}
	current := map[int64]rowInfo{}
	rows, err := tx.QueryContext(ctx,
		`SELECT tag_id, is_auto, tagger_name FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var tid int64
		var isAuto int
		var tname sql.NullString
		if err := rows.Scan(&tid, &isAuto, &tname); err != nil {
			rows.Close()
			return err
		}
		current[tid] = rowInfo{isAuto: isAuto == 1, taggerName: tname.String}
	}
	rows.Close()

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

	// Apply removals. Roll back on failure rather than committing a
	// half-replaced state.
	for tid := range toRemove {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ? AND is_auto = 1`, imageID, tid); err != nil {
			return fmt.Errorf("remove auto tag %d: %w", tid, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, tid); err != nil {
			return fmt.Errorf("decrement usage for tag %d: %w", tid, err)
		}
	}

	// Refresh confidence and attribution for tags that stay.
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

	for tid, t := range toAdd {
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, confidence, tagger_name) VALUES (?, ?, 1, ?, ?)`,
			imageID, tid, t.score, tname)
		if err != nil {
			return fmt.Errorf("insert auto tag %d: %w", tid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tid); err != nil {
			return fmt.Errorf("increment usage for tag %d: %w", tid, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE images SET auto_tagged_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), imageID); err != nil {
		return fmt.Errorf("stamp auto_tagged_at on image %d: %w", imageID, err)
	}

	return tx.Commit()
}

// loadLabels parses the tagger's label file. `.csv` follows the WD14
// schema (id, name, category_id); any other extension is read one
// label per line with every label mapped to WD14 category 0
// (`general`). Names are sanitised for the tag allowlist; the slice
// index always lines up 1:1 with the model's output channels, even for
// placeholder labels.
func loadLabels(path string) ([]tagLabel, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if strings.EqualFold(filepath.Ext(path), ".csv") {
		return loadLabelsCSV(f)
	}
	return loadLabelsText(f)
}

func loadLabelsCSV(f io.Reader) ([]tagLabel, error) {
	r := csv.NewReader(f)
	if _, err := r.Read(); err != nil {
		return nil, err
	}
	var labels []tagLabel
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 3 {
			continue
		}
		catID, _ := strconv.Atoi(strings.TrimSpace(rec[2]))
		name, ok := sanitizeLabel(rec[1], len(labels))
		labels = append(labels, tagLabel{
			name:        name,
			categoryID:  catID,
			placeholder: !ok,
		})
	}
	return labels, nil
}

func loadLabelsText(f io.Reader) ([]tagLabel, error) {
	var labels []tagLabel
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		if strings.TrimSpace(raw) == "" {
			continue
		}
		name, ok := sanitizeLabel(raw, len(labels))
		labels = append(labels, tagLabel{
			name:        name,
			categoryID:  0,
			placeholder: !ok,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return labels, nil
}

// sanitizeLabel coerces a label-file name into the documented tag
// allowlist. Spaces collapse to underscores; out-of-set runes drop.
// The colon is preserved so labels like `:3` and `rating:general` round
// trip unchanged. A label that empties out becomes
// `_unsupported_<idx>` so the slice index keeps its 1:1 mapping with
// the model's output channels - dropping the entry would shift every
// later label and corrupt downstream attribution. The returned bool is
// false in that fallback case so callers can flag the slot as a
// placeholder and skip emission at inference time.
func sanitizeLabel(raw string, idx int) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_' || r == '(' || r == ')' || r == '!' ||
				r == '@' || r == '#' || r == '$' || r == '.' ||
				r == '~' || r == '+' || r == '-' || r == ':' ||
				r == '?' || r == '<' || r == '>' || r == '=' ||
				r == '^':
			b.WriteRune(r)
		case r == ' ' || r == '\t':
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 200 {
		out = out[:200]
	}
	// Match ValidateTagName: emoticon-only labels like "??", ">_<", "^_^"
	// are accepted alongside alphanumeric ones; only pure separator-class
	// punctuation drops to the placeholder slot.
	hasContent := false
	for _, r := range out {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '?' || r == '<' || r == '>' || r == '=' || r == '^' {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return fmt.Sprintf("_unsupported_%d", idx), false
	}
	return out, true
}

// sharedLibPath finds the ONNX Runtime shared library. ORT_LIB_PATH
// overrides; otherwise we try the usual install locations.
func sharedLibPath() string {
	if p := os.Getenv("ORT_LIB_PATH"); p != "" {
		return p
	}
	candidates := []string{
		"/usr/lib/libonnxruntime.so",
		"/usr/local/lib/libonnxruntime.so",
		"libonnxruntime.so",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "libonnxruntime.so"
}
