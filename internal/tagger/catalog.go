package tagger

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed catalog_default.json
var defaultCatalogJSON []byte

// CatalogEntry describes one downloadable tagger: a target subfolder name
// under paths.model_path plus the URLs the user fetches the model and tags
// file from. Monbooru itself never reaches out to these URLs - the Settings
// → Auto-Tagger dialog only renders copy-paste curl commands so the
// "no automatic outbound HTTP" promise stays intact.
type CatalogEntry struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Files       []CatalogFile `json:"files"`
}

// CatalogFile is one URL-to-filename pair; Filename is the basename the file
// gets dropped under inside <modelPath>/<entry name>/.
type CatalogFile struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
}

type catalogDoc struct {
	Version int            `json:"version"`
	Models  []CatalogEntry `json:"models"`
}

// LoadCatalog returns the merged tagger catalog. The embedded default
// catalog (two suggested taggers - WD14 SwinV2 and JoyTag) is the base; an
// optional <modelPath>/models.json override is applied on top so users can
// add or replace entries without rebuilding. Same-name entries in the
// override replace the default; new names append.
func LoadCatalog(modelPath string) []CatalogEntry {
	var doc catalogDoc
	if err := json.Unmarshal(defaultCatalogJSON, &doc); err != nil {
		// Embedded asset is shipped with the binary; an invalid one is a
		// build-time bug, not a runtime concern. Surface as empty.
		return nil
	}
	out := append([]CatalogEntry(nil), doc.Models...)

	if data, err := os.ReadFile(filepath.Join(modelPath, "models.json")); err == nil {
		var override catalogDoc
		if err := json.Unmarshal(data, &override); err == nil {
			byName := map[string]int{}
			for i, e := range out {
				byName[e.Name] = i
			}
			for _, e := range override.Models {
				if i, ok := byName[e.Name]; ok {
					out[i] = e
				} else {
					byName[e.Name] = len(out)
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// HostCommand renders the `mkdir + curl` chain a user runs on the host
// (no docker). Paths are relative to the model path.
func (c CatalogEntry) HostCommand() string {
	parts := []string{"mkdir -p " + shellSingleQuote(c.Name)}
	for _, f := range c.Files {
		dst := c.Name + "/" + f.Filename
		parts = append(parts, "curl -L -o "+shellSingleQuote(dst)+" "+shellSingleQuote(f.URL))
	}
	return strings.Join(parts, " && \\\n")
}

// DockerCommand renders a `docker exec <container> sh -c '...'` chain that
// drops model files into the container's /models mount. Container name
// defaults to "monbooru" (matching the shipped docker-compose.yml).
func (c CatalogEntry) DockerCommand(containerName string) string {
	if containerName == "" {
		containerName = "monbooru"
	}
	target := "/models/" + c.Name
	parts := []string{"mkdir -p " + shellSingleQuote(target)}
	for _, f := range c.Files {
		dst := target + "/" + f.Filename
		parts = append(parts, "curl -L -o "+shellSingleQuote(dst)+" "+shellSingleQuote(f.URL))
	}
	inner := strings.Join(parts, " && ")
	return fmt.Sprintf("docker exec %s sh -c %s", containerName, shellSingleQuote(inner))
}

// shellSingleQuote wraps s in shell single quotes, escaping any embedded
// single quote with the standard `'\''` recipe.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
