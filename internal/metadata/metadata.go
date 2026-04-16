package metadata

import (
	"os"
	"sort"

	"github.com/rwcarlsen/goexif/exif"
	"github.com/rwcarlsen/goexif/tiff"
	"github.com/leqwin/monbooru/internal/models"
)

// Extract extracts SD and ComfyUI metadata from a file.
// Returns at most one of the two structs (the other will be nil).
// Failures are returned as nil structs (not errors); parsing is best-effort.
func Extract(path, fileType string) (*models.SDMetadata, *models.ComfyUIMetadata, error) {
	switch fileType {
	case "png":
		sd, comfy, err := extractFromPNG(path)
		return sd, comfy, err
	case "jpeg":
		sd, err := extractSDFromJPEG(path)
		return sd, nil, err
	case "webp":
		sd, err := extractSDFromWebP(path)
		return sd, nil, err
	default:
		return nil, nil, nil
	}
}

// ExtractGeneric returns image metadata key-value pairs that the SD and
// ComfyUI parsers do not consume (extra PNG text chunks, EXIF tags, …).
// Best-effort: file read errors return an empty slice.
func ExtractGeneric(path, fileType string) []models.SDParam {
	switch fileType {
	case "png":
		return genericFromPNG(path)
	case "jpeg":
		return genericFromEXIF(path)
	case "webp":
		return genericFromWebP(path)
	default:
		return nil
	}
}

func genericFromPNG(path string) []models.SDParam {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	chunks, err := readPNGTextChunks(f)
	if err != nil {
		return nil
	}
	skip := map[string]bool{"parameters": true, "prompt": true, "workflow": true}
	keys := make([]string, 0, len(chunks))
	for k := range chunks {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]models.SDParam, 0, len(keys))
	for _, k := range keys {
		out = append(out, models.SDParam{Key: k, Val: chunks[k]})
	}
	return out
}

func genericFromEXIF(path string) []models.SDParam {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil {
		return nil
	}
	return collectEXIFTags(x)
}

// collectEXIFTags walks every EXIF tag (across all IFDs) and returns them as
// key/value pairs, skipping the SD-source UserComment field.
func collectEXIFTags(x *exif.Exif) []models.SDParam {
	type kv struct{ k, v string }
	var pairs []kv
	x.Walk(walkFunc(func(name exif.FieldName, tag *tiff.Tag) error {
		if name == exif.UserComment {
			return nil
		}
		pairs = append(pairs, kv{k: string(name), v: tag.String()})
		return nil
	}))
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })
	out := make([]models.SDParam, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, models.SDParam{Key: p.k, Val: p.v})
	}
	return out
}

// walkFunc adapts a closure to the exif.Walker interface.
type walkFunc func(name exif.FieldName, tag *tiff.Tag) error

func (w walkFunc) Walk(name exif.FieldName, tag *tiff.Tag) error { return w(name, tag) }
