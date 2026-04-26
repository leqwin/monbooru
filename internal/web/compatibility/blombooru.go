package compatibility

import (
	"archive/zip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func init() {
	Register(Provider{
		Name:      "blombooru",
		Detect:    detectBlombooru,
		Translate: translateBlombooru,
	})
}

// blombooruBackup is the relevant subset of the JSON document blombooru
// emits as `backup.json` at the archive root.
type blombooruBackup struct {
	Version int                    `json:"version"`
	Type    string                 `json:"type"`
	Media   []blombooruBackupMedia `json:"media"`
}

type blombooruBackupMedia struct {
	Filename    string   `json:"filename"`
	Hash        string   `json:"hash"`
	Tags        []string `json:"tags"`
	ArchivePath string   `json:"archive_path"`
}

// detectBlombooru: a Blombooru full backup carries `backup.json` at the
// archive root and at least one entry under `media/`.
func detectBlombooru(entries []NormalizedEntry) bool {
	hasBackup, hasMedia := false, false
	for _, e := range entries {
		switch {
		case e.Rel == "backup.json":
			hasBackup = true
		case strings.HasPrefix(e.Rel, "media/"):
			hasMedia = true
		}
		if hasBackup && hasMedia {
			return true
		}
	}
	return false
}

// translateBlombooru parses backup.json (mandatory) plus tags.csv
// (optional, supplies category attribution) and emits the light-shaped
// manifest plus the media files map.
func translateBlombooru(entries []NormalizedEntry) (Result, error) {
	var backupFile, tagsCSV *zip.File
	media := map[string]*zip.File{}
	for _, e := range entries {
		switch {
		case e.Rel == "backup.json":
			backupFile = e.File
		case e.Rel == "tags.csv":
			tagsCSV = e.File
		case strings.HasPrefix(e.Rel, "media/"):
			media[strings.TrimPrefix(e.Rel, "media/")] = e.File
		}
	}
	if backupFile == nil {
		return Result{}, fmt.Errorf("blombooru archive missing backup.json")
	}

	catByTag, err := readBlombooruTagsCSV(tagsCSV)
	if err != nil {
		return Result{}, err
	}

	rc, err := backupFile.Open()
	if err != nil {
		return Result{}, fmt.Errorf("open backup.json: %w", err)
	}
	var bb blombooruBackup
	err = json.NewDecoder(rc).Decode(&bb)
	rc.Close()
	if err != nil {
		return Result{}, fmt.Errorf("decode backup.json: %w", err)
	}

	out := Result{Files: map[string]*zip.File{}}
	for _, m := range bb.Media {
		// archive_path is "media/<filename>"; route the file under the
		// same basename in the target gallery root.
		rel := strings.TrimPrefix(m.ArchivePath, "media/")
		if rel == "" {
			rel = m.Filename
		}
		if rel == "" {
			continue
		}
		out.Manifest.Images = append(out.Manifest.Images, ManifestImage{
			SHA256: PickValidSHA256(m.Hash),
			Path:   rel,
			Tags:   blombooruTagTokens(m.Tags, catByTag),
		})
		if zf, ok := media[rel]; ok {
			out.Files[rel] = zf
		}
	}
	return out, nil
}

// blombooruTagTokens turns a record's plain-string tag list into the
// `name` / `category:name` token form the apply path expects, using the
// (tag → category) map built from tags.csv. Tags missing from the map
// fall through as `general` (no prefix).
func blombooruTagTokens(tags []string, catByTag map[string]string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		cat := catByTag[t]
		if cat != "" && cat != "general" {
			out = append(out, cat+":"+t)
		} else {
			out = append(out, t)
		}
	}
	return out
}

// readBlombooruTagsCSV builds a (tag-name → category-name) map from a
// blombooru tags.csv. Schema: `name, category_id [, ...]`. Best-effort:
// a missing or malformed file yields an empty map so every tag falls
// back to general.
func readBlombooruTagsCSV(tagsFile *zip.File) (map[string]string, error) {
	if tagsFile == nil {
		return map[string]string{}, nil
	}
	rc, err := tagsFile.Open()
	if err != nil {
		return nil, fmt.Errorf("open tags.csv: %w", err)
	}
	defer rc.Close()
	out := map[string]string{}
	r := csv.NewReader(rc)
	r.FieldsPerRecord = -1
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, nil
		}
		if len(rec) < 2 {
			continue
		}
		name := strings.TrimSpace(rec[0])
		if name == "" {
			continue
		}
		out[name] = blombooruCategoryByID(strings.TrimSpace(rec[1]))
	}
	return out, nil
}

// blombooruCategoryByID maps the blombooru category enum to monbooru's
// built-in category names. Anything unrecognised falls through to
// `general` so a future blombooru schema change degrades gracefully
// rather than dropping tags.
func blombooruCategoryByID(id string) string {
	switch id {
	case "0":
		return "general"
	case "1":
		return "artist"
	case "3":
		return "copyright"
	case "4":
		return "character"
	case "5":
		return "meta"
	}
	return "general"
}
