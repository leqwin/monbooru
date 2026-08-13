package tagger

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
)

// Default filenames for a tagger subfolder. Each can be overridden in
// the TOML entry. When neither default is present, a lone .onnx / .csv
// / .txt in the folder is auto-picked.
const (
	DefaultModelFile    = "model.onnx"
	DefaultTagsFile     = "tags.csv"
	DefaultTextTagsFile = "tags.txt"
	// DefaultConfidenceThreshold is applied to taggers discovered on disk
	// that do not yet have a TOML entry with an explicit threshold.
	DefaultConfidenceThreshold = 0.4
)

// taggerNameRe enforces the same allowlist gallery names use. The name
// becomes the folder under paths.model_path and a TOML key, so a
// permissive value would let the settings handlers persist a row that
// lookups can never resolve.
var taggerNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateTaggerName rejects an empty or non-allowlist name. Mirrors
// config.ValidateGalleryName so the two operator-supplied identifiers
// share the same vocabulary.
func ValidateTaggerName(name string) error {
	if name == "" {
		return fmt.Errorf("tagger name must not be empty")
	}
	if !taggerNameRe.MatchString(name) {
		return fmt.Errorf("tagger name %q must match [A-Za-z0-9_-]+", name)
	}
	return nil
}

// TaggerStatus pairs a configured tagger with its runtime availability
// so the settings UI can show why each row is active or inactive.
type TaggerStatus struct {
	config.TaggerInstance
	Available bool
	Reason    string
}

// DiscoverTaggers merges tagger subfolders under paths.model_path with
// the configured list. The result has an entry for every on-disk folder
// AND every configured tagger (so leftover config is still visible
// after the folder vanishes). Sorted by Name.
func DiscoverTaggers(cfg *config.Config) []TaggerStatus {
	byName := map[string]config.TaggerInstance{}
	order := []string{}

	catalogDefaults := catalogDefaultsByName(cfg.Paths.ModelPath)

	// Start from disk so untouched subfolders show up even without
	// config. TOML overlays override below. Completely empty
	// subdirectories are skipped so they don't appear as permanently
	// broken rows.
	if entries, err := os.ReadDir(cfg.Paths.ModelPath); err == nil {
		for _, e := range entries {
			name := e.Name()
			// Stat rather than trusting the entry's own type: a model
			// directory symlinked in from elsewhere (one large model
			// shared across installs) reports as a link, not a dir.
			fi, err := os.Stat(filepath.Join(cfg.Paths.ModelPath, name))
			if err != nil || !fi.IsDir() {
				continue
			}
			if !hasTaggerFiles(filepath.Join(cfg.Paths.ModelPath, name)) {
				continue
			}
			byName[name] = SeedTaggerInstance(name, true, catalogDefaults[name])
			order = append(order, name)
		}
	}

	// Overlay TOML entries so enable/threshold/file overrides win.
	for _, t := range cfg.Tagger.Taggers {
		if _, seen := byName[t.Name]; !seen {
			order = append(order, t.Name)
		}
		byName[t.Name] = t
	}

	out := make([]TaggerStatus, 0, len(order))
	for _, name := range order {
		t := byName[name]
		dir := filepath.Join(cfg.Paths.ModelPath, name)
		t.ModelFile, t.TagsFile = resolveTaggerFiles(dir, t.ModelFile, t.TagsFile)

		status := TaggerStatus{TaggerInstance: t, Available: true}
		onnxPath := filepath.Join(dir, t.ModelFile)
		tagsPath := filepath.Join(dir, t.TagsFile)
		if _, err := os.Stat(onnxPath); err != nil {
			status.Available = false
			status.Reason = "missing " + t.ModelFile
		} else if _, err := os.Stat(tagsPath); err != nil {
			status.Available = false
			status.Reason = "missing " + t.TagsFile
		}
		out = append(out, status)
	}
	return out
}

// EnabledTaggers returns taggers that are both enabled in config and
// available on disk. Returns nil on a noop build so the UI hides
// affordances that depend on inference. Used by surfaces that don't
// know which gallery a tag job is about to run on (e.g. the Settings
// page itself); per-gallery callers should use EnabledTaggersForGallery
// instead.
func EnabledTaggers(cfg *config.Config) []TaggerStatus {
	return enabledTaggers(cfg, nil)
}

// EnabledTaggersForGallery filters EnabledTaggers down to the rows whose
// per-tagger Galleries list either is empty (applies to every gallery,
// the legacy behaviour) or contains the named gallery. Used by every
// per-job entry point so a tagger configured for `default` doesn't fire
// on `stock` and vice versa.
func EnabledTaggersForGallery(cfg *config.Config, gallery string) []TaggerStatus {
	return enabledTaggers(cfg, func(t TaggerStatus) bool { return t.AppliesToGallery(gallery) })
}

// enabledTaggers backs both listings. A gallery-scoped tagger answers false
// to AppliesToGallery(""), so the unscoped listing passes no extra predicate
// rather than an empty gallery name.
func enabledTaggers(cfg *config.Config, extra func(TaggerStatus) bool) []TaggerStatus {
	if !buildSupportsInference() {
		return nil
	}
	var out []TaggerStatus
	for _, t := range DiscoverTaggers(cfg) {
		if !t.Enabled || !t.Available {
			continue
		}
		if extra != nil && !extra(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// resolveTaggerFiles picks model and tags filenames for one subfolder.
// Explicit TOML values win; otherwise the defaults (model.onnx,
// tags.csv, tags.txt) are tried, then a lone .onnx or a lone label
// file is auto-picked. Falls back to the defaults so the caller can
// surface a missing-file reason rather than an empty filename.
func resolveTaggerFiles(dir, explicitModel, explicitTags string) (string, string) {
	modelFile := explicitModel
	tagsFile := explicitTags

	var onnxFiles, labelFiles []string
	hasTagsCSV, hasTagsTXT := false, false
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			switch strings.ToLower(filepath.Ext(n)) {
			case ".onnx":
				onnxFiles = append(onnxFiles, n)
			case ".csv":
				labelFiles = append(labelFiles, n)
				if n == DefaultTagsFile {
					hasTagsCSV = true
				}
			case ".txt":
				labelFiles = append(labelFiles, n)
				if n == DefaultTextTagsFile {
					hasTagsTXT = true
				}
			case ".json":
				if !isTaggerSidecar(n) {
					labelFiles = append(labelFiles, n)
				}
			}
		}
	}

	if modelFile == "" {
		switch {
		case slices.Contains(onnxFiles, DefaultModelFile):
			modelFile = DefaultModelFile
		case len(onnxFiles) == 1:
			modelFile = onnxFiles[0]
		default:
			modelFile = DefaultModelFile
		}
	}

	if tagsFile == "" {
		switch {
		case hasTagsCSV:
			tagsFile = DefaultTagsFile
		case hasTagsTXT:
			tagsFile = DefaultTextTagsFile
		case len(labelFiles) == 1:
			tagsFile = labelFiles[0]
		default:
			tagsFile = DefaultTagsFile
		}
	}

	return modelFile, tagsFile
}

// isTaggerSidecar reports whether name is an operator sidecar rather
// than a label file: excluding them by name keeps the lone-label
// auto-pick and the empty-directory skip honest.
func isTaggerSidecar(name string) bool {
	return name == "tagger.json" || name == "dispatch.json"
}

// hasTaggerFiles reports whether dir contains at least one file with a
// tagger-related extension, used to skip empty subdirectories during
// discovery. `tagger.json` / `dispatch.json` sidecars don't count: a
// folder carrying only those is not a runnable tagger.
func hasTaggerFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		switch strings.ToLower(filepath.Ext(n)) {
		case ".onnx", ".csv", ".txt":
			return true
		case ".json":
			if !isTaggerSidecar(n) {
				return true
			}
		}
	}
	return false
}

// SeedTaggerInstance returns a fresh TaggerInstance with the given name
// and enabled flag, prefilled from the catalog's default_threshold,
// default_thresholds, and default_top_k when a non-nil entry is
// supplied. Used both by the discover-from-disk path and the per-row
// Enable handler so the same catalog-supplied defaults land in either
// flow without restating them. Categories absent from the catalog's
// DefaultTopK still resolve through DefaultPerCategoryTopK at merge
// time, so leaving the map nil here picks up the built-ins.
func SeedTaggerInstance(name string, enabled bool, catalog *CatalogEntry) config.TaggerInstance {
	t := config.TaggerInstance{
		Name:                name,
		Enabled:             enabled,
		ConfidenceThreshold: DefaultConfidenceThreshold,
	}
	if catalog == nil {
		return t
	}
	if catalog.DefaultThreshold > 0 {
		t.ConfidenceThreshold = catalog.DefaultThreshold
	}
	if len(catalog.DefaultThresholds) > 0 {
		t.CategoryThresholds = make(map[string]float64, len(catalog.DefaultThresholds))
		for k, v := range catalog.DefaultThresholds {
			t.CategoryThresholds[k] = v
		}
	}
	if len(catalog.DefaultTopK) > 0 {
		t.PerCategoryTopK = make(map[string]int, len(catalog.DefaultTopK))
		for k, v := range catalog.DefaultTopK {
			t.PerCategoryTopK[k] = v
		}
	}
	return t
}

// catalogDefaultsByName indexes the merged catalog (embedded + on-disk
// override) by tagger name so the discover loop can pick up
// default_threshold / default_thresholds without reparsing the JSON
// once per row.
func catalogDefaultsByName(modelPath string) map[string]*CatalogEntry {
	cat := LoadCatalog(modelPath)
	out := make(map[string]*CatalogEntry, len(cat))
	for i := range cat {
		out[cat[i].Name] = &cat[i]
	}
	return out
}
