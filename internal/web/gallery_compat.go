package web

import (
	"archive/zip"
	"sort"

	"github.com/leqwin/monbooru/internal/web/compatibility"
)

// detectCompatFormat is a thin alias over compatibility.Detect kept so
// the call sites in gallery_io.go and gallery_merge.go stay readable.
// Importing the compatibility package here also runs its providers'
// init() functions, registering the per-application translators.
func detectCompatFormat(files []*zip.File) string {
	return compatibility.Detect(files)
}

// replaceFromCompatArchive translates a foreign-format zip and routes
// through the same wipe+extract+ingest path the native light replacer
// uses. The compat package's Manifest/Files shapes mirror lightManifest
// 1:1, so the boundary conversion is a single field copy. The provider
// name (`format`) is propagated to applyLightReplace so the detail page
// credits the originating app instead of the generic "import" bucket.
func replaceFromCompatArchive(files []*zip.File, format, dbPath, thumbsPath, galleryPath string) error {
	result, err := compatibility.Translate(files, format)
	if err != nil {
		return err
	}
	return applyLightReplace(
		toLightManifest(result.Manifest),
		translatedFilesFromCompat(result.Files),
		dbPath, thumbsPath, galleryPath, format,
	)
}

// mergeFromCompatArchive translates a foreign-format zip and applies its
// records through the existing zip-merge path: tags onto pre-existing
// SHAs, ingest-and-tag for new SHAs that ride along as zip entries.
func mergeFromCompatArchive(cx *galleryCtx, files []*zip.File, format string) error {
	result, err := compatibility.Translate(files, format)
	if err != nil {
		return err
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
	applyMergeRecords(cx, records, format)
	return nil
}

// toLightManifest converts the compat-package shape into the
// lightManifest the apply path consumes. Version is stamped to the
// current galleryExportVersion since the in-memory translation has no
// notion of an on-disk format version.
func toLightManifest(m compatibility.Manifest) lightManifest {
	out := lightManifest{
		Version: galleryExportVersion,
		Images:  make([]lightManifestImage, 0, len(m.Images)),
	}
	for _, img := range m.Images {
		out.Images = append(out.Images, lightManifestImage{
			SHA256: img.SHA256,
			Path:   img.Path,
			Tags:   img.Tags,
		})
	}
	return out
}

// translatedFilesFromCompat sorts the {rel → file} map deterministically
// and rewraps it as the slice applyLightReplace consumes.
func translatedFilesFromCompat(in map[string]*zip.File) []translatedFile {
	rels := make([]string, 0, len(in))
	for r := range in {
		rels = append(rels, r)
	}
	sort.Strings(rels)
	out := make([]translatedFile, 0, len(rels))
	for _, r := range rels {
		out = append(out, translatedFile{rel: r, file: in[r]})
	}
	return out
}

