package tagger

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
)

// Default filenames for a tagger subfolder. Either can be overridden per
// tagger via its TOML entry. When a subfolder has neither default, a lone
// .onnx / .csv / .txt is picked automatically.
const (
	DefaultModelFile    = "model.onnx"
	DefaultTagsFile     = "tags.csv"
	DefaultTextTagsFile = "tags.txt"
	// DefaultConfidenceThreshold is applied to taggers discovered on disk
	// that do not yet have a TOML entry with an explicit threshold.
	DefaultConfidenceThreshold = 0.4
)

// TaggerStatus pairs a configured tagger with its runtime availability
// so the settings UI can show why a tagger is active or inactive.
type TaggerStatus struct {
	config.TaggerInstance
	Available bool
	Reason    string // populated when Available is false
}

// DiscoverTaggers lists the tagger subfolders under paths.model_path and
// merges them with the configured list from cfg.Tagger.Taggers. An entry
// exists in the result for every subfolder present on disk OR every
// configured tagger (so users can see leftover config even if the folder
// is gone). Results are sorted by Name.
func DiscoverTaggers(cfg *config.Config) []TaggerStatus {
	byName := map[string]config.TaggerInstance{}
	order := []string{}

	// Start from disk so untouched subfolders appear even without config.
	// Discovered subfolders are enabled by default; explicit TOML entries
	// below override (Disable actions persist Enabled=false). Completely
	// empty subdirectories (no .onnx / .csv / .txt) are skipped so they
	// don't show up in the settings table as permanently-broken rows.
	if entries, err := os.ReadDir(cfg.Paths.ModelPath); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if !hasTaggerFiles(filepath.Join(cfg.Paths.ModelPath, name)) {
				continue
			}
			byName[name] = config.TaggerInstance{
				Name:                name,
				Enabled:             true,
				ConfidenceThreshold: DefaultConfidenceThreshold,
			}
			order = append(order, name)
		}
	}

	// Overlay configured entries so enabled/threshold/file overrides win.
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

// EnabledTaggers returns the taggers that are both enabled in config and
// have their files present on disk. Returns nil on a noop build so UI
// affordances that offer to run the tagger stay hidden on a binary that
// cannot run one.
func EnabledTaggers(cfg *config.Config) []TaggerStatus {
	if !buildSupportsInference() {
		return nil
	}
	var out []TaggerStatus
	for _, t := range DiscoverTaggers(cfg) {
		if t.Enabled && t.Available {
			out = append(out, t)
		}
	}
	return out
}

// resolveTaggerFiles picks the model and tags filenames for one tagger
// subfolder. Explicit TOML values always win. Otherwise the default names
// (model.onnx, tags.csv, tags.txt) are tried, then a lone .onnx or a lone
// label file (.csv/.txt) is auto-picked whatever its name. When nothing
// matches, the defaults are returned so the caller surfaces a missing-file
// reason instead of a silent empty filename.
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
			}
		}
	}

	if modelFile == "" {
		switch {
		case contains(onnxFiles, DefaultModelFile):
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

// hasTaggerFiles reports whether dir contains at least one file with a
// tagger-related extension. Used to skip completely empty subdirectories
// during discovery (H13 in the UI audit).
func hasTaggerFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".onnx", ".csv", ".txt":
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
