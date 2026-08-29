package gallery

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/fsx"
	"github.com/monbooru/monbooru/internal/logx"
)

const thumbMaxDim = 300
const thumbQuality = 85

// viewMaxPixels is the size past which the detail view stops handing the
// browser the original file, and ViewMaxDim the longest side of the
// rendition it hands over instead. Browsers cap a decoded bitmap at 2 GiB,
// which at four bytes a pixel is 2^29 pixels, and past that they refuse the
// decode outright - so an image above the cap is undisplayable everywhere
// with only a 300 px thumbnail between it and the file. The ceiling is a
// round number well under that measured limit rather than the limit itself,
// so it survives an engine stricter than Chromium and leaves every real
// photo and scan on its full-resolution file.
const (
	viewMaxPixels = 100_000_000
	ViewMaxDim    = 4000
)

// ViewRenditionPath is where an image's bounded display rendition is cached,
// beside its thumbnail.
func ViewRenditionPath(dir string, imageID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d_view.jpg", imageID))
}

// NeedsViewRendition reports whether an image's stored geometry is past the
// ceiling, so the caller serves the rendition rather than the file. Zero
// dimensions (a header nothing could read) answer false: without a size
// there is nothing to decide on, and the original is what every other
// unmeasurable file gets.
func NeedsViewRendition(width, height int) bool {
	return int64(width)*int64(height) > viewMaxPixels
}

// EnsureViewRendition returns the cached rendition's path, generating it
// from the original on first use. Lazy because only the rare oversized image
// needs one at all: producing it at ingest would write a second file per
// image for a ceiling almost nothing reaches.
func EnsureViewRendition(srcPath, dstDir string, imageID int64) (string, error) {
	dst := ViewRenditionPath(dstDir, imageID)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", fmt.Errorf("create rendition dir: %w", err)
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	src, err := DecodeImageWithCap(f)
	if err != nil {
		return "", fmt.Errorf("decoding image: %w", err)
	}
	if err := writeJPEGAtomic(scaleImage(src, ViewMaxDim), dst, thumbQuality); err != nil {
		return "", err
	}
	return dst, nil
}

// maxImageBytes caps the destination bitmap the decode path is willing
// to allocate, so a header claiming 50000x50000 truecolor is refused
// instead of demanding 10 GiB and OOM-killing the process at ingest or
// thumbnail regen. Budgeting bytes rather than pixels is what lets a
// 670-MPx map through at 640 MiB greyscale while a 16-bit header of the
// same geometry, four times the cost, stays out.
const maxImageBytes = 3 << 30

// decodedBytesPerPixel is the per-pixel cost of the concrete image type
// the stdlib decoders return for a header's colour model. Models not
// listed bill at 4, the truecolor width.
func decodedBytesPerPixel(m color.Model) int64 {
	switch m {
	case color.GrayModel:
		return 1
	case color.Gray16Model:
		return 2
	case color.YCbCrModel:
		return 3
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	}
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	return 4
}

// largeDecodeBytes is the decoded-bitmap size past which the thumbnail
// path hands the heap back before the next file. Below it, holding the
// spent copy costs less than forcing the collection would.
const largeDecodeBytes = 32 << 20

// decodedBytes is what a decoded bitmap occupies, the same budget
// decodeBudgetError applies to a header.
func decodedBytes(img image.Image) int64 {
	b := img.Bounds()
	return int64(b.Dx()) * int64(b.Dy()) * decodedBytesPerPixel(img.ColorModel())
}

// decodeBudgetError refuses a header whose decoded bitmap would not fit
// maxImageBytes.
func decodeBudgetError(cfg image.Config) error {
	// Nothing decodes to less than a byte per pixel, so gating on the
	// pixel count first also keeps the byte multiply from overflowing.
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if pixels > maxImageBytes || pixels*decodedBytesPerPixel(cfg.ColorModel) > maxImageBytes {
		return fmt.Errorf("image %dx%d exceeds the %d GiB decode cap", cfg.Width, cfg.Height, maxImageBytes>>30)
	}
	return nil
}

// DecodeBudgetError reports why the file at path is past the decode
// budget, or nil when it fits - and when it cannot be read or is not a
// still image, since a caller explaining a missing thumbnail has nothing
// to add in those cases. Reads the header only.
func DecodeBudgetError(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil
	}
	return decodeBudgetError(cfg)
}

// DecodeImageWithCap is image.Decode gated on maxImageBytes. Runs
// image.DecodeConfig first to read just the header, refuses any image
// whose decoded bitmap would exceed the budget, then replays the header
// bytes alongside the rest of the stream so the full Decode works on
// non-seekable readers (zip page streams). Mirrors the stdlib signature
// minus the format-name return.
func DecodeImageWithCap(r io.Reader) (image.Image, error) {
	var buf bytes.Buffer
	tee := io.TeeReader(r, &buf)
	cfg, _, err := image.DecodeConfig(tee)
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := decodeBudgetError(cfg); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(io.MultiReader(&buf, r))
	return img, err
}

func ThumbnailPath(dir string, imageID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d.jpg", imageID))
}

func HoverPath(dir string, imageID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d_hover.webp", imageID))
}

// Generate writes the static thumbnail (and animated hover for videos
// and GIFs when ffmpeg is available) for the given file under dstDir.
func Generate(srcPath, dstDir string, imageID int64, fileType string) error {
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("creating thumbnail dir: %w", err)
	}
	// Both renditions come off the same bytes, so anything that rewrites the
	// thumbnail - a replace, a re-ingest, a rebuild - leaves the display
	// rendition showing the picture the file no longer holds.
	_ = os.Remove(ViewRenditionPath(dstDir, imageID))

	dstPath := ThumbnailPath(dstDir, imageID)

	if IsVideoType(fileType) {
		if err := generateVideoThumb(srcPath, dstPath); err != nil {
			return err
		}
		hoverDst := HoverPath(dstDir, imageID)
		if err := generateVideoHover(srcPath, hoverDst); err != nil {
			logx.Warnf("hover preview for %q: %v", srcPath, err)
		}
		return nil
	}
	if fileType == "cbz" {
		return generateMangaThumbnails(srcPath, dstDir, imageID)
	}
	if err := generateImageThumb(srcPath, dstPath); err != nil {
		return err
	}
	if fileType == "gif" {
		hoverDst := HoverPath(dstDir, imageID)
		if err := generateGIFHover(srcPath, hoverDst); err != nil {
			logx.Warnf("hover preview for %q: %v", srcPath, err)
		}
	}
	return nil
}

// generateMangaThumbnails writes the cover thumbnail (`<dstDir>/<id>.jpg`)
// and hands the per-page set (`MangaImageDir/page_NNNN_thumb.jpg`) to a
// bounded background worker. The cover is the phash input, so it stays on
// the ingest path; pre-generating every page turns the first /pages render
// into a static-file serve but takes minutes on a large archive, which
// would hold the caller's phash write and cache invalidation behind it. A
// page whose thumbnail is not ready yet falls back to the lazy
// EnsureMangaPageThumb path on access.
func generateMangaThumbnails(srcPath, dstDir string, imageID int64) error {
	archive, err := OpenManga(srcPath)
	if err != nil {
		return fmt.Errorf("open manga thumb: %w", err)
	}
	defer func() { _ = archive.Close() }()

	cover, err := archive.CoverImage()
	if err != nil {
		return fmt.Errorf("decode manga cover: %w", err)
	}
	if err := writeJPEGAtomic(scaleImage(cover, thumbMaxDim), ThumbnailPath(dstDir, imageID), thumbQuality); err != nil {
		return err
	}

	imageDir := MangaImageDir(dstDir, imageID)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("create manga thumb dir: %w", err)
	}
	queueMangaPageThumbs(srcPath, imageDir)
	return nil
}

// mangaThumbWorkers caps how many archives decode their pages at once.
// The work is background-only, so it stays well under the core count to
// leave the foreground request path room on a modest host.
const mangaThumbWorkers = 2

type mangaThumbJob struct{ srcPath, imageDir string }

// mangaThumbQueue feeds the pregeneration workers. Bounded so a bulk
// ingest of thousands of archives queues small jobs instead of parking
// one goroutine per archive; an overflow skips the archive and leaves
// the lazy EnsureMangaPageThumb path to cover its pages on access.
var mangaThumbQueue = make(chan mangaThumbJob, 256)

var mangaThumbOnce sync.Once

// queueMangaPageThumbs hands the archive to the worker pool, starting
// the workers on first use. Never blocks the ingest path.
func queueMangaPageThumbs(srcPath, imageDir string) {
	mangaThumbOnce.Do(func() {
		for i := 0; i < mangaThumbWorkers; i++ {
			go func() {
				for job := range mangaThumbQueue {
					pregenerateMangaPageThumbs(job.srcPath, job.imageDir)
				}
			}()
		}
	})
	select {
	case mangaThumbQueue <- mangaThumbJob{srcPath: srcPath, imageDir: imageDir}:
	default:
	}
}

// pregenerateMangaPageThumbs writes a thumbnail for every page of the
// archive at srcPath into imageDir. It reopens the archive rather than
// borrowing the caller's so no file handle is held while it waits in
// the queue. Every page is best-effort: a failure logs and leaves the
// lazy path to regenerate it on access.
func pregenerateMangaPageThumbs(srcPath, imageDir string) {
	archive, err := OpenManga(srcPath)
	if err != nil {
		logx.Warnf("manga page thumbs for %q: %v", srcPath, err)
		return
	}
	defer func() { _ = archive.Close() }()

	for i := range archive.Pages {
		// RemoveMangaCache drops this directory when the image is deleted
		// or its bytes are replaced. Both can land mid-loop, and grinding
		// on through a long archive would burn the worker and log a
		// failure per page for a row that no longer wants them.
		if _, err := os.Stat(imageDir); err != nil {
			return
		}
		pageNum := i + 1
		thumbPath := MangaPageThumbPath(imageDir, pageNum)
		if err := generateOneMangaPageThumb(archive, i, thumbPath); err != nil {
			logx.Warnf("manga page thumb %d for %q: %v", pageNum, srcPath, err)
		}
	}
}

// generateOneMangaPageThumb decodes one page directly from the archive
// (no raw-bytes cache write) and writes the thumbnail. Keeps the
// per-page footprint to one file on disk - the raw bytes stay lazy.
func generateOneMangaPageThumb(archive *Manga, idx int, dstPath string) error {
	rc, err := archive.PageReader(idx)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	src, err := DecodeImageWithCap(rc)
	if err != nil {
		return fmt.Errorf("decode page %d: %w", idx+1, err)
	}
	return writeJPEGAtomic(scaleImage(src, thumbMaxDim), dstPath, thumbQuality)
}

func generateImageThumb(srcPath, dstPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer func() { _ = f.Close() }()

	src, err := DecodeImageWithCap(f)
	if err != nil {
		return fmt.Errorf("decoding image: %w", err)
	}
	// Asked here, not below: reading src after the release would keep the
	// bitmap alive across it.
	large := decodedBytes(src) >= largeDecodeBytes

	thumb := scaleImage(src, thumbMaxDim)
	if err := writeJPEGAtomic(thumb, dstPath, thumbQuality); err != nil {
		return err
	}
	// A spent bitmap stays resident until the heap next reaches its goal,
	// so a sync over large files holds two of them at once.
	if large {
		debug.FreeOSMemory()
	}
	return nil
}

// scaleImage scales src so its longest side is at most maxDim.
func scaleImage(src image.Image, maxDim int) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	if w <= maxDim && h <= maxDim {
		return src
	}

	var nw, nh int
	if w >= h {
		nw = maxDim
		nh = h * maxDim / w
	} else {
		nh = maxDim
		nw = w * maxDim / h
	}
	nh, nw = max(nh, 1), max(nw, 1)

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// writeJPEGAtomic encodes img as JPEG at path via a temp file + rename.
func writeJPEGAtomic(img image.Image, path string, quality int) error {
	return fsx.WriteAtomic(path, ".thumb.*", func(f *os.File) error {
		if err := jpeg.Encode(f, img, &jpeg.Options{Quality: quality}); err != nil {
			return fmt.Errorf("encoding jpeg: %w", err)
		}
		return nil
	})
}

// RegenerateDerived renders the thumbnail and, on success, the phash. Neither
// failure is fatal: a missing thumbnail is regenerated on demand, and a NULL
// phash keeps the row out of the relations system until a recompute lands
// rather than leaving a stale value behind. logCtx names the caller.
func RegenerateDerived(database *db.DB, thumbnailsPath, path string, imageID int64, fileType, logCtx string) {
	if err := Generate(path, thumbnailsPath, imageID, fileType); err != nil {
		logx.Warnf("%s: thumbnail for %q: %v", logCtx, path, err)
	} else if err := RecomputeAndStorePhash(context.Background(), database, imageID, thumbnailsPath); err != nil {
		logx.Warnf("%s: phash for %q: %v", logCtx, path, err)
	}
}
