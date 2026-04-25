package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedImportExportFixture populates the "stock" gallery with a handful of
// rows so round-trip tests have something to compare. Lives on "stock"
// (non-default, non-active) because ImportGallery rejects the active and
// default galleries.
func seedImportExportFixture(t *testing.T, srv *Server) {
	t.Helper()
	cx := srv.Get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, ingested_at)
		 VALUES ('seed-sha', 'seed.png', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	// A merged alias pair, so the export includes a canonical_tag_id link.
	alias, err := cx.TagSvc.GetOrCreateTag("cat", 1)
	if err != nil {
		t.Fatalf("create alias tag: %v", err)
	}
	canon, err := cx.TagSvc.GetOrCreateTag("feline", 1)
	if err != nil {
		t.Fatalf("create canon tag: %v", err)
	}
	if err := cx.TagSvc.MergeTags(alias.ID, canon.ID); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var imgID int64
	cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, "seed-sha").Scan(&imgID)
	if err := cx.TagSvc.AddTagToImage(imgID, canon.ID, false, nil); err != nil {
		t.Fatalf("tag image: %v", err)
	}

	// Drop one physical file into the gallery tree so zip exports have
	// something to package alongside the db.
	if err := os.WriteFile(filepath.Join(cx.GalleryPath, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExportGalleryDB_ProducesValidSQLite(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &buf); err != nil {
		t.Fatal(err)
	}
	// SQLite files start with the 16-byte magic "SQLite format 3\0".
	if !bytes.HasPrefix(buf.Bytes(), []byte("SQLite format 3")) {
		t.Errorf("exported DB missing SQLite magic prefix")
	}
	if buf.Len() < 1024 {
		t.Errorf("exported DB suspiciously small: %d bytes", buf.Len())
	}
}

func TestExportGalleryJSON_RoundTripsAliases(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	var exp galleryExport
	if err := json.Unmarshal(buf.Bytes(), &exp); err != nil {
		t.Fatalf("unmarshal: %v\nraw:\n%s", err, buf.String())
	}
	if exp.Version != galleryExportVersion {
		t.Errorf("version = %d, want %d", exp.Version, galleryExportVersion)
	}
	if len(exp.Images) != 1 {
		t.Errorf("images = %d, want 1", len(exp.Images))
	}
	// Alias row must round-trip with its canonical_tag_id populated.
	foundAlias := false
	for _, tag := range exp.Tags {
		if tag.Name == "cat" && tag.IsAlias == 1 && tag.CanonicalTagID.Valid {
			foundAlias = true
		}
	}
	if !foundAlias {
		t.Errorf("alias tag not preserved in JSON export, got:\n%s", buf.String())
	}
}

func TestImportGalleryDB_RestoresRows(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &buf); err != nil {
		t.Fatal(err)
	}

	// Wipe the stock gallery's DB by loading its snapshot back in - the
	// round-trip is the verification, not the state before vs after.
	if err := srv.ImportGallery("stock", "db", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}

	cx := srv.Get("stock")
	var n int
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&n)
	if n != 1 {
		t.Errorf("images after import = %d, want 1", n)
	}
	var aliasCount int
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE is_alias = 1`).Scan(&aliasCount)
	if aliasCount != 1 {
		t.Errorf("aliases after import = %d, want 1", aliasCount)
	}
}

func TestImportGalleryJSON_RestoresRows(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}

	cx := srv.Get("stock")
	var imgCount, aliasCount, imageTagCount int
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&imgCount)
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE is_alias = 1`).Scan(&aliasCount)
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_tags`).Scan(&imageTagCount)
	if imgCount != 1 || aliasCount != 1 || imageTagCount != 1 {
		t.Errorf("after JSON import: images=%d aliases=%d image_tags=%d, want 1/1/1",
			imgCount, aliasCount, imageTagCount)
	}
}

func TestImportGalleryArchive_RestoresImagesAndDB(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryArchive("stock", "db", &buf); err != nil {
		t.Fatal(err)
	}
	// Remove the physical file so we can tell the import put it back.
	cx := srv.Get("stock")
	target := filepath.Join(cx.GalleryPath, "hello.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if err := srv.ImportGallery("stock", "zip", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("gallery file not restored: %v", err)
	}
}

func TestImportGallery_RebasesPathsToTargetGallery(t *testing.T) {
	// The export's canonical_path points at the source gallery's root
	// (say /source/gallery). Importing into a differently-mounted target
	// (stock gallery in the multi-gallery fixture) must rewrite every
	// image path onto the target root so links don't dangle.
	srv := newMultiGalleryServer(t)

	// Seed the stock gallery with rows whose canonical_path intentionally
	// lives under a path that is NOT the stock gallery root. Export then
	// import back; rebase must pin everything under stock.
	cx := srv.Get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		 VALUES ('sha1', '/foreign/gallery/2024/foo.png', '2024', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical)
		 VALUES ((SELECT id FROM images WHERE sha256 = 'sha1'), '/foreign/gallery/2024/foo.png', 1)`,
	); err != nil {
		t.Fatalf("seed image_path: %v", err)
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	cx = srv.Get("stock")
	wantPrefix := cx.GalleryPath
	var got string
	cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE sha256 = 'sha1'`).Scan(&got)
	want := wantPrefix + "/2024/foo.png"
	if got != want {
		t.Errorf("canonical_path = %q, want %q", got, want)
	}
	var gotAlias string
	cx.DB.Read.QueryRow(
		`SELECT path FROM image_paths WHERE image_id = (SELECT id FROM images WHERE sha256 = 'sha1')`,
	).Scan(&gotAlias)
	if gotAlias != want {
		t.Errorf("image_paths.path = %q, want %q", gotAlias, want)
	}
}

func TestImportGallery_QueuesRebuildThumbsJob(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "db", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	// The seeded fixture has one image - the rebuild job kickoff is fire-
	// and-forget, so we accept either "still running" or "already completed"
	// as proof it ran.
	state := srv.jobs.Get()
	if state == nil {
		t.Fatal("no job state after import; expected rebuild-thumbs")
	}
	if state.JobType != "rebuild-thumbs" {
		t.Errorf("job type = %q, want rebuild-thumbs", state.JobType)
	}
}

func TestImportGallery_ActivatesImportedGallery(t *testing.T) {
	// Before the import the active gallery is "default"; after it finishes
	// the imported "stock" gallery becomes active so the rebuild-thumbs
	// job's job-manager lock doesn't leave the user stranded on the
	// previous gallery until the rebuild completes.
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)
	if srv.activeName != "default" {
		t.Fatalf("pre-import activeName = %q, want default", srv.activeName)
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "db", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	if srv.activeName != "stock" {
		t.Errorf("post-import activeName = %q, want stock", srv.activeName)
	}
}

// TestImportGallery_FlagsMissingFilesAfterRebase exercises the post-rebase
// is_missing pass: an exported row whose rebased path won't exist in the
// target's filesystem must come out flagged is_missing=1, mirroring how
// Sync handles a file that vanished off disk. Without this the gallery
// view would show a healthy-looking thumbnail that 404s on click.
func TestImportGallery_FlagsMissingFilesAfterRebase(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.Get("stock")
	if cx == nil {
		t.Fatal("stock gallery missing")
	}

	// Two rows: one whose basename matches a file we'll drop into the
	// stock gallery, and one whose basename is unique to the source so it
	// won't resolve on the target after rebase.
	pngBytes := makePNGBytes(t, 8, 8, 1, 2, 3)
	presentPath := filepath.Join(cx.GalleryPath, "present.png")
	if err := os.WriteFile(presentPath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		 VALUES ('present', '/foreign/gallery/present.png', '', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, ingested_at)
		 VALUES ('gone', '/foreign/gallery/gone.png', '', 'png', 10, datetime('now'))`,
	); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{"present", "gone"} {
		if _, err := cx.DB.Write.Exec(
			`INSERT INTO image_paths (image_id, path, is_canonical)
			 VALUES ((SELECT id FROM images WHERE sha256 = ?),
			         (SELECT canonical_path FROM images WHERE sha256 = ?), 1)`,
			sha, sha,
		); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryJSON("stock", &buf); err != nil {
		t.Fatal(err)
	}
	if err := srv.ImportGallery("stock", "json", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}

	cx2 := srv.Get("stock")
	rows := map[string]int{}
	r, err := cx2.DB.Read.Query(`SELECT sha256, is_missing FROM images`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var sha string
		var miss int
		if err := r.Scan(&sha, &miss); err != nil {
			t.Fatal(err)
		}
		rows[sha] = miss
	}
	if rows["present"] != 0 {
		t.Errorf("present row is_missing = %d, want 0", rows["present"])
	}
	if rows["gone"] != 1 {
		t.Errorf("gone row is_missing = %d, want 1", rows["gone"])
	}
}

func TestImportGallery_RejectsActive(t *testing.T) {
	srv := newMultiGalleryServer(t)
	var buf bytes.Buffer
	// Any bytes - we expect the ctxMu check to fire before we look at content.
	err := srv.ImportGallery("default", "db", &buf)
	if err == nil || !strings.Contains(err.Error(), "active gallery") {
		t.Errorf("expected active-gallery error, got %v", err)
	}
}

func TestExportHandler_ServesDBDownload(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/settings/galleries/stock/export?format=db", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), `filename="stock.db"`) {
		t.Errorf("missing download filename: %q", w.Header().Get("Content-Disposition"))
	}
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("SQLite format 3")) {
		t.Errorf("body missing SQLite magic prefix")
	}
}

func TestImportHandler_RoundTripsStockGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var dbBuf bytes.Buffer
	if err := srv.ExportGalleryDB("stock", &dbBuf); err != nil {
		t.Fatal(err)
	}

	body, ct := buildMultipart(t, map[string]string{
		"_csrf":        srv.csrfToken("anon"),
		"confirm_name": "stock",
	}, "file", "stock.db", dbBuf.Bytes())

	h := srv.Handler()
	req := httptest.NewRequest("POST", "/settings/galleries/stock/import", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body: %s", w.Code, w.Body.String())
	}
	// Success writes a flash-ok into the response body; the dialog's
	// client-side after-request hook closes the modal when it sees the
	// flash-ok class.
	resp := w.Body.String()
	if !strings.Contains(resp, "flash-ok") || !strings.Contains(resp, "imported") {
		t.Errorf("expected flash-ok import message, got %q", resp)
	}
}

func TestImportHandler_RejectsBadConfirmName(t *testing.T) {
	srv := newMultiGalleryServer(t)
	body, ct := buildMultipart(t, map[string]string{
		"_csrf":        srv.csrfToken("anon"),
		"confirm_name": "wrong",
	}, "file", "stock.db", []byte("whatever"))

	h := srv.Handler()
	req := httptest.NewRequest("POST", "/settings/galleries/stock/import", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "confirm") {
		t.Errorf("expected confirm-name error, got %q", w.Body.String())
	}
}

func TestSettingsRendersImportExportColumn(t *testing.T) {
	srv := newMultiGalleryServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/settings", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	checks := []string{
		`<th>Import/Export</th>`,
		`btn-gallery-export`,
		`btn-gallery-import`,
		`id="gallery-export-dialog"`,
		`id="gallery-import-dialog"`,
	}
	for _, c := range checks {
		if !strings.Contains(body, c) {
			t.Errorf("settings page missing: %q", c)
		}
	}
}

func TestExportArchive_ContainsGalleryFiles(t *testing.T) {
	srv := newMultiGalleryServer(t)
	seedImportExportFixture(t, srv)

	var buf bytes.Buffer
	if err := srv.ExportGalleryArchive("stock", "db", &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	seenDB := false
	seenFile := false
	for _, f := range zr.File {
		switch f.Name {
		case "monbooru.db":
			seenDB = true
		case "gallery/hello.txt":
			seenFile = true
		}
	}
	if !seenDB || !seenFile {
		t.Errorf("archive missing entries: db=%v file=%v (%d entries)", seenDB, seenFile, len(zr.File))
	}
}

// buildMultipart is a small helper that emits a multipart body with the given
// text fields and a single file part. Returns the body reader and the
// Content-Type header to set on the request.
func buildMultipart(t *testing.T, fields map[string]string, fileField, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile(fileField, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}
