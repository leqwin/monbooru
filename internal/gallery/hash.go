package gallery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/leqwin/monbooru/internal/models"
)

// ResolveSubdir validates a user-supplied folder path and returns the
// absolute destination directory under galleryPath. An empty folder
// yields the gallery root itself. Paths containing ".." or absolute
// paths are rejected so callers cannot escape the gallery root.
func ResolveSubdir(galleryPath, folder string) (string, error) {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return galleryPath, nil
	}
	// Reject absolute paths before the slash trim - otherwise "/tmp/x"
	// is trimmed to "tmp/x" and looks relative by the time IsAbs runs.
	if filepath.IsAbs(folder) {
		return "", fmt.Errorf("folder must be relative to the gallery root")
	}
	folder = strings.Trim(folder, "/\\")
	cleaned := filepath.Clean(filepath.ToSlash(folder))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("folder path may not contain ..")
	}
	abs, err := filepath.Abs(filepath.Join(galleryPath, cleaned))
	if err != nil {
		return "", err
	}
	galleryAbs, err := filepath.Abs(galleryPath)
	if err != nil {
		return "", err
	}
	if !PathInside(galleryAbs, abs) {
		return "", fmt.Errorf("folder path escapes the gallery root")
	}
	return abs, nil
}

// PathInside reports whether target resolves inside root. Both arguments
// should be cleaned and absolute; the function uses filepath.Rel so a
// sibling directory that shares a literal prefix with root (e.g.
// `/data/gallery` vs `/data/gallery_backup`) is correctly rejected.
// A target that equals root is considered inside.
func PathInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

// ErrUnsupportedType is returned when the file type is not recognized.
var ErrUnsupportedType = errors.New("unsupported file type")

// SupportedMIMETypes is the accept attribute value for file inputs, listing all
// MIME types that Monbooru can ingest.
const SupportedMIMETypes = "image/jpeg,image/png,image/webp,image/gif,video/mp4,video/webm"

// UniqueDestPath returns a path under destDir that does not currently
// exist, starting from filename and appending `_1`, `_2`, … to the
// stem on collision until a free slot is found. Mirrors the
// auto-suffix behaviour used by both the web upload form and the API
// createImage handler so two callers don't drift on rename rules.
// The first-collision check is racy (TOCTOU) but the gallery is a
// single-process write target - callers that need stronger
// guarantees should `O_CREATE|O_EXCL` themselves.
func UniqueDestPath(destDir, filename string) string {
	dst := filepath.Join(destDir, filename)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	ext := filepath.Ext(filename)
	for i := 1; ; i++ {
		candidate := filepath.Join(destDir, fmt.Sprintf("%s_%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// HashFile computes the SHA-256 of the file at path using streaming 32 KB chunks.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening file for hashing: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, 32*1024)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", fmt.Errorf("hashing file: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// DetectFileType returns the file type constant for the given path.
// It first tries extension matching, then magic bytes.
func DetectFileType(path string) (string, error) {
	// Primary: extension match (case-insensitive)
	dot := strings.LastIndex(path, ".")
	if dot >= 0 {
		ext := strings.ToLower(path[dot:])
		switch ext {
		case ".jpg", ".jpeg":
			return models.FileTypeJPEG, nil
		case ".png":
			return models.FileTypePNG, nil
		case ".webp":
			return models.FileTypeWEBP, nil
		case ".gif":
			return models.FileTypeGIF, nil
		case ".mp4":
			return models.FileTypeMP4, nil
		case ".webm":
			return models.FileTypeWEBM, nil
		}
	}

	// Fallback: magic bytes
	f, err := os.Open(path)
	if err != nil {
		return "", ErrUnsupportedType
	}
	defer f.Close()

	buf := make([]byte, 16)
	n, _ := f.Read(buf)
	buf = buf[:n]

	return detectMagic(buf)
}

func detectMagic(buf []byte) (string, error) {
	if len(buf) < 4 {
		return "", ErrUnsupportedType
	}

	// JPEG: FF D8 FF
	if buf[0] == 0xFF && buf[1] == 0xD8 && buf[2] == 0xFF {
		return models.FileTypeJPEG, nil
	}
	// PNG: 89 50 4E 47 0D 0A 1A 0A
	if len(buf) >= 8 &&
		buf[0] == 0x89 && buf[1] == 0x50 && buf[2] == 0x4E && buf[3] == 0x47 &&
		buf[4] == 0x0D && buf[5] == 0x0A && buf[6] == 0x1A && buf[7] == 0x0A {
		return models.FileTypePNG, nil
	}
	// GIF: 47 49 46 38
	if buf[0] == 0x47 && buf[1] == 0x49 && buf[2] == 0x46 && buf[3] == 0x38 {
		return models.FileTypeGIF, nil
	}
	// WEBP: 52 49 46 46 .. .. .. .. 57 45 42 50
	if len(buf) >= 12 &&
		buf[0] == 0x52 && buf[1] == 0x49 && buf[2] == 0x46 && buf[3] == 0x46 &&
		buf[8] == 0x57 && buf[9] == 0x45 && buf[10] == 0x42 && buf[11] == 0x50 {
		return models.FileTypeWEBP, nil
	}
	// MP4: ftyp box at offset 4 (66 74 79 70)
	if len(buf) >= 8 && buf[4] == 0x66 && buf[5] == 0x74 && buf[6] == 0x79 && buf[7] == 0x70 {
		return models.FileTypeMP4, nil
	}
	// WEBM: 1A 45 DF A3 (EBML header)
	if buf[0] == 0x1A && buf[1] == 0x45 && buf[2] == 0xDF && buf[3] == 0xA3 {
		return models.FileTypeWEBM, nil
	}

	return "", ErrUnsupportedType
}

// IsVideoType returns true for video file types.
func IsVideoType(fileType string) bool {
	return fileType == models.FileTypeMP4 || fileType == models.FileTypeWEBM
}
