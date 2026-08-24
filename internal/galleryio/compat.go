package galleryio

import (
	"archive/zip"
	"maps"
	"slices"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio/compatibility"
)

// Importing the compatibility package here runs its providers' init()
// functions, registering the per-application translators.
func detectCompatFormat(files []*zip.File) string {
	return compatibility.Detect(files)
}

// replaceFromCompatArchive routes a foreign-format zip through the native
// light-replacer path. format is propagated to ApplyLightReplace so the
// detail page credits the originating app instead of the generic "import".
func replaceFromCompatArchive(files []*zip.File, format, dbPath, thumbsPath, galleryPath string, maxFileSizeMB int) error {
	result, err := compatibility.Translate(files, format)
	if err != nil {
		return err
	}
	return ApplyLightReplace(
		toLightManifest(result.Manifest),
		translatedFilesFromCompat(result.Files),
		dbPath, thumbsPath, galleryPath, format, maxFileSizeMB,
	)
}

// mergeFromCompatArchive routes a foreign-format zip through the zip-merge
// path: tags onto existing SHAs, ingest-and-tag for new SHAs.
func mergeFromCompatArchive(cx gallery.Handle, files []*zip.File, format string, maxFileSizeMB int) (MergeResult, error) {
	result, err := compatibility.Translate(files, format)
	if err != nil {
		return MergeResult{}, err
	}
	records := make([]mergeRecord, 0, len(result.Manifest.Images))
	for _, m := range result.Manifest.Images {
		rec := mergeRecord{
			SHA256:     m.SHA256,
			Tags:       m.Tags,
			SourcePath: m.Path,
		}
		if zf, ok := result.Files[m.Path]; ok {
			rec.zipEntry = zf
		}
		records = append(records, rec)
	}
	return applyMergeRecords(cx, records, format, maxFileSizeMB), nil
}

func toLightManifest(m compatibility.Manifest) LightManifest {
	out := LightManifest{
		Version: LightManifestVersion,
		Images:  make([]LightManifestImage, 0, len(m.Images)),
	}
	for _, img := range m.Images {
		out.Images = append(out.Images, LightManifestImage{
			SHA256: img.SHA256,
			Path:   img.Path,
			Tags:   img.Tags,
		})
	}
	return out
}

func translatedFilesFromCompat(in map[string]*zip.File) []translatedFile {
	rels := slices.Sorted(maps.Keys(in))
	out := make([]translatedFile, 0, len(rels))
	for _, r := range rels {
		out = append(out, translatedFile{rel: r, file: in[r]})
	}
	return out
}
