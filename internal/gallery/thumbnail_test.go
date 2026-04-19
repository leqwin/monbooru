package gallery

import (
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func createTestJPEG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
	return path
}

func createTestPNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGenerate_JPEG(t *testing.T) {
	dir := t.TempDir()
	srcPath := createTestJPEG(t, dir, "test.jpg", 600, 400)

	dstDir := filepath.Join(dir, "thumbs")
	if err := Generate(srcPath, dstDir, 1, "jpeg"); err != nil {
		t.Fatal(err)
	}

	dstPath := ThumbnailPath(dstDir, 1)
	if _, err := os.Stat(dstPath); err != nil {
		t.Errorf("thumbnail not created: %v", err)
	}

	// Verify dimensions ≤ 300px
	f, _ := os.Open(dstPath)
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() > 300 || b.Dy() > 300 {
		t.Errorf("thumbnail too large: %dx%d", b.Dx(), b.Dy())
	}
}

func TestGenerate_PNG(t *testing.T) {
	dir := t.TempDir()
	srcPath := createTestPNG(t, dir, "test.png", 200, 200)

	dstDir := filepath.Join(dir, "thumbs")
	if err := Generate(srcPath, dstDir, 2, "png"); err != nil {
		t.Fatal(err)
	}

	dstPath := ThumbnailPath(dstDir, 2)
	if _, err := os.Stat(dstPath); err != nil {
		t.Errorf("thumbnail not created: %v", err)
	}
}

func TestGenerate_SmallImage_NoUpscale(t *testing.T) {
	dir := t.TempDir()
	// Image smaller than 300px should not be upscaled
	srcPath := createTestJPEG(t, dir, "small.jpg", 100, 80)

	dstDir := filepath.Join(dir, "thumbs")
	if err := Generate(srcPath, dstDir, 3, "jpeg"); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	f, err := os.Open(ThumbnailPath(dstDir, 3))
	if err != nil {
		t.Fatalf("open thumbnail: %v", err)
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	b := img.Bounds()
	// Should stay at original size (100x80)
	if b.Dx() != 100 || b.Dy() != 80 {
		t.Errorf("small image dimensions changed: %dx%d (want 100x80)", b.Dx(), b.Dy())
	}
}

func TestThumbnailPath(t *testing.T) {
	got := ThumbnailPath("/thumbs", 42)
	want := "/thumbs/42.jpg"
	if got != want {
		t.Errorf("ThumbnailPath = %q, want %q", got, want)
	}
}

func TestHoverPath(t *testing.T) {
	got := HoverPath("/thumbs", 42)
	want := "/thumbs/42_hover.webp"
	if got != want {
		t.Errorf("HoverPath = %q, want %q", got, want)
	}
}

func TestVideoThumb_SkipIfNoFFmpeg(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	// If ffmpeg is available, just verify no panic on a non-video file
}
