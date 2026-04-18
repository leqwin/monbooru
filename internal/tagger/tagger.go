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
}

// IsAvailable returns true when at least one enabled tagger has its files.
func IsAvailable(cfg *config.Config) bool {
	return len(EnabledTaggers(cfg)) > 0
}

// CheckCUDAAvailable probes the ONNX Runtime shared library for CUDA support
// and verifies that an NVIDIA GPU device file is present inside the container.
// Called from the settings handler before persisting use_cuda=true so the user
// gets an immediate error instead of a surprise at tagger-job time.
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

// AvailableTaggers returns every known tagger with runtime availability set.
func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	return DiscoverTaggers(cfg)
}

// tagKey uniquely identifies a (name, category_id) pair so multiple taggers
// never insert the same tag twice on the same image.
type tagKey struct {
	name  string
	catID int64
}

// scored records the highest confidence seen across taggers for one tag key,
// along with the tagger that produced that score so attribution survives
// multi-tagger merges.
type scored struct {
	score      float32
	taggerName string
}

// loadedTagger holds one tagger's ORT session and the preprocessing choice
// derived from its tags-file extension. joytagLayout toggles NCHW + RGB +
// CLIP-normalized input and sigmoid output; WD14 stays on NHWC + BGR +
// 0..255 raw with probability output.
type loadedTagger struct {
	cfg          config.TaggerInstance
	session      *ort.DynamicAdvancedSession
	labels       []tagLabel
	joytagLayout bool
}

// RunWithTaggers processes the given image IDs through the supplied taggers
// and merges the results so each image ends up with one row per unique tag.
// Handlers may restrict a job to a single auto-tagger without altering config.
// Only taggers that are both enabled and available on disk should be passed;
// this function does not re-filter the list. useCUDA takes precedence over
// cfg.Tagger.UseCUDA so per-request callers (single-image detail runs) can
// keep inference on the CPU even when the global toggle is on.
func RunWithTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, useCUDA bool) error {
	if len(taggers) == 0 {
		return fmt.Errorf("no tagger is enabled or available")
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	defer ort.DestroyEnvironment()

	// Build session options once; when GPU is enabled we attach the CUDA
	// execution provider so every tagger session runs on the GPU.
	var sessionOpts *ort.SessionOptions
	if useCUDA {
		opts, err := ort.NewSessionOptions()
		if err != nil {
			return fmt.Errorf("ort session options: %w", err)
		}
		defer opts.Destroy()
		cudaOpts, err := ort.NewCUDAProviderOptions()
		if err != nil {
			return fmt.Errorf("ort cuda options (ensure libonnxruntime was built with CUDA): %w", err)
		}
		defer cudaOpts.Destroy()
		if err := opts.AppendExecutionProviderCUDA(cudaOpts); err != nil {
			return fmt.Errorf("append cuda provider: %w", err)
		}
		sessionOpts = opts
	}

	// Open one ORT session per enabled tagger, reused for all images.
	loaded := make([]loadedTagger, 0, len(taggers))
	for _, t := range taggers {
		onnxPath := filepath.Join(cfg.Paths.ModelPath, t.Name, t.ModelFile)
		tagsPath := filepath.Join(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		labels, err := loadLabels(tagsPath)
		if err != nil {
			return fmt.Errorf("load labels for %q: %w", t.Name, err)
		}
		inputs, outputs, err := ort.GetInputOutputInfo(onnxPath)
		if err != nil {
			return fmt.Errorf("inspect ort model for %q: %w", t.Name, err)
		}
		if len(inputs) == 0 || len(outputs) == 0 {
			return fmt.Errorf("ort model for %q has no input/output", t.Name)
		}
		session, err := ort.NewDynamicAdvancedSession(onnxPath,
			[]string{inputs[0].Name}, []string{outputs[0].Name}, sessionOpts)
		if err != nil {
			return fmt.Errorf("create ort session for %q: %w", t.Name, err)
		}
		loaded = append(loaded, loadedTagger{
			cfg:          t.TaggerInstance,
			session:      session,
			labels:       labels,
			joytagLayout: !strings.EqualFold(filepath.Ext(t.TagsFile), ".csv"),
		})
	}
	defer func() {
		for _, l := range loaded {
			l.session.Destroy()
		}
	}()

	// Names of the taggers actually running this job; used to scope the
	// replace step so one tagger never wipes another tagger's output.
	taggerNames := make([]string, 0, len(loaded))
	for _, lt := range loaded {
		taggerNames = append(taggerNames, lt.cfg.Name)
	}

	// Resolve category IDs from DB.
	catIDs := map[string]int64{}
	catRows, err := database.Read.QueryContext(ctx, `SELECT id, name FROM tag_categories`)
	if err == nil {
		for catRows.Next() {
			var id int64
			var name string
			catRows.Scan(&id, &name)
			catIDs[name] = id
		}
		catRows.Close()
	}
	metaCatID := catIDs["meta"]
	generalCatID := catIDs["general"]

	// Inference map for label-only (.txt) taggers: tag name → catID for
	// existing non-general non-meta categorized tags. Names with multiple
	// categorized variants are dropped (ambiguous → fall back to general).
	// A .txt tagger emits no category info, so this lets `hakurei_reimu`
	// from joytag attach to a pre-existing `character:hakurei_reimu`
	// instead of being filed under general.
	inferredCats := map[string]int64{}
	hasTxt := false
	for _, lt := range loaded {
		if lt.joytagLayout {
			hasTxt = true
			break
		}
	}
	if hasTxt && generalCatID != 0 {
		// Exclude names where the user has manually used the general
		// counterpart - that's an explicit signal the user wants the
		// general version, not the inferred categorized one.
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

	// processOne runs the full per-image tagging pipeline. It is called from
	// one or more worker goroutines; ORT sessions are safe for concurrent Run
	// calls (each call allocates its own input/output tensors) and the DB
	// write pool serialises storeResults naturally.
	processOne := func(imageID int64) {
		var canonPath, fileType string
		if err := database.Read.QueryRowContext(ctx,
			`SELECT canonical_path, file_type FROM images WHERE id = ?`, imageID,
		).Scan(&canonPath, &fileType); err != nil {
			return
		}

		framePaths, cleanup := framesForTagging(canonPath, fileType)
		defer cleanup()
		if len(framePaths) == 0 {
			return
		}

		merged := map[tagKey]scored{}
		for _, lt := range loaded {
			// For videos we keep the best score per tag across all sampled frames.
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
				if score < threshold || score < 0.001 {
					continue
				}
				label := lt.labels[idx]
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
				// .txt taggers have no category info; if a unique categorized
				// tag with this name already exists, attach to it instead of
				// the default general bucket.
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

	return ctx.Err()
}

// framesForTagging returns the list of image file paths to feed the tagger
// for one asset, plus a cleanup function that removes any temp frames.
// For static images it returns [canonPath]; for videos it samples up to
// five frames via ffmpeg. When ffmpeg is missing or fails, videos yield
// no frames and the caller should skip the asset.
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
// WD14 models want NHWC float32 BGR in 0-255 and emit sigmoid probabilities;
// joytag models want NCHW float32 RGB normalized with CLIP mean/std and emit
// raw logits, so we sigmoid the output ourselves before thresholding.
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

	// Output is nil; DynamicAdvancedSession will allocate it.
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

// buildTensor fills the ORT input buffer from the resized RGBA image. The two
// branches diverge on layout, channel order, and value range; padAndResize
// always returns *image.RGBA, so we read Pix directly to skip the per-pixel
// image.Image interface dispatch and RGBA() alpha math that would otherwise
// dominate this loop and keep the GPU waiting between inferences.
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

	// CLIP mean/std as used by joytag's preprocess step.
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

// padAndResize pads img to square with white then resizes to size×size.
// Returns *image.RGBA so the caller can read .Pix directly.
func padAndResize(src image.Image, size int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Max.X-b.Min.X, b.Max.Y-b.Min.Y
	maxDim := w
	if h > maxDim {
		maxDim = h
	}

	square := image.NewRGBA(image.Rect(0, 0, maxDim, maxDim))
	// Fill white background.
	for i := range square.Pix {
		square.Pix[i] = 0xFF
	}
	offX := (maxDim - w) / 2
	offY := (maxDim - h) / 2
	draw.Draw(square, image.Rect(offX, offY, offX+w, offY+h), src, b.Min, draw.Src)

	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), square, square.Bounds(), draw.Src, nil)
	// Ensure white where alpha is 0.
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

// storeResults writes the merged auto-tag rows for one image within a
// transaction and keeps usage_count in sync. The replace step only removes
// rows produced by the taggers in taggerNames so other taggers' tags survive.
func storeResults(
	ctx context.Context, database *db.DB,
	imageID int64, merged map[tagKey]scored, taggerNames []string,
) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Resolve each desired tag to a tag_id, creating new rows as needed.
	type target struct {
		score      float32
		taggerName string
	}
	targets := make(map[int64]target, len(merged))
	for k, s := range merged {
		var tagID int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM tags WHERE name = ? AND category_id = ?`, k.name, k.catID,
		).Scan(&tagID)
		if err == sql.ErrNoRows {
			res, err2 := tx.ExecContext(ctx,
				`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, k.name, k.catID)
			if err2 != nil {
				return fmt.Errorf("insert tag %q (cat=%d): %w", k.name, k.catID, err2)
			}
			tagID, _ = res.LastInsertId()
		} else if err != nil {
			return fmt.Errorf("lookup tag %q (cat=%d): %w", k.name, k.catID, err)
		}
		targets[tagID] = target{score: s.score, taggerName: s.taggerName}
	}

	// Snapshot tags currently on the image along with attribution.
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

	// Figure out the diff.
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

	// Apply removals. Failing here would leave an old auto-tag attached
	// to the image with the wrong attribution; roll back instead of
	// committing a partial state.
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

	// Refresh confidence and tagger attribution for tags that stay.
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

	// Insert new tags.
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

// loadLabels parses the tagger's label file. `.csv` files follow the WD14
// schema (id, name, category_id). Any other extension (joytag ships a plain
// `tags.txt` / `top_tags.txt` with one label per line) is read line-by-line
// with every label mapped to WD14 category 0 (→ monbooru `general`). Label
// names are normalised to fit the documented tag allowlist before they
// become tag rows; the slice index still maps 1:1 to the model's output
// channels even when individual labels are placeholders.
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
	if _, err := r.Read(); err != nil { // skip header
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
		labels = append(labels, tagLabel{
			name:       sanitizeLabel(rec[1], len(labels)),
			categoryID: catID,
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
		labels = append(labels, tagLabel{
			name:       sanitizeLabel(raw, len(labels)),
			categoryID: 0,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return labels, nil
}

// sanitizeLabel coerces a label-file name into the documented tag
// allowlist (`[a-z0-9_()!@#$.~+-]`, length 1-200, must contain a
// letter or digit). Spaces collapse to underscores; out-of-set runes
// drop. A label that empties out (or stays all-punctuation) is
// replaced by an `_unsupported_<idx>` placeholder so the slice
// position still aligns with the model's output channel - dropping
// the entry would shift every later label by one and corrupt every
// downstream tag attribution.
func sanitizeLabel(raw string, idx int) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_' || r == '(' || r == ')' || r == '!' ||
				r == '@' || r == '#' || r == '$' || r == '.' ||
				r == '~' || r == '+' || r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '\t':
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 200 {
		out = out[:200]
	}
	hasAlphanum := false
	for _, r := range out {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			hasAlphanum = true
			break
		}
	}
	if !hasAlphanum {
		return fmt.Sprintf("_unsupported_%d", idx)
	}
	return out
}

// sharedLibPath returns the path to the ONNX Runtime shared library.
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
