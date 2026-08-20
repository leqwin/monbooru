package gallery

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
)

// ResolveSubdir validates a user-supplied folder path and returns the
// absolute destination directory under galleryPath. An empty folder
// yields the gallery root. Paths containing ".." or absolute paths are
// rejected so callers cannot escape the root.
func ResolveSubdir(galleryPath, folder string) (string, error) {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return galleryPath, nil
	}
	// Reject absolute paths before the slash trim, otherwise "/tmp/x"
	// becomes "tmp/x" and looks relative by the time IsAbs runs.
	if filepath.IsAbs(folder) {
		return "", fmt.Errorf("folder must be relative to the gallery root")
	}
	folder = strings.Trim(folder, "/\\")
	cleaned := filepath.Clean(filepath.ToSlash(folder))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("folder path may not contain a .. segment")
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
// should be cleaned and absolute. Uses filepath.Rel so a sibling directory
// sharing a literal prefix (`/data/gallery` vs `/data/gallery_backup`) is
// correctly rejected. A target equal to root counts as inside.
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

// ResolvedInside resolves both paths before asking PathInside, which is the
// gate every serve path runs before opening a stored file. A path that cannot
// be resolved counts as outside: this decides whether arbitrary bytes leave
// the box, so an unanswerable question is refused rather than guessed.
func ResolvedInside(root, target string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	return PathInside(rootAbs, targetAbs)
}

// ErrUnsupportedType is returned when the file type is not recognized.
var ErrUnsupportedType = errors.New("unsupported file type")

// SupportedMIMETypes is the accept attribute value for file inputs, listing all
// MIME types that Monbooru can ingest. The cbz line covers both `.cbz` and
// plain `.zip` uploads; both ingest as one manga row.
const SupportedMIMETypes = "image/jpeg,image/png,image/webp,image/gif,video/mp4,video/webm,application/vnd.comicbook+zip,application/zip,application/x-cbz"

// UniqueDestPath returns a path under destDir that does not currently
// exist, appending `_1`, `_2`, ... to the stem on collision. Shared by
// the upload form, API createImage, and merge-extract paths so the
// rename rule is consistent. The stat check is racy (TOCTOU); callers
// needing stronger guarantees should O_CREATE|O_EXCL themselves.
func UniqueDestPath(destDir, filename string) string {
	return uniquePathBy(destDir, filename, func(stem, ext string, i int) string {
		return fmt.Sprintf("%s_%d%s", stem, i, ext)
	})
}

// uniquePathBy returns dir/filename when it is free, else the first name
// nameNth produces that is. The stat check is racy (TOCTOU); callers needing
// stronger guarantees should O_CREATE|O_EXCL themselves.
func uniquePathBy(dir, filename string, nameNth func(stem, ext string, i int) string) string {
	dst := filepath.Join(dir, filename)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	for i := 1; ; i++ {
		candidate := filepath.Join(dir, nameNth(stem, ext, i))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// cancellableReader stops a read once its context is done. io.Copy has no
// cancellation of its own, and an original can be gigabytes.
type cancellableReader struct {
	ctx context.Context
	r   io.Reader
}

func (c cancellableReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// streamFile copies the file at path into w in 32 KB chunks, giving up
// between chunks when ctx is done.
func streamFile(ctx context.Context, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening file for hashing: %w", err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 32*1024)
	if _, err := io.CopyBuffer(w, cancellableReader{ctx, f}, buf); err != nil {
		return fmt.Errorf("hashing file: %w", err)
	}
	return nil
}

// hashFileWith streams the file at path through h.
func hashFileWith(ctx context.Context, path string, h hash.Hash) (string, error) {
	if err := streamFile(ctx, path, h); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashFile computes the SHA-256 of the file at path.
func HashFile(path string) (string, error) {
	return hashFileWith(context.Background(), path, sha256.New())
}

// Md5File computes the MD5 of the file at path. Boorus key their posts on
// md5; sha256 remains the content address, and md5 is never a dedup key.
func Md5File(path string) (string, error) {
	return hashFileWith(context.Background(), path, md5.New())
}

// HashFileDigests computes both stored digests of the file at path in one
// read. Every path that writes images.sha256 goes through here, so the two
// columns cannot drift apart: an md5 describing bytes the row no longer
// holds is what a later booru lookup would search for.
func HashFileDigests(path string) (sha, sum string, err error) {
	shaH, md5H := sha256.New(), md5.New()
	if err := streamFile(context.Background(), path, io.MultiWriter(shaH, md5H)); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(shaH.Sum(nil)), hex.EncodeToString(md5H.Sum(nil)), nil
}

// DetectFileType returns the file type constant for the given path,
// trying extension matching first and falling back to magic bytes.
func DetectFileType(path string) (string, error) {
	if t := ExtFileType(path); t != "" {
		return t, nil
	}
	return detectMagicType(path)
}

// ExtFileType returns the type the path's extension claims, or "" when it
// claims none. Ingest records what the bytes say, so comparing the two is
// what tells the operator a file is misnamed.
func ExtFileType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return models.FileTypeJPEG
	case ".png":
		return models.FileTypePNG
	case ".webp":
		return models.FileTypeWEBP
	case ".gif":
		return models.FileTypeGIF
	case ".mp4":
		return models.FileTypeMP4
	case ".webm":
		return models.FileTypeWEBM
	case ".cbz", ".zip":
		return models.FileTypeCBZ
	}
	return ""
}

// detectMagicType reads the file's leading bytes and returns the type
// their signature declares, or ErrUnsupportedType when they match none.
func detectMagicType(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", ErrUnsupportedType
	}
	defer func() { _ = f.Close() }()

	// Enough for the longest ftyp brand list in practice; every other
	// signature fits in 12.
	buf := make([]byte, 64)
	n, _ := io.ReadFull(f, buf)
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
	// MP4: ftyp box at offset 4 (66 74 79 70). Its brands disambiguate
	// ISO base-media containers; without the brand check `.mov`,
	// `.heic`, and old `.3gp` files would be accepted as MP4 and then
	// fail to decode in the browser.
	if len(buf) >= 12 && buf[4] == 0x66 && buf[5] == 0x74 && buf[6] == 0x79 && buf[7] == 0x70 &&
		hasMP4Brand(buf) {
		return models.FileTypeMP4, nil
	}
	// WEBM: 1A 45 DF A3 (EBML header)
	if buf[0] == 0x1A && buf[1] == 0x45 && buf[2] == 0xDF && buf[3] == 0xA3 {
		return models.FileTypeWEBM, nil
	}
	// ZIP / CBZ: PK\x03\x04 (LFH) or PK\x05\x06 (empty zip EOCD).
	// Both end up as FileTypeCBZ; an empty archive is rejected at ingest.
	if buf[0] == 0x50 && buf[1] == 0x4B &&
		(buf[2] == 0x03 && buf[3] == 0x04 || buf[2] == 0x05 && buf[3] == 0x06) {
		return models.FileTypeCBZ, nil
	}

	return "", ErrUnsupportedType
}

// hasMP4Brand reports whether an ftyp box names a brand monbooru can
// serve. The playable brand lands in compatible_brands as readily as in
// major_brand: danbooru's mp4s are major iso5 and name mp41 only there.
func hasMP4Brand(buf []byte) bool {
	// Bounded by the declared box size so the scan stops at ftyp.
	end := min(int(binary.BigEndian.Uint32(buf[:4])), len(buf))
	for off := 8; off+4 <= end; off += 4 {
		if off == 12 {
			continue // minor_version
		}
		switch string(buf[off : off+4]) {
		case "mp42", "mp41", "isom", "iso2", "avc1":
			return true
		}
	}
	return false
}

// IsVideoType returns true for video file types.
func IsVideoType(fileType string) bool {
	return fileType == models.FileTypeMP4 || fileType == models.FileTypeWEBM
}

// ExtForFileType returns the extension a file of this type is named with,
// or "" when unmapped. Only for files monbooru names itself; an
// operator's own file keeps the name they gave it.
func ExtForFileType(fileType string) string {
	return fileTypeMeta[fileType].ext
}

// MIMEForFileType maps a stored file type to the media type to serve it
// under, or "" when unmapped. Handlers set this explicitly because
// http.ServeFile answers from the extension, which the bytes can
// contradict.
func MIMEForFileType(fileType string) string {
	return fileTypeMeta[fileType].mime
}

// fileTypeMeta names each stored file type on disk and on the wire. An
// unmapped type reads as the zero value, which both accessors report as "".
var fileTypeMeta = map[string]struct{ ext, mime string }{
	models.FileTypeJPEG: {".jpg", "image/jpeg"},
	models.FileTypePNG:  {".png", "image/png"},
	models.FileTypeWEBP: {".webp", "image/webp"},
	models.FileTypeGIF:  {".gif", "image/gif"},
	models.FileTypeMP4:  {".mp4", "video/mp4"},
	models.FileTypeWEBM: {".webm", "video/webm"},
	// CBZ serves as what the stdlib sniffer already answers for a PK archive.
	models.FileTypeCBZ: {".cbz", "application/zip"},
}

// ContentDispositionFor names a download after the file on disk. The byte
// routes end in an extensionless segment, so a browser left to itself names
// the download from the Content-Type, which cannot tell a .cbz from a .zip;
// inline keeps the same URL usable as an <img>/<video> src.
func ContentDispositionFor(canonPath string) string {
	return mime.FormatMediaType("inline", map[string]string{"filename": filepath.Base(canonPath)})
}
