package gallery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveImage_IntoSubfolder(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")

	_, _, err := Ingest(database, galleryDir, env.thumbnailsPath, srcPath, "png")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	res, err := MoveImage(database, galleryDir, id, "2026/april")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if res.NewFolderPath != "2026/april" {
		t.Errorf("new folder = %q, want 2026/april", res.NewFolderPath)
	}

	var canonPath, folderPath string
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath); err != nil {
		t.Fatal(err)
	}
	if folderPath != "2026/april" {
		t.Errorf("folder_path = %q, want 2026/april", folderPath)
	}
	wantPath := filepath.Join(galleryDir, "2026", "april", "original.png")
	if canonPath != wantPath {
		t.Errorf("canonical_path = %q, want %q", canonPath, wantPath)
	}

	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("file not at new path: %v", err)
	}
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Errorf("file still at old path (err=%v)", err)
	}

	var aliasPath string
	if err := database.Read.QueryRow(
		`SELECT path FROM image_paths WHERE image_id = ? AND is_canonical = 1`, id,
	).Scan(&aliasPath); err != nil {
		t.Fatal(err)
	}
	if aliasPath != wantPath {
		t.Errorf("canonical image_paths row = %q, want %q", aliasPath, wantPath)
	}
}

func TestMoveImage_FilenameCollisionAutosuffixes(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFileSize(t, galleryDir, "pic.png", 10, 10)
	if _, _, err := Ingest(database, galleryDir, env.thumbnailsPath, srcPath, "png"); err != nil {
		t.Fatalf("ingest src: %v", err)
	}
	// Pre-seed an existing distinct file at the destination with the same
	// filename so UniqueDestPath must take the `_1` branch.
	dstDir := filepath.Join(galleryDir, "dst")
	os.MkdirAll(dstDir, 0o755)
	createTestPNGFileSize(t, dstDir, "pic.png", 11, 10)

	var id int64
	database.Read.QueryRow(`SELECT id FROM images ORDER BY id LIMIT 1`).Scan(&id)

	res, err := MoveImage(database, galleryDir, id, "dst")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	want := filepath.Join(dstDir, "pic_1.png")
	if res.NewCanonicalPath != want {
		t.Errorf("new canonical = %q, want %q", res.NewCanonicalPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at auto-suffixed path: %v", err)
	}
}

func TestMoveImage_SameFolderIsNoop(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	sub := filepath.Join(galleryDir, "here")
	os.MkdirAll(sub, 0o755)
	srcPath := createTestPNGFile(t, sub, "x.png")
	if _, _, err := Ingest(database, galleryDir, env.thumbnailsPath, srcPath, "png"); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var id int64
	database.Read.QueryRow(`SELECT id FROM images`).Scan(&id)

	res, err := MoveImage(database, galleryDir, id, "here")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if res.NewCanonicalPath != srcPath {
		t.Errorf("new canonical = %q, want unchanged %q", res.NewCanonicalPath, srcPath)
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Errorf("file moved unexpectedly: %v", err)
	}
}
