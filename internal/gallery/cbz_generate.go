package gallery

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/fsx"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
)

// CBZMember is one collection member fed to WriteCollectionCBZ: its
// canonical path and file type, in the order the pages should appear.
// filename is the member's basename, used to sort unordered members into
// natural filename order before packing.
type CBZMember struct {
	Path     string
	FileType string
	filename string
}

// WriteCollectionCBZ packs members into a cbz archive at dstPath: every
// member as a page named 0001.ext, 0002.ext, ... in slice order, plus a
// ComicInfo.xml carrying the title and page count. Members whose file
// has vanished since the query are skipped and counted in skipped;
// numbering stays contiguous over the pages actually written.
//
// A member that is not a still image vetoes the whole archive rather
// than being dropped: a caller asking for a comic out of a set that
// includes a video wants to hear about it, not to get the rest.
// Nothing is created on disk before that check clears.
//
// The archive is written to a temp file and atomically renamed so a
// watcher never ingests a half-written archive; pages are stored
// uncompressed (zip.Store) since images are already compressed.
func WriteCollectionCBZ(ctx context.Context, dstPath string, members []CBZMember, title string, progress func(processed, total int, message string)) (pages, skipped int, err error) {
	for _, m := range members {
		if models.MediaKind(m.FileType) != "image" {
			return 0, 0, fmt.Errorf("%s is an animated image, video or archive: it cannot be a cbz page", filepath.Base(m.Path))
		}
	}
	total := len(members)
	if total == 0 {
		return 0, 0, errors.New("no members to generate")
	}
	if progress != nil {
		progress(0, total, "generating…")
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, 0, fmt.Errorf("create output dir: %w", err)
	}
	err = fsx.WriteAtomic(dstPath, ".cbz-generate-*", func(tmp *os.File) error {
		zw := zip.NewWriter(tmp)
		for _, m := range members {
			if ctx != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			// The file can have vanished between the member query and now (a
			// concurrent delete); skip it rather than failing the generation.
			f, openErr := os.Open(m.Path)
			if openErr != nil {
				skipped++
				continue
			}
			// Entry extension from the stored file type, not the on-disk
			// name, so extension-less files still produce recognized pages.
			// Numbered off the written count, not the member index, so a
			// skipped member leaves no gap in the page sequence.
			entry, err := zw.CreateHeader(&zip.FileHeader{
				Name:   fmt.Sprintf("%04d.%s", pages+1, m.FileType),
				Method: zip.Store,
			})
			if err != nil {
				_ = f.Close()
				return err
			}
			_, copyErr := io.Copy(entry, f)
			_ = f.Close()
			if copyErr != nil {
				return fmt.Errorf("write page %d: %w", pages+1, copyErr)
			}
			pages++
			if progress != nil {
				progress(pages, total, "generating…")
			}
		}
		if pages == 0 {
			return errors.New("every member's file is missing from disk")
		}

		// ComicInfo.xml is deflated since it is text; readers locate it by
		// name, so its position in the archive is irrelevant.
		ci, err := metadata.MarshalComicInfo(title, pages)
		if err != nil {
			return fmt.Errorf("comic info: %w", err)
		}
		ciEntry, err := zw.CreateHeader(&zip.FileHeader{Name: "ComicInfo.xml", Method: zip.Deflate})
		if err != nil {
			return err
		}
		if _, err := ciEntry.Write(ci); err != nil {
			return fmt.Errorf("write comic info: %w", err)
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("close zip: %w", err)
		}
		return nil
	})
	if err != nil {
		return pages, skipped, err
	}
	// CreateTemp leaves 0600; align with the 0644 of files that land in
	// the gallery through other paths so host-side tooling sees the same
	// mode. Best-effort: a chmod failure should not fail the generation.
	if err := os.Chmod(dstPath, 0o644); err != nil {
		logx.Warnf("cbz generation: chmod %q: %v", dstPath, err)
	}
	return pages, skipped, nil
}
