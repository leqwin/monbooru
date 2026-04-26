package tagger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCatalog_DefaultEmbedded(t *testing.T) {
	tmp := t.TempDir() // no models.json present
	cat := LoadCatalog(tmp)
	if len(cat) < 2 {
		t.Fatalf("expected at least 2 default catalog entries, got %d", len(cat))
	}
	names := map[string]bool{}
	for _, e := range cat {
		names[e.Name] = true
	}
	for _, want := range []string{"wd-swinv2", "joytag"} {
		if !names[want] {
			t.Errorf("default catalog missing %q", want)
		}
	}
}

func TestLoadCatalog_OverrideAddsAndReplaces(t *testing.T) {
	tmp := t.TempDir()
	override := `{
		"version": 1,
		"models": [
			{"name": "wd-swinv2", "description": "custom",
			 "files": [{"url": "https://example.invalid/m.onnx", "filename": "model.onnx"}]},
			{"name": "extra", "description": "added",
			 "files": [{"url": "https://example.invalid/e.onnx", "filename": "model.onnx"}]}
		]
	}`
	if err := os.WriteFile(filepath.Join(tmp, "models.json"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := LoadCatalog(tmp)
	byName := map[string]CatalogEntry{}
	for _, e := range cat {
		byName[e.Name] = e
	}
	if e := byName["wd-swinv2"]; e.Description != "custom" {
		t.Errorf("override did not replace wd-swinv2: %+v", e)
	}
	if _, ok := byName["extra"]; !ok {
		t.Errorf("override did not add 'extra' entry")
	}
	if _, ok := byName["joytag"]; !ok {
		t.Errorf("default 'joytag' entry should still be present")
	}
}

func TestCatalogEntry_HostCommand_QuotesPathsAndURLs(t *testing.T) {
	e := CatalogEntry{
		Name: "wd-swinv2",
		Files: []CatalogFile{
			{URL: "https://huggingface.co/foo/m.onnx", Filename: "model.onnx"},
			{URL: "https://huggingface.co/foo/tags.csv", Filename: "tags.csv"},
		},
	}
	cmd := e.HostCommand()
	if !strings.Contains(cmd, "mkdir -p 'wd-swinv2'") {
		t.Errorf("missing single-quoted mkdir target: %q", cmd)
	}
	if !strings.Contains(cmd, "'https://huggingface.co/foo/m.onnx'") {
		t.Errorf("URL not single-quoted: %q", cmd)
	}
	if !strings.Contains(cmd, "'wd-swinv2/model.onnx'") {
		t.Errorf("destination path not single-quoted: %q", cmd)
	}
	if !strings.Contains(cmd, " && \\\n") {
		t.Errorf("expected && line continuation between curl invocations: %q", cmd)
	}
}

func TestCatalogEntry_DockerCommand_DefaultsContainerName(t *testing.T) {
	e := CatalogEntry{
		Name: "joytag",
		Files: []CatalogFile{
			{URL: "https://example.com/m.onnx", Filename: "model.onnx"},
		},
	}
	cmd := e.DockerCommand("")
	if !strings.HasPrefix(cmd, "docker exec monbooru sh -c '") {
		t.Errorf("default container name not 'monbooru': %q", cmd)
	}
	if !strings.Contains(cmd, "mkdir -p '\\''/models/joytag'\\'' && curl") {
		// The inner script is single-quoted; embedded single quotes around
		// /models/joytag get the '\'' escape and the body chains with &&.
		t.Errorf("docker inner script not chained: %q", cmd)
	}
}

func TestCatalogEntry_DockerCommand_CustomContainerName(t *testing.T) {
	e := CatalogEntry{Name: "x", Files: []CatalogFile{{URL: "u", Filename: "f"}}}
	cmd := e.DockerCommand("alt-name")
	if !strings.HasPrefix(cmd, "docker exec alt-name sh -c '") {
		t.Errorf("custom container name not honoured: %q", cmd)
	}
}
