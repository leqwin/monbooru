package gallery

import (
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/leqwin/monbooru/internal/logx"
)

const thumbMaxDim = 300
const thumbQuality = 85

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
	if err := generateImageThumb(srcPath, dstPath, fileType); err != nil {
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

func generateImageThumb(srcPath, dstPath, fileType string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer f.Close()

	var src image.Image

	if fileType == "gif" {
		g, err := gif.Decode(f)
		if err != nil {
			return fmt.Errorf("decoding gif: %w", err)
		}
		src = g
	} else {
		img, _, err := image.Decode(f)
		if err != nil {
			return fmt.Errorf("decoding image: %w", err)
		}
		src = img
	}

	thumb := scaleImage(src, thumbMaxDim)
	return writeJPEGAtomic(thumb, dstPath, thumbQuality)
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
	if nh == 0 {
		nh = 1
	}
	if nw == 0 {
		nw = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// writeJPEGAtomic encodes img as JPEG at path via a temp file + rename.
func writeJPEGAtomic(img image.Image, path string, quality int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".thumb.*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	if err := jpeg.Encode(tmp, img, &jpeg.Options{Quality: quality}); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("encoding jpeg: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}
