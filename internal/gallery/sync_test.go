package gallery

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/db"
)

// syncEnv is the per-test bundle of paths and limits used by the refactored
// gallery signatures.
type syncEnv struct {
	galleryPath    string
	thumbnailsPath string
	maxFileSizeMB  int
}

func setupSyncTest(t *testing.T) (*db.DB, *syncEnv, string) {
	t.Helper()
	tmpDir := t.TempDir()
	galleryDir := filepath.Join(tmpDir, "gallery")
	os.MkdirAll(galleryDir, 0755)
	thumbDir := filepath.Join(tmpDir, "thumbs")
	os.MkdirAll(thumbDir, 0755)

	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	env := &syncEnv{
		galleryPath:    galleryDir,
		thumbnailsPath: thumbDir,
		maxFileSizeMB:  100,
	}
	return database, env, galleryDir
}

func (e *syncEnv) sync(t *testing.T, database *db.DB) SyncResult {
	t.Helper()
	r, err := Sync(context.Background(), database, e.galleryPath, e.thumbnailsPath, e.maxFileSizeMB, func(int, int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func createTestPNGFile(t *testing.T, dir, name string) string {
	t.Helper()
	return createTestPNGFileSize(t, dir, name, 10, 10)
}

func createTestPNGFileSize(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, img)
	return path
}

func TestSync_NewFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "test.png")

	result := env.sync(t, database)
	if result.Added != 1 {
		t.Errorf("Added = %d, want 1", result.Added)
	}
}

func TestSync_NoChange(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "test.png")

	env.sync(t, database)
	result := env.sync(t, database)
	if result.Added != 0 || result.Removed != 0 {
		t.Errorf("expected no changes, got Added=%d Removed=%d", result.Added, result.Removed)
	}
}

func TestSync_FileDeleted(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "test.png")

	env.sync(t, database)
	os.Remove(path)
	result := env.sync(t, database)
	if result.Removed != 1 {
		t.Errorf("Removed = %d, want 1", result.Removed)
	}

	var isMissing int
	database.Read.QueryRow(`SELECT is_missing FROM images`).Scan(&isMissing)
	if isMissing != 1 {
		t.Error("image not marked as missing")
	}
}

func TestSync_Duplicate(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	subDir := filepath.Join(galleryDir, "sub")
	os.MkdirAll(subDir, 0755)

	original := createTestPNGFile(t, galleryDir, "original.png")
	content, _ := os.ReadFile(original)
	os.WriteFile(filepath.Join(subDir, "copy.png"), content, 0644)

	result := env.sync(t, database)
	if result.Added != 1 {
		t.Errorf("Added = %d, want 1", result.Added)
	}
	if result.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1", result.Duplicates)
	}
}

func TestSync_SkipsLargeFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	// Build a >1 MiB PNG so a 1 MiB cap leaves it out. Random pixel data
	// defeats deflate so the encoded size exceeds the cap.
	big := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	for i := 0; i < len(big.Pix); i++ {
		big.Pix[i] = byte(i * 131 % 251)
	}
	bigPath := filepath.Join(galleryDir, "too_big.png")
	bf, err := os.Create(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(bf, big); err != nil {
		bf.Close()
		t.Fatal(err)
	}
	bf.Close()
	if fi, err := os.Stat(bigPath); err != nil || fi.Size() <= 1024*1024 {
		t.Skipf("big PNG unexpectedly compressed below 1 MiB (%d bytes); cannot exercise cap", fi.Size())
	}

	// Also drop a tiny file that should always ingest regardless of cap.
	createTestPNGFileSize(t, galleryDir, "small.png", 10, 10)

	// 1 MiB cap → big is skipped, small ingests.
	env.maxFileSizeMB = 1
	r1 := env.sync(t, database)
	if r1.Added != 1 {
		t.Errorf("with cap=1 MiB expected 1 added (small), got %d", r1.Added)
	}
	// Raise the cap; big now ingests, small stays.
	env.maxFileSizeMB = 100
	r2 := env.sync(t, database)
	if r2.Added != 1 {
		t.Errorf("after raising cap expected 1 new add (big), got %d", r2.Added)
	}
}

func TestFolderPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		galleryPath string
		filePath    string
		want        string
	}{
		{"root", "/gallery", "/gallery/image.png", ""},
		{"nested", "/gallery", "/gallery/2024/jan/x.png", "2024/jan"},
		{"one level", "/gallery", "/gallery/sub/image.png", "sub"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FolderPath(tt.galleryPath, tt.filePath)
			if got != tt.want {
				t.Errorf("FolderPath(%q, %q) = %q, want %q", tt.galleryPath, tt.filePath, got, tt.want)
			}
		})
	}
}

func TestSync_FileMoved(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")

	r1 := env.sync(t, database)
	if r1.Added != 1 {
		t.Fatalf("initial sync Added=%d, want 1", r1.Added)
	}

	subDir := filepath.Join(galleryDir, "sub")
	os.MkdirAll(subDir, 0755)
	dstPath := filepath.Join(subDir, "original.png")
	os.Rename(srcPath, dstPath)

	r2 := env.sync(t, database)
	if r2.Moved != 1 {
		t.Errorf("Moved = %d, want 1", r2.Moved)
	}
}

func TestFolderTree_Empty(t *testing.T) {
	database, _, _ := setupSyncTest(t)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 root node, got %d", len(nodes))
	}
	if nodes[0].Name != "(root)" {
		t.Errorf("root name = %q", nodes[0].Name)
	}
	if nodes[0].Count != 0 {
		t.Errorf("root count = %d, want 0", nodes[0].Count)
	}
}

func TestFolderTree_WithImages(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	// Root image (10x10)
	createTestPNGFileSize(t, galleryDir, "root.png", 10, 10)

	// Sub-folder image (distinct size to ensure different SHA-256)
	subDir := filepath.Join(galleryDir, "sub")
	os.MkdirAll(subDir, 0755)
	createTestPNGFileSize(t, subDir, "sub.png", 11, 10)

	env.sync(t, database)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("expected nodes")
	}
	root := nodes[0]
	if root.Count < 2 {
		t.Errorf("root total count = %d, want >= 2", root.Count)
	}
	if len(root.Children) < 1 {
		t.Errorf("expected at least 1 child folder, got %d", len(root.Children))
	}
	// Check sub folder node
	found := false
	for _, child := range root.Children {
		if child.Name == "sub" {
			found = true
			if child.Count != 1 {
				t.Errorf("sub folder count = %d, want 1", child.Count)
			}
			if child.Depth != 1 {
				t.Errorf("sub folder depth = %d, want 1", child.Depth)
			}
		}
	}
	if !found {
		t.Error("sub folder not found in FolderTree")
	}
}

func TestFolderTree_RecursiveCount(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	// parent/ has zero direct images but two subfolders with one each, so
	// parent should roll up to 2 even though nothing sits at parent/ itself.
	parentDir := filepath.Join(galleryDir, "parent")
	subA := filepath.Join(parentDir, "a")
	subB := filepath.Join(parentDir, "b")
	os.MkdirAll(subA, 0755)
	os.MkdirAll(subB, 0755)
	createTestPNGFileSize(t, subA, "a.png", 10, 10)
	createTestPNGFileSize(t, subB, "b.png", 11, 10)

	env.sync(t, database)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	root := nodes[0]
	if root.Count != 2 {
		t.Errorf("root count = %d, want 2", root.Count)
	}
	var parent *FolderNode
	for i := range root.Children {
		if root.Children[i].Name == "parent" {
			parent = &root.Children[i]
		}
	}
	if parent == nil {
		t.Fatal("parent folder not found in tree")
	}
	if parent.Count != 2 {
		t.Errorf("parent count = %d, want 2 (recursive)", parent.Count)
	}
	for _, c := range parent.Children {
		if c.Count != 1 {
			t.Errorf("%s count = %d, want 1", c.Name, c.Count)
		}
	}
}

func TestCountSlashes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    string
		want int
	}{
		{"empty", "", 0},
		{"one slash", "a/b", 1},
		{"two slashes", "a/b/c", 2},
		{"no slashes", "no_slashes", 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := countSlashes(tt.s); got != tt.want {
				t.Errorf("countSlashes(%q) = %d, want %d", tt.s, got, tt.want)
			}
		})
	}
}

func TestSync_ContextCanceled(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	// Seed enough files that Phase 2's per-file loop notices the cancelled
	// context before it finishes scanning everything.
	for i := 0; i < 32; i++ {
		createTestPNGFileSize(t, galleryDir, fmt.Sprintf("ctx_%02d.png", i), 10+i, 10)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Sync(ctx, database, env.galleryPath, env.thumbnailsPath, env.maxFileSizeMB, func(int, int, string) {})
	if err == nil {
		t.Fatal("Sync with a pre-cancelled ctx should surface the cancellation, got nil err")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestWatcher_IngestsFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	w, err := NewWatcher("", env.galleryPath, env.thumbnailsPath, env.maxFileSizeMB, database, nil)
	if err != nil {
		if strings.Contains(err.Error(), "too many open files") {
			t.Skip("skipping: system file descriptor limit reached")
		}
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)

	// Drop a file; poll the DB for arrival instead of assuming a fixed
	// debounce + IO budget.
	createTestPNGFile(t, galleryDir, "new.png")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		database.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&count)
		if count == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var count int
	database.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&count)
	t.Errorf("watcher did not ingest within 8 s; count = %d", count)
}
