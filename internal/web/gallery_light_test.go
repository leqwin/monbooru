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

// TestExportGalleryLight_RoundTripsImages exports `stock` as a light .zip,
// inspects the manifest, and confirms the gallery files are bundled.
func TestExportGalleryLight_RoundTripsImages(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.Get("stock")

	// Drop a real PNG into the gallery and ingest it so the manifest has
	// something concrete to emit.
	pngBytes := makePNGBytes(t, 8, 8, 7, 8, 9)
	imgPath := filepath.Join(cx.GalleryPath, "subdir/light.png")
	if err := os.MkdirAll(filepath.Dir(imgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgPath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	img, _, err := galleryIngest(srv, "stock", imgPath, "png")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := cx.TagSvc.GetOrCreateTag("light_tag", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.AddTagToImage(img.ID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryLight("stock", &buf); err != nil {
		t.Fatalf("ExportGalleryLight: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	var sawManifest, sawImage bool
	var manifest lightManifest
	for _, f := range zr.File {
		switch f.Name {
		case "tags.json":
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			if err := json.Unmarshal(b, &manifest); err != nil {
				t.Fatalf("manifest unmarshal: %v", err)
			}
			sawManifest = true
		case "gallery/subdir/light.png":
			sawImage = true
		}
	}
	if !sawManifest {
		t.Fatal("light export missing tags.json")
	}
	if !sawImage {
		t.Fatal("light export missing gallery/subdir/light.png")
	}
	if len(manifest.Images) != 1 {
		t.Fatalf("manifest images = %d, want 1", len(manifest.Images))
	}
	if got, want := manifest.Images[0].Path, "subdir/light.png"; got != want {
		t.Errorf("manifest path = %q, want %q", got, want)
	}
	if len(manifest.Images[0].Tags) != 1 || manifest.Images[0].Tags[0] != "light_tag" {
		t.Errorf("manifest tags = %v, want [light_tag]", manifest.Images[0].Tags)
	}
}

// TestImportGalleryLightZip_WipesAndRebuilds exports stock then imports the
// resulting .zip back into stock through the HTTP handler. Verifies the
// destination gallery is wiped and the manifest's tags are reattached.
func TestImportGalleryLightZip_WipesAndRebuilds(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.Get("stock")

	// Two distinct images so we can make sure the wipe really happens.
	for i, name := range []string{"a.png", "b.png"} {
		bytesPng := makePNGBytes(t, 16, 16, byte(i*30), byte(i*30+10), byte(i*30+20))
		p := filepath.Join(cx.GalleryPath, name)
		if err := os.WriteFile(p, bytesPng, 0o644); err != nil {
			t.Fatal(err)
		}
		img, _, err := galleryIngest(srv, "stock", p, "png")
		if err != nil {
			t.Fatal(err)
		}
		tag, err := cx.TagSvc.GetOrCreateTag("orig_tag", 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := cx.TagSvc.AddTagToImage(img.ID, tag.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Export.
	var exportBuf bytes.Buffer
	if err := srv.ExportGalleryLight("stock", &exportBuf); err != nil {
		t.Fatalf("ExportGalleryLight: %v", err)
	}

	// Drop a stray file that the wipe should remove. Demote default first
	// so stock can be imported into.
	if err := srv.SetDefault("stock"); err != nil {
		t.Fatal(err)
	}
	// stock cannot be both default and active for import; ImportGallery
	// rejects active and default. Switch active to the other gallery and
	// keep default on the other gallery too.
	if err := srv.SwitchGallery("default"); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetDefault("default"); err != nil {
		t.Fatal(err)
	}
	strayPath := filepath.Join(cx.GalleryPath, "stray.txt")
	if err := os.WriteFile(strayPath, []byte("should be wiped"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Import via the HTTP handler so the full request path is exercised
	// (CSRF, multipart, format detection from extension).
	req := makeImportReq(t, srv, "stock", "stock-light.zip", exportBuf.Bytes())
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("import handler status = %d, body = %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "flash-ok") {
		t.Errorf("import response missing flash-ok: %s", w.Body.String())
	}

	// Stray file gone.
	if _, err := os.Stat(strayPath); !os.IsNotExist(err) {
		t.Errorf("stray.txt should have been wiped, stat err: %v", err)
	}

	// Both PNGs back, with their tags.
	cx2 := srv.Get("stock")
	var n int
	if err := cx2.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("after light import: %d images, want 2", n)
	}
	rows, err := cx2.DB.Read.Query(`
		SELECT t.name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		ORDER BY t.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	tagNames := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		tagNames = append(tagNames, s)
	}
	if len(tagNames) != 2 {
		t.Errorf("expected 2 image_tag rows back, got %v", tagNames)
	}
	for _, n := range tagNames {
		if n != "orig_tag" {
			t.Errorf("unexpected tag name %q", n)
		}
	}
}

// TestExportGalleryLightManifest_StreamsTagsJSON exercises the no-zip
// variant of the light export: the writer should receive the bare tags.json
// document with the manifest schema, no zip framing.
func TestExportGalleryLightManifest_StreamsTagsJSON(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.Get("stock")

	pngBytes := makePNGBytes(t, 8, 8, 5, 6, 7)
	imgPath := filepath.Join(cx.GalleryPath, "lone.png")
	if err := os.WriteFile(imgPath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	img, _, err := galleryIngest(srv, "stock", imgPath, "png")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := cx.TagSvc.GetOrCreateTag("solo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.AddTagToImage(img.ID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := srv.ExportGalleryLightManifest("stock", &buf); err != nil {
		t.Fatalf("ExportGalleryLightManifest: %v", err)
	}
	var manifest lightManifest
	if err := json.Unmarshal(buf.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v (body=%q)", err, buf.String())
	}
	if manifest.Version != galleryExportVersion {
		t.Errorf("manifest version = %d, want %d", manifest.Version, galleryExportVersion)
	}
	if len(manifest.Images) != 1 {
		t.Fatalf("manifest images = %d, want 1", len(manifest.Images))
	}
	if got, want := manifest.Images[0].Path, "lone.png"; got != want {
		t.Errorf("manifest path = %q, want %q", got, want)
	}
	if len(manifest.Images[0].Tags) != 1 || manifest.Images[0].Tags[0] != "solo" {
		t.Errorf("manifest tags = %v, want [solo]", manifest.Images[0].Tags)
	}
}

// TestImportLightJSON_ReplaceRebuildsFromOnDiskFiles exports a gallery as a
// light manifest, then replace-imports the bare tags.json into a target
// whose on-disk files mirror the source. The db should be wiped and rebuilt
// so each manifest entry resolves to its on-disk file and gets its tags
// reattached. Manifest entries with no matching file on disk are recorded
// as is_missing=1 rows so the manifest's tags survive and the user can
// surface the gap with the missing:true filter.
func TestImportLightJSON_ReplaceRebuildsFromOnDiskFiles(t *testing.T) {
	srv := newMultiGalleryServer(t)
	cx := srv.Get("stock")

	// One image on disk + one entry in the manifest that doesn't match
	// anything on disk; both should land in the new db, the second flagged
	// as missing.
	keepBytes := makePNGBytes(t, 16, 16, 11, 22, 33)
	keepPath := filepath.Join(cx.GalleryPath, "keep.png")
	if err := os.WriteFile(keepPath, keepBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	keepImg, _, err := galleryIngest(srv, "stock", keepPath, "png")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := cx.TagSvc.GetOrCreateTag("kept_tag", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.AddTagToImage(keepImg.ID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	// Build a manifest that references the keep file plus a phantom path.
	manifest := lightManifest{
		Version: galleryExportVersion,
		Images: []lightManifestImage{
			{SHA256: keepImg.SHA256, Path: "keep.png", Tags: []string{"kept_tag"}},
			{SHA256: "deadbeef", Path: "missing/from/disk.png", Tags: []string{"phantom_tag"}},
		},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	// stock cannot be active or default at import time; defer to the same
	// reshuffle the zip test uses.
	if err := srv.SetDefault("stock"); err != nil {
		t.Fatal(err)
	}
	if err := srv.SwitchGallery("default"); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetDefault("default"); err != nil {
		t.Fatal(err)
	}

	req := makeImportReq(t, srv, "stock", "stock-light.json", manifestJSON)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("import handler status = %d, body = %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "flash-ok") {
		t.Errorf("import response missing flash-ok: %s", w.Body.String())
	}

	cx2 := srv.Get("stock")
	var n int
	if err := cx2.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("after light-json replace: %d images, want 2 (kept + missing)", n)
	}
	var missing int
	if err := cx2.DB.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE is_missing = 1`,
	).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 1 {
		t.Errorf("expected 1 row flagged is_missing=1, got %d", missing)
	}
	rows, err := cx2.DB.Read.Query(`
		SELECT t.name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		ORDER BY t.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tagNames []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		tagNames = append(tagNames, s)
	}
	want := map[string]bool{"kept_tag": false, "phantom_tag": false}
	for _, n := range tagNames {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("expected tag %q after light-json replace, got %v", k, tagNames)
		}
	}
}

// TestMergeGalleryLightJSON_AppliesTagsBySha covers the merge path for a
// bare tags.json: tags should be applied onto target images that match by
// sha256, and entries with no match should be silently skipped.
func TestMergeGalleryLightJSON_AppliesTagsBySha(t *testing.T) {
	srv := newMultiGalleryServer(t)
	imgID, sha, _ := seedMergeTarget(t, srv, "stock")

	manifest := lightManifest{
		Version: galleryExportVersion,
		Images: []lightManifestImage{
			{SHA256: sha, Path: "ignored.png", Tags: []string{"merged_via_json", "artist:painter"}},
			{SHA256: "deadbeefnomatch", Path: "wherever.png", Tags: []string{"phantom"}},
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	if err := srv.MergeGallery("stock", "json", bytes.NewReader(body)); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}
	got := imageTagNames(t, srv, "stock", imgID)
	wantSeen := map[string]bool{"merged_via_json": false, "painter": false}
	for _, n := range got {
		if _, ok := wantSeen[n]; ok {
			wantSeen[n] = true
		}
		if n == "phantom" {
			t.Errorf("phantom tag should not have been applied (no sha match), got %v", got)
		}
	}
	for k, seen := range wantSeen {
		if !seen {
			t.Errorf("expected tag %q after light-json merge, got %v", k, got)
		}
	}
}

// makeImportReq builds an /settings/galleries/{name}/import multipart POST
// with mode=replace and the uploaded file.
func makeImportReq(t *testing.T, srv *Server, gallery, filename string, body []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("_csrf", srv.csrfToken("anon")); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("mode", "replace"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("confirm_name", gallery); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/settings/galleries/"+gallery+"/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	return req
}
