package gallery

import (
	"archive/zip"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/fsx"
)

// MangaPage describes one image entry inside a cbz/zip archive. Path is
// the entry's full archive path verbatim (used by the zip reader);
// OriginalName is the leaf basename used to choose the cache extension
// for the serve / page-bytes path.
type MangaPage struct {
	Path         string
	OriginalName string
}

// Manga is an opened cbz/zip archive plus its sorted page list. The
// caller must Close() to release the file handle.
type Manga struct {
	Pages  []MangaPage
	zr     *zip.ReadCloser
	byPath map[string]*zip.File
}

// ErrEmptyManga is returned when a zip carries no recognised image
// entries. Ingest rejects the file rather than creating an empty row.
var ErrEmptyManga = errors.New("archive contains no recognised image entries")

// pageImageExts is the set of extensions accepted as manga pages.
// Mirrors the gallery's standalone image support so a cbz of webp or
// gif files works identically to one of jpeg/png.
var pageImageExts = map[string]struct{}{
	".jpg":  {},
	".jpeg": {},
	".png":  {},
	".webp": {},
	".gif":  {},
}

// pageSkipBasenames are filesystem artefacts a creator's archiver would
// have folded into the zip but that aren't comic content. Folded out at
// the page-list build step so they never reach the reader UI.
var pageSkipBasenames = map[string]struct{}{
	".ds_store":   {},
	"thumbs.db":   {},
	"desktop.ini": {},
}

// OpenManga opens path as a zip, builds the natural-sorted page list,
// and returns the open archive. ErrEmptyManga is returned for zips with
// zero recognised image entries; callers ingest no row in that case.
func OpenManga(path string) (*Manga, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open archive %q: %w", path, err)
	}
	pages := make([]MangaPage, 0, len(zr.File))
	byPath := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		name := f.Name
		if strings.HasSuffix(name, "/") {
			continue
		}
		if strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		base := strings.ToLower(filepath.Base(name))
		if _, skip := pageSkipBasenames[base]; skip {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if _, ok := pageImageExts[ext]; !ok {
			continue
		}
		if f.UncompressedSize64 == 0 {
			continue
		}
		pages = append(pages, MangaPage{Path: name, OriginalName: filepath.Base(name)})
		byPath[name] = f
	}
	sort.Slice(pages, func(i, j int) bool {
		return NaturalLess(strings.ToLower(pages[i].Path), strings.ToLower(pages[j].Path))
	})
	if len(pages) == 0 {
		_ = zr.Close()
		return nil, ErrEmptyManga
	}
	return &Manga{Pages: pages, zr: zr, byPath: byPath}, nil
}

// Close releases the archive file handle.
func (m *Manga) Close() error {
	if m == nil || m.zr == nil {
		return nil
	}
	return m.zr.Close()
}

// Reader returns the underlying *zip.Reader for callers that want to
// stream entries directly (e.g. the ComicInfo parser).
func (m *Manga) Reader() *zip.Reader {
	if m == nil || m.zr == nil {
		return nil
	}
	return &m.zr.Reader
}

// PageReader opens an io.ReadCloser for the n-th page (0-based).
// Callers must Close it.
func (m *Manga) PageReader(n int) (io.ReadCloser, error) {
	if n < 0 || n >= len(m.Pages) {
		return nil, fmt.Errorf("page %d out of range [1,%d]", n+1, len(m.Pages))
	}
	f, ok := m.byPath[m.Pages[n].Path]
	if !ok {
		return nil, fmt.Errorf("page %d not in archive", n+1)
	}
	return f.Open()
}

// ExtractPage writes page n's bytes to dst via a temp file + atomic
// rename so a concurrent reader never sees a partial file. dst's parent
// directory must already exist.
func (m *Manga) ExtractPage(n int, dst string) error {
	rc, err := m.PageReader(n)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	return fsx.WriteAtomic(dst, ".page.*", func(f *os.File) error {
		if _, err := io.Copy(f, rc); err != nil {
			return fmt.Errorf("write page %d: %w", n+1, err)
		}
		return nil
	})
}

// CoverImage decodes page 1 (entry 0 of the sorted list) into an
// image.Image suitable for the gallery thumbnail pipeline.
func (m *Manga) CoverImage() (image.Image, error) {
	rc, err := m.PageReader(0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	img, err := DecodeImageWithCap(rc)
	if err != nil {
		return nil, fmt.Errorf("decode cover: %w", err)
	}
	return img, nil
}

// CoverDimensions reads page 1's dimensions without decoding the full
// pixel buffer. Used at ingest to populate images.width/height with the
// cover's geometry.
func (m *Manga) CoverDimensions() (int, int, error) {
	rc, err := m.PageReader(0)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rc.Close() }()
	cfg, _, err := image.DecodeConfig(rc)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// PageCacheExt returns the extension to use for the n-th cached page
// file: the archive entry's lowercase extension, with a leading dot.
// Callers compose the full filename via PageCachePath.
func (m *Manga) PageCacheExt(n int) string {
	if n < 0 || n >= len(m.Pages) {
		return ""
	}
	return strings.ToLower(filepath.Ext(m.Pages[n].OriginalName))
}

// NaturalLess returns true if a < b under natural ordering: numeric
// runs compare as integers, non-numeric runs compare byte-by-byte.
// Handles `1.jpg, 2.jpg, 10.jpg` correctly while preserving the usual
// lex order on the rest. Inputs are assumed lowercased by the caller.
func NaturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ai, aj := a[i], b[j]
		ad := ai >= '0' && ai <= '9'
		bd := aj >= '0' && aj <= '9'
		if ad && bd {
			// Read numeric runs from both sides and compare as integers.
			// Strip leading zeros so "01" == "1" but break the tie by
			// choosing the longer (more leading zeros) as smaller -
			// otherwise "01" and "1" would sort equal and fall through
			// to a byte tie-break that prefers the digit's ASCII value.
			as, bs := i, j
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			as2 := as
			for as2 < i && a[as2] == '0' {
				as2++
			}
			bs2 := bs
			for bs2 < j && b[bs2] == '0' {
				bs2++
			}
			la, lb := i-as2, j-bs2
			if la != lb {
				return la < lb
			}
			if cmp := strings.Compare(a[as2:i], b[bs2:j]); cmp != 0 {
				return cmp < 0
			}
			// Equal numeric values: shorter zero-padding sorts first.
			if as != bs {
				// fewer leading zeros (longer stripped index gap) sorts first;
				// equivalent to comparing the original-run lengths.
				return (i - as) < (j - bs)
			}
			continue
		}
		if ai != aj {
			return ai < aj
		}
		i++
		j++
	}
	return len(a) < len(b)
}
