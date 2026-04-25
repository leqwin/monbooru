package web

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/models"
)

// sha256OfBytes returns the lowercase hex SHA-256 of b. Mirrors the digest
// monbooru stores on images.sha256 so light-archive manifests can be
// constructed in-test and matched against ingested rows.
func sha256OfBytes(t *testing.T, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// seedMergeTarget puts one image into the target gallery so merge tests can
// distinguish "tags-only on existing image" from "ingest new image".
// Returns the seeded image's id, sha256, and on-disk path.
func seedMergeTarget(t *testing.T, srv *Server, gallery string) (int64, string, string) {
	t.Helper()
	cx := srv.Get(gallery)
	if cx == nil {
		t.Fatalf("gallery %q missing", gallery)
	}
	pngBytes := makePNGBytes(t, 8, 8, 1, 2, 3)
	imgPath := filepath.Join(cx.GalleryPath, "seed.png")
	if err := os.WriteFile(imgPath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	img, _, err := galleryIngest(srv, gallery, imgPath, "png")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return img.ID, img.SHA256, imgPath
}

// galleryIngest is a small wrapper around gallery.Ingest pinned to one of
// the server's gallery contexts. Keeps the test bodies short.
func galleryIngest(srv *Server, name, path, fileType string) (*models.Image, bool, error) {
	cx := srv.Get(name)
	return gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, path, fileType, models.OriginIngest)
}

// makeUniquePNG returns a PNG that hashes to a different sha256 than the
// seed image (different pixel values).
func makeUniquePNG(t *testing.T, marker uint8) []byte {
	t.Helper()
	w, h := 12, 12
	plain := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			plain.Pix[(y*w+x)*4+0] = marker
			plain.Pix[(y*w+x)*4+1] = 200
			plain.Pix[(y*w+x)*4+2] = 50
			plain.Pix[(y*w+x)*4+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, plain); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// imageTagNames returns every tag name attached to imgID, joined by "|"
// for stable assertions.
func imageTagNames(t *testing.T, srv *Server, gallery string, imgID int64) []string {
	t.Helper()
	cx := srv.Get(gallery)
	rows, err := cx.DB.Read.Query(`
		SELECT t.name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		WHERE it.image_id = ? ORDER BY t.name`, imgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestMergeGallery_JSON_ApplyTagsOnExistingImage(t *testing.T) {
	srv := newMultiGalleryServer(t)
	imgID, sha, _ := seedMergeTarget(t, srv, "stock")

	// Build a JSON manifest that references the existing sha256 with extra tags.
	exp := galleryExport{
		Version:     galleryExportVersion,
		GalleryName: "anywhere",
		TagCategories: []tagCategoryRow{{
			ID: 1, Name: "general", Color: "#3d90e3", IsBuiltin: 1,
		}},
		Tags: []tagRow{
			{ID: 100, Name: "merged_tag", CategoryID: 1},
		},
		Images: []imageRow{
			{ID: 999, SHA256: sha, CanonicalPath: "/elsewhere/seed.png", FolderPath: "", FileType: "png", FileSize: 8, IngestedAt: "2026-01-01T00:00:00Z", SourceType: "none", Origin: "ingest"},
		},
		ImageTags: []imageTagRow{
			{ImageID: 999, TagID: 100, IsAuto: 0, CreatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	body, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}

	if err := srv.MergeGallery("stock", "json", bytes.NewReader(body)); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}

	tags := imageTagNames(t, srv, "stock", imgID)
	found := false
	for _, n := range tags {
		if n == "merged_tag" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected merged_tag on image %d after merge, got %v", imgID, tags)
	}
}

func TestMergeGallery_DB_ApplyTagsOnExistingImage(t *testing.T) {
	srv := newMultiGalleryServer(t)
	// Seed *both* the source we'll export and the target we'll merge into.
	// The default gallery is the source; we export it then merge into stock.
	cx := srv.Get("default")
	if cx == nil {
		t.Fatal("default gallery missing")
	}
	pngBytes := makePNGBytes(t, 8, 8, 11, 22, 33)
	srcPath := filepath.Join(cx.GalleryPath, "src.png")
	if err := os.WriteFile(srcPath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	srcImg, _, err := galleryIngest(srv, "default", srcPath, "png")
	if err != nil {
		t.Fatal(err)
	}
	gen, err := cx.TagSvc.GetOrCreateTag("from_db_merge", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.AddTagToImage(srcImg.ID, gen.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	// Drop a same-content file into the target so SHA matches, then merge.
	stockCx := srv.Get("stock")
	stockPath := filepath.Join(stockCx.GalleryPath, "src.png")
	if err := os.WriteFile(stockPath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	tgtImg, _, err := galleryIngest(srv, "stock", stockPath, "png")
	if err != nil {
		t.Fatal(err)
	}

	// Export the source as a .db snapshot, then merge into stock.
	var snap bytes.Buffer
	if err := srv.ExportGalleryDB("default", &snap); err != nil {
		t.Fatal(err)
	}
	if err := srv.MergeGallery("stock", "db", &snap); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}

	got := imageTagNames(t, srv, "stock", tgtImg.ID)
	found := false
	for _, n := range got {
		if n == "from_db_merge" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected from_db_merge on image %d after .db merge, got %v", tgtImg.ID, got)
	}
}

func TestMergeGallery_Zip_IngestsNewImage(t *testing.T) {
	srv := newMultiGalleryServer(t)
	// Pre-seed stock with one image so the test can assert that merge
	// adds a *new* one without disturbing the existing row.
	existingID, _, _ := seedMergeTarget(t, srv, "stock")

	// Build a light-format zip in memory: tags.json manifest + gallery/<file>.
	imgBytes := makeUniquePNG(t, 77)
	manifest := lightManifest{
		Version: galleryExportVersion,
		Images: []lightManifestImage{{
			SHA256: sha256OfBytes(t, imgBytes),
			Path:   "merged_subdir/new.png",
			Tags:   []string{"new_tag", "general:other_tag"},
		}},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	if w, err := zw.Create("tags.json"); err != nil {
		t.Fatal(err)
	} else if _, err := w.Write(manifestJSON); err != nil {
		t.Fatal(err)
	}
	if w, err := zw.Create("gallery/merged_subdir/new.png"); err != nil {
		t.Fatal(err)
	} else if _, err := w.Write(imgBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := srv.MergeGallery("stock", "zip", bytes.NewReader(zipBuf.Bytes())); err != nil {
		t.Fatalf("MergeGallery: %v", err)
	}

	// Existing image untouched.
	cx := srv.Get("stock")
	var n int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 images after zip merge, got %d", n)
	}
	// New image carries its tags.
	var newID int64
	if err := cx.DB.Read.QueryRow(
		`SELECT id FROM images WHERE id != ?`, existingID,
	).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	got := imageTagNames(t, srv, "stock", newID)
	if len(got) != 2 {
		t.Errorf("expected 2 tags on merged image, got %v", got)
	}
}

func TestMergeGallery_Zip_RejectsTraversalEntry(t *testing.T) {
	srv := newMultiGalleryServer(t)
	// Build a manifest pointing at an entry outside `gallery/`.
	manifest := lightManifest{
		Version: galleryExportVersion,
		Images: []lightManifestImage{{
			SHA256: "doesnotmatter",
			Path:   "../escape.png",
			Tags:   []string{"trying_to_escape"},
		}},
	}
	manifestJSON, _ := json.Marshal(manifest)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	if w, _ := zw.Create("tags.json"); w != nil {
		_, _ = w.Write(manifestJSON)
	}
	zw.Close()

	// Merging shouldn't blow up; it should just skip the unsafe entry.
	if err := srv.MergeGallery("stock", "zip", bytes.NewReader(zipBuf.Bytes())); err != nil {
		t.Fatalf("MergeGallery should skip traversal entries silently, got: %v", err)
	}
	cx := srv.Get("stock")
	var n int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected no images ingested from a traversal-only manifest, got %d", n)
	}
}
