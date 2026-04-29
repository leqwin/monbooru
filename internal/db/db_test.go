package db

import (
	"context"
	"sync"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	// Use a temp file so both read and write pools share the same database.
	// In-memory SQLite (:memory:) creates separate DBs per pool.
	dir := t.TempDir()
	db, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestBootstrap(t *testing.T) {
	db := openTestDB(t)

	if err := Bootstrap(db); err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}

	tables := []string{
		"tag_categories", "tags",
		"images", "image_paths", "image_tags",
		"sd_metadata", "comfyui_metadata", "saved_searches",
	}
	for _, tbl := range tables {
		var n int
		if err := db.Read.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&n); err != nil {
			t.Fatalf("querying for table %q: %v", tbl, err)
		}
		if n == 0 {
			t.Errorf("table %q not found after Bootstrap", tbl)
		}
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	db := openTestDB(t)

	if err := Bootstrap(db); err != nil {
		t.Fatalf("first Bootstrap failed: %v", err)
	}
	if err := Bootstrap(db); err != nil {
		t.Fatalf("second Bootstrap failed: %v", err)
	}
}

func TestForeignKeyEnforcement(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatal(err)
	}

	// Insert image_tags row with non-existent image_id - must fail
	_, err := db.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id) VALUES (9999, 9999)`,
	)
	if err == nil {
		t.Error("expected foreign key error, got nil")
	}
}

func TestConcurrentReads(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int
			if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
				t.Errorf("concurrent read failed: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestShrinkMemory(t *testing.T) {
	db := openTestDB(t)
	if err := Bootstrap(db); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		var n int
		if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ShrinkMemory(context.Background()); err != nil {
		t.Errorf("ShrinkMemory: %v", err)
	}
	var n int
	if err := db.Read.QueryRow(`SELECT COUNT(*) FROM tag_categories`).Scan(&n); err != nil {
		t.Errorf("read after shrink: %v", err)
	}
}
