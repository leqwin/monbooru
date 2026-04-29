//go:build tagger

package tagger

import (
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/config"
)

func TestCacheSatisfies(t *testing.T) {
	c := taggerCache{
		initialized: true,
		useCUDA:     false,
		sessions: map[string]*loadedSession{
			"wd-swinv2": {modelFile: "model.onnx", tagsFile: "tags.csv"},
			"joytag":    {modelFile: "model.onnx", tagsFile: "tags.txt"},
		},
	}

	mk := func(name, model, tags string) TaggerStatus {
		return TaggerStatus{TaggerInstance: config.TaggerInstance{
			Name: name, ModelFile: model, TagsFile: tags,
		}}
	}

	tests := []struct {
		name    string
		req     []TaggerStatus
		useCUDA bool
		want    bool
	}{
		{"single match", []TaggerStatus{mk("wd-swinv2", "model.onnx", "tags.csv")}, false, true},
		{"both match", []TaggerStatus{
			mk("wd-swinv2", "model.onnx", "tags.csv"),
			mk("joytag", "model.onnx", "tags.txt"),
		}, false, true},
		{"useCUDA flip invalidates", []TaggerStatus{mk("wd-swinv2", "model.onnx", "tags.csv")}, true, false},
		{"unknown tagger", []TaggerStatus{mk("ghost", "model.onnx", "tags.csv")}, false, false},
		{"model file swapped", []TaggerStatus{mk("wd-swinv2", "v3.onnx", "tags.csv")}, false, false},
		{"tags file swapped", []TaggerStatus{mk("wd-swinv2", "model.onnx", "selected_tags.csv")}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.satisfies(tc.req, tc.useCUDA); got != tc.want {
				t.Fatalf("satisfies: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCacheSatisfies_Uninitialized(t *testing.T) {
	c := taggerCache{}
	if c.satisfies(nil, false) {
		t.Fatalf("uninitialized cache must not satisfy any request")
	}
}

func TestReleaseIdle_Cold(t *testing.T) {
	// Cold package-global cache: ReleaseIdle is a no-op.
	if ReleaseIdle(time.Hour) {
		t.Fatalf("ReleaseIdle on a cold cache must return false")
	}
}

func TestReleaseAll_Cold(t *testing.T) {
	// A second teardown on a cold cache must be safe; it's the path
	// hit when the server closes without ever running a tagger job.
	ReleaseAll()
	ReleaseAll()
}
