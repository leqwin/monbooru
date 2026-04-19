//go:build tagger

package tagger

import (
	"testing"

	"github.com/leqwin/monbooru/internal/config"
)

// TestEnabledTaggers_FiltersUnavailable exercises EnabledTaggers on a build
// that supports inference: the function must drop subfolders whose files
// aren't both present, leaving only usable taggers. Paired with
// TestEnabledTaggers_NoopBuildReturnsNil in discover_test.go.
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
