package tagger

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leqwin/monbooru/internal/config"
)

// makeTaggerDir lays out a tagger subfolder with the given files (each name
// becomes an empty file) under tmpDir/<name>/. Returns the parent dir.
func makeTaggerDir(t *testing.T, tmpDir, name string, files []string) string {
	t.Helper()
	dir := filepath.Join(tmpDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatalf("write %q: %v", f, err)
		}
	}
	return tmpDir
}

func TestResolveTaggerFiles_Defaults(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd")
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv"})

	model, tags := resolveTaggerFiles(dir, "", "")
	if model != "model.onnx" {
		t.Errorf("model = %q, want model.onnx", model)
	}
	if tags != "tags.csv" {
		t.Errorf("tags = %q, want tags.csv", tags)
	}
}

func TestResolveTaggerFiles_LoneOnnxAndCSV(t *testing.T) {
	// When neither default name is present, a lone .onnx + lone label file
	// should be auto-picked even with non-canonical names.
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "joytag")
	makeTaggerDir(t, tmp, "joytag", []string{"weights.onnx", "top_tags.txt"})

	model, tags := resolveTaggerFiles(dir, "", "")
	if model != "weights.onnx" {
		t.Errorf("model = %q, want weights.onnx", model)
	}
	if tags != "top_tags.txt" {
		t.Errorf("tags = %q, want top_tags.txt", tags)
	}
}

func TestResolveTaggerFiles_DefaultsBeatLone(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd")
	// Two label files - the canonical default name must win.
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv", "extra.txt"})

	_, tags := resolveTaggerFiles(dir, "", "")
	if tags != "tags.csv" {
		t.Errorf("tags = %q, want tags.csv to win when present", tags)
	}
}

func TestResolveTaggerFiles_AmbiguousLabelFalls(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd")
	// Two label files of different extensions, neither matches a default -
	// the resolver gives up and returns the default name so the caller
	// surfaces a missing-file reason.
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "labels_a.csv", "labels_b.txt"})

	_, tags := resolveTaggerFiles(dir, "", "")
	if tags != DefaultTagsFile {
		t.Errorf("tags = %q, want fallback to %q", tags, DefaultTagsFile)
	}
}

func TestResolveTaggerFiles_ExplicitTOMLWins(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "wd")
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv", "selected.csv"})

	model, tags := resolveTaggerFiles(dir, "model.onnx", "selected.csv")
	if model != "model.onnx" {
		t.Errorf("model = %q", model)
	}
	if tags != "selected.csv" {
		t.Errorf("tags = %q, explicit value should win", tags)
	}
}

func TestDiscoverTaggers_FromDiskOnly(t *testing.T) {
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv"})
	makeTaggerDir(t, tmp, "joytag", []string{"weights.onnx", "tags.txt"})

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	got := DiscoverTaggers(cfg)
	if len(got) != 2 {
		t.Fatalf("got %d taggers, want 2: %+v", len(got), got)
	}
	for _, ts := range got {
		if !ts.Available {
			t.Errorf("tagger %q expected Available=true, reason=%q", ts.Name, ts.Reason)
		}
		if !ts.Enabled {
			t.Errorf("tagger %q expected default Enabled=true", ts.Name)
		}
	}
}

func TestDiscoverTaggers_TOMLOverlayWins(t *testing.T) {
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx", "tags.csv"})

	cfg := &config.Config{
		Paths: config.PathsConfig{ModelPath: tmp},
		Tagger: config.TaggerConfig{
			Taggers: []config.TaggerInstance{{
				Name:                "wd",
				Enabled:             false,
				ConfidenceThreshold: 0.42,
			}},
		},
	}
	got := DiscoverTaggers(cfg)
	if len(got) != 1 {
		t.Fatalf("got %d taggers, want 1", len(got))
	}
	if got[0].Enabled {
		t.Error("explicit Enabled=false from TOML should override discovery default")
	}
	if got[0].ConfidenceThreshold != 0.42 {
		t.Errorf("threshold = %v, want 0.42", got[0].ConfidenceThreshold)
	}
}

func TestDiscoverTaggers_MissingFilesUnavailable(t *testing.T) {
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "wd", []string{"model.onnx"})   // missing tags file
	makeTaggerDir(t, tmp, "joytag", []string{"tags.txt"}) // missing model

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	got := DiscoverTaggers(cfg)
	byName := map[string]TaggerStatus{}
	for _, t := range got {
		byName[t.Name] = t
	}
	if wd, ok := byName["wd"]; !ok || wd.Available {
		t.Errorf("wd should be unavailable: %+v", wd)
	}
	if jt, ok := byName["joytag"]; !ok || jt.Available {
		t.Errorf("joytag should be unavailable: %+v", jt)
	}
}

func TestEnabledTaggers_FiltersUnavailable(t *testing.T) {
	tmp := t.TempDir()
	makeTaggerDir(t, tmp, "ok", []string{"model.onnx", "tags.csv"})
	makeTaggerDir(t, tmp, "broken", []string{"model.onnx"}) // no labels

	cfg := &config.Config{Paths: config.PathsConfig{ModelPath: tmp}}
	got := EnabledTaggers(cfg)
	if len(got) != 1 || got[0].Name != "ok" {
		t.Errorf("EnabledTaggers = %+v, want only `ok`", got)
	}
}
