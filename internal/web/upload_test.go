package web

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeUploadReq constructs a multipart POST /upload with the given files and
// optional folder/tags fields. Returns a request ready to hand to the mux.
func makeUploadReq(t *testing.T, srv *Server, files map[string][]byte, folder, tagsLine string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, contents := range files {
		part, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(contents)
	}
	if folder != "" {
		mw.WriteField("folder", folder)
	}
	if tagsLine != "" {
		mw.WriteField("tags", tagsLine)
	}
	mw.WriteField("_csrf", srv.csrfToken("anon"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	return req
}

// makePNGBytes returns a valid PNG of the given rgb / dimensions.
func makePNGBytes(t *testing.T, w, h int, r, g, b uint8) []byte {
	t.Helper()
	plain := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			plain.Pix[(y*w+x)*4+0] = r
			plain.Pix[(y*w+x)*4+1] = g
			plain.Pix[(y*w+x)*4+2] = b
			plain.Pix[(y*w+x)*4+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, plain); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUploadPost_IngestsSinglePNG(t *testing.T) {
	srv := newTestServer(t)
	req := makeUploadReq(t, srv, map[string][]byte{
		"shot.png": makePNGBytes(t, 16, 16, 10, 20, 30),
	}, "", "")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "1 added") {
		t.Errorf("expected '1 added' flash, got: %s", body)
	}
	// File is actually on disk in the gallery root.
	cx := srv.Active()
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "shot.png")); err != nil {
		t.Errorf("uploaded file not on disk: %v", err)
	}
	// Image row made it into the DB.
	var count int
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&count)
	if count != 1 {
		t.Errorf("image count = %d, want 1", count)
	}
}

func TestUploadPost_IngestsIntoSubfolder(t *testing.T) {
	srv := newTestServer(t)
	req := makeUploadReq(t, srv, map[string][]byte{
		"nested.png": makePNGBytes(t, 12, 12, 50, 50, 50),
	}, "2026/april", "")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cx := srv.Active()
	want := filepath.Join(cx.GalleryPath, "2026", "april", "nested.png")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at %s: %v", want, err)
	}
	var folderPath string
	cx.DB.Read.QueryRow(`SELECT folder_path FROM images LIMIT 1`).Scan(&folderPath)
	if folderPath != "2026/april" {
		t.Errorf("folder_path = %q, want 2026/april", folderPath)
	}
}

func TestUploadPost_AppliesTagsToEveryFile(t *testing.T) {
	srv := newTestServer(t)
	req := makeUploadReq(t, srv, map[string][]byte{
		"a.png": makePNGBytes(t, 8, 8, 10, 10, 10),
		"b.png": makePNGBytes(t, 8, 8, 20, 20, 20),
	}, "", "shared_tag character:charname")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "2 added") {
		t.Errorf("expected '2 added', got: %s", w.Body.String())
	}
	cx := srv.Active()
	var n int
	// Both images should share the `shared_tag` tag.
	cx.DB.Read.QueryRow(`
		SELECT COUNT(*) FROM image_tags it
		JOIN tags t ON t.id = it.tag_id WHERE t.name = 'shared_tag'
	`).Scan(&n)
	if n != 2 {
		t.Errorf("shared_tag image_tags count = %d, want 2", n)
	}
	// `charname` should live in the character category.
	var category string
	cx.DB.Read.QueryRow(`
		SELECT tc.name FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE t.name = 'charname'
	`).Scan(&category)
	if category != "character" {
		t.Errorf("charname category = %q, want character", category)
	}
}

func TestUploadPost_DuplicateFilenameAutoSuffix(t *testing.T) {
	srv := newTestServer(t)
	// First upload.
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, makeUploadReq(t, srv, map[string][]byte{
		"dup.png": makePNGBytes(t, 8, 8, 1, 1, 1),
	}, "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("first upload: %d %s", w.Code, w.Body.String())
	}
	// Second upload with same filename but different bytes.
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, makeUploadReq(t, srv, map[string][]byte{
		"dup.png": makePNGBytes(t, 8, 8, 200, 200, 200),
	}, "", ""))
	if w2.Code != http.StatusOK {
		t.Fatalf("second upload: %d %s", w2.Code, w2.Body.String())
	}

	cx := srv.Active()
	want := filepath.Join(cx.GalleryPath, "dup_1.png")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("auto-suffixed file missing: %v", err)
	}
}

func TestUploadPost_SameShaReturnsDuplicate(t *testing.T) {
	srv := newTestServer(t)
	png := makePNGBytes(t, 8, 8, 1, 1, 1)
	// Upload once.
	srv.Handler().ServeHTTP(httptest.NewRecorder(), makeUploadReq(t, srv, map[string][]byte{"x.png": png}, "", ""))
	// Upload same bytes under a different name.
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, makeUploadReq(t, srv, map[string][]byte{"y.png": png}, "", ""))
	body := w.Body.String()
	if !strings.Contains(body, "duplicate") {
		t.Errorf("expected 'duplicate' flash, got: %s", body)
	}
}

func TestUploadPost_RejectsAbsoluteFolder(t *testing.T) {
	srv := newTestServer(t)
	req := makeUploadReq(t, srv, map[string][]byte{
		"x.png": makePNGBytes(t, 8, 8, 0, 0, 0),
	}, "/etc/passwd", "")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "relative to the gallery root") {
		t.Errorf("expected absolute-path error, got: %s", body)
	}
	// No file should have been written anywhere on disk.
	cx := srv.Active()
	var count int
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&count)
	if count != 0 {
		t.Errorf("images table should be empty after rejected upload, got %d", count)
	}
}

func TestUploadPost_RejectsTraversalFolder(t *testing.T) {
	srv := newTestServer(t)
	req := makeUploadReq(t, srv, map[string][]byte{
		"x.png": makePNGBytes(t, 8, 8, 0, 0, 0),
	}, "../escape", "")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "..") {
		t.Errorf("expected '..' rejection, got: %s", body)
	}
	cx := srv.Active()
	if _, err := os.Stat(filepath.Join(filepath.Dir(cx.GalleryPath), "escape")); !os.IsNotExist(err) {
		t.Error("traversal attempt should not have created the escape directory")
	}
}

func TestUploadPost_RejectsNestedTraversal(t *testing.T) {
	srv := newTestServer(t)
	req := makeUploadReq(t, srv, map[string][]byte{
		"x.png": makePNGBytes(t, 8, 8, 0, 0, 0),
	}, "ok/../../escape", "")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "..") {
		t.Errorf("expected nested '..' rejection, got: %s", w.Body.String())
	}
}

func TestUploadPost_NoFilesRejected(t *testing.T) {
	srv := newTestServer(t)
	// Empty body but with CSRF in form data.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("_csrf", srv.csrfToken("anon"))
	mw.Close()
	req := httptest.NewRequest("POST", "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "No files selected") {
		t.Errorf("expected 'No files selected', got: %s", w.Body.String())
	}
}

func TestUploadPost_UnsupportedTypeCountsAsError(t *testing.T) {
	srv := newTestServer(t)
	req := makeUploadReq(t, srv, map[string][]byte{
		"malicious.exe": []byte("MZ\x90\x00" + strings.Repeat("\x00", 32)),
	}, "", "")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	// The handler surfaces the per-file error count in the flash message.
	if !strings.Contains(body, "error") {
		t.Errorf("expected 'error(s)' flash when file type is unsupported, got: %s", body)
	}
	// File must be cleaned up off disk too.
	cx := srv.Active()
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "malicious.exe")); !os.IsNotExist(err) {
		t.Error("unsupported-type file should be removed from disk")
	}
}

// TestUploadPost_EnforcesMaxFileSize pins the MaxBytesReader guard in
// uploadPost. Each worker's test config sets max_file_size_mb = 100 via
// newTestEnv-style helpers; here we tighten that cap and try to upload a
// bigger-than-cap image.
func TestUploadPost_EnforcesMaxFileSize(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Gallery.MaxFileSizeMB = 1
	srv.cfgMu.Unlock()

	// Build a PNG larger than 1 MB. A 1024x1024 RGBA encode lands comfortably
	// past 1 MB.
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	for i := 0; i < len(img.Pix); i++ {
		img.Pix[i] = byte(i % 251) // resist PNG deflate so the output stays large
	}
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 1024*1024 {
		t.Skipf("encoded PNG was only %d bytes; cannot exceed 1 MiB cap", buf.Len())
	}

	// Build the request directly - makeUploadReq would inline the body but
	// we want to feed the real byte slice under http.MaxBytesReader.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("files", "big.png")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(part, &buf)
	mw.WriteField("_csrf", srv.csrfToken("anon"))
	mw.Close()

	req := httptest.NewRequest("POST", "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// The MaxBytesReader makes ParseMultipartForm fail; the handler surfaces
	// "Upload too large or invalid." instead of a 413. Match whatever it
	// actually emits today but pin the contract.
	resp := w.Body.String()
	if !strings.Contains(resp, "too large") && !strings.Contains(resp, "invalid") {
		t.Errorf("oversize upload: expected rejection flash, got %d %s", w.Code, resp)
	}
	cx := srv.Active()
	if _, err := os.Stat(filepath.Join(cx.GalleryPath, "big.png")); err == nil {
		t.Error("oversize file should not have been written to disk")
	}
}
