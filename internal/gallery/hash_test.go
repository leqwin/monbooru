package gallery

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestHashFile(t *testing.T) {
	content := []byte("hello monbooru")
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, content, 0644)

	want := sha256.Sum256(content)
	wantHex := hex.EncodeToString(want[:])

	got, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantHex {
		t.Errorf("hash = %q, want %q", got, wantHex)
	}
}

func TestDetectFileType_Extensions(t *testing.T) {
	tests := []struct {
		name     string
		wantType string
	}{
		{"image.jpg", "jpeg"},
		{"image.jpeg", "jpeg"},
		{"IMAGE.JPG", "jpeg"},
		{"image.png", "png"},
		{"image.webp", "webp"},
		{"image.gif", "gif"},
		{"video.mp4", "mp4"},
		{"video.webm", "webm"},
	}
	for _, tt := range tests {
		got, err := DetectFileType(tt.name)
		if err != nil {
			t.Errorf("DetectFileType(%q) error: %v", tt.name, err)
			continue
		}
		if got != tt.wantType {
			t.Errorf("DetectFileType(%q) = %q, want %q", tt.name, got, tt.wantType)
		}
	}
}

func TestDetectFileType_Unknown(t *testing.T) {
	_, err := DetectFileType("file.xyz")
	if err != ErrUnsupportedType {
		t.Errorf("expected ErrUnsupportedType, got %v", err)
	}
}

func TestDetectMagic_PNG(t *testing.T) {
	buf := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	got, err := detectMagic(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "png" {
		t.Errorf("got %q, want png", got)
	}
}

func TestDetectMagic_JPEG(t *testing.T) {
	buf := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}
	got, err := detectMagic(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "jpeg" {
		t.Errorf("got %q, want jpeg", got)
	}
}

func TestDetectMagic_GIF(t *testing.T) {
	buf := []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0, 0, 0, 0, 0, 0}
	got, err := detectMagic(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "gif" {
		t.Errorf("got %q, want gif", got)
	}
}

func TestDetectMagic_WEBP(t *testing.T) {
	buf := []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50}
	got, err := detectMagic(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "webp" {
		t.Errorf("got %q, want webp", got)
	}
}

func TestDetectMagic_MP4(t *testing.T) {
	// ftyp box at offset 4
	buf := []byte{0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70, 0x00, 0x00, 0x00, 0x00, 0, 0, 0, 0}
	got, err := detectMagic(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "mp4" {
		t.Errorf("got %q, want mp4", got)
	}
}

func TestDetectMagic_WEBM(t *testing.T) {
	// EBML header: 1A 45 DF A3
	buf := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x00, 0x00, 0x00, 0x00, 0, 0, 0, 0}
	got, err := detectMagic(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != "webm" {
		t.Errorf("got %q, want webm", got)
	}
}

func TestDetectMagic_Unknown(t *testing.T) {
	buf := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0, 0, 0, 0}
	_, err := detectMagic(buf)
	if err != ErrUnsupportedType {
		t.Errorf("expected ErrUnsupportedType, got %v", err)
	}
}

func TestDetectMagic_TooShort(t *testing.T) {
	buf := []byte{0xFF, 0xD8}
	_, err := detectMagic(buf)
	if err != ErrUnsupportedType {
		t.Errorf("expected ErrUnsupportedType for short buf, got %v", err)
	}
}

func TestHashFile_NonExistent(t *testing.T) {
	_, err := HashFile("/nonexistent/path/file.bin")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestDetectFileType_MagicFallback(t *testing.T) {
	// File with no extension - should try magic bytes
	dir := t.TempDir()
	path := dir + "/noext"
	// Write JPEG magic bytes
	os.WriteFile(path, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}, 0644)
	got, err := DetectFileType(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "jpeg" {
		t.Errorf("magic fallback: got %q, want jpeg", got)
	}
}

func TestIsVideoType(t *testing.T) {
	if !IsVideoType("mp4") {
		t.Error("mp4 should be video")
	}
	if !IsVideoType("webm") {
		t.Error("webm should be video")
	}
	if IsVideoType("png") {
		t.Error("png should not be video")
	}
}

func TestUniqueDestPath_NoCollision(t *testing.T) {
	dir := t.TempDir()
	got := UniqueDestPath(dir, "fresh.png")
	want := filepath.Join(dir, "fresh.png")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestUniqueDestPath_SuffixesOnCollision(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"image.png", "image_1.png", "image_2.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := UniqueDestPath(dir, "image.png")
	want := filepath.Join(dir, "image_3.png")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestUniqueDestPath_PreservesExtension(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "shot.tar.gz"), nil, 0o644)
	got := UniqueDestPath(dir, "shot.tar.gz")
	want := filepath.Join(dir, "shot.tar_1.gz")
	if got != want {
		t.Errorf("got %q, want %q (only the last extension is preserved)", got, want)
	}
}
