package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromValidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")

	content := `
default_gallery = "default"

[[galleries]]
name = "default"
gallery_path = "/my/gallery"

[server]
bind_address = "0.0.0.0:9090"
base_url = "http://example.com"

[paths]
data_path  = "/my/data"
model_path = "/my/models"

[gallery]
watch_enabled    = false
max_file_size_mb = 100

[[tagger.taggers]]
name = "wd-swinv2"
enabled = true
confidence_threshold = 0.50

[auth]
enable_password       = true
password_hash         = "$2a$10$test"
session_lifetime_days = 14
api_token             = "mysecret"

[ui]
page_size = 60

[log]
level = "debug"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.BindAddress != "0.0.0.0:9090" {
		t.Errorf("BindAddress = %q", cfg.Server.BindAddress)
	}
	if len(cfg.Galleries) != 1 || cfg.Galleries[0].GalleryPath != "/my/gallery" {
		t.Errorf("Galleries = %+v", cfg.Galleries)
	}
	if cfg.Galleries[0].DBPath != "/my/data/default/monbooru.db" {
		t.Errorf("DBPath = %q", cfg.Galleries[0].DBPath)
	}
	if cfg.Galleries[0].ThumbnailsPath != "/my/data/default/thumbnails" {
		t.Errorf("ThumbnailsPath = %q", cfg.Galleries[0].ThumbnailsPath)
	}
	if cfg.Paths.DataPath != "/my/data" {
		t.Errorf("DataPath = %q", cfg.Paths.DataPath)
	}
	if cfg.Gallery.MaxFileSizeMB != 100 {
		t.Errorf("MaxFileSizeMB = %d", cfg.Gallery.MaxFileSizeMB)
	}
	if len(cfg.Tagger.Taggers) != 1 || cfg.Tagger.Taggers[0].ConfidenceThreshold != 0.50 {
		t.Errorf("Taggers = %+v", cfg.Tagger.Taggers)
	}
	if !cfg.Auth.EnablePassword {
		t.Errorf("EnablePassword should be true")
	}
	if cfg.UI.PageSize != 60 {
		t.Errorf("PageSize = %d", cfg.UI.PageSize)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q", cfg.Log.Level)
	}
}

func TestDefaultLogLevel(t *testing.T) {
	if got := Default().Log.Level; got != "warn" {
		t.Errorf("default Log.Level = %q, want %q", got, "warn")
	}
}

func TestMissingTOMLCreatesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Server.BindAddress != "127.0.0.1:8080" {
		t.Errorf("default BindAddress = %q", cfg.Server.BindAddress)
	}
	if cfg.UI.PageSize != 40 {
		t.Errorf("default PageSize = %d", cfg.UI.PageSize)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("default config file was not created")
	}
}

func TestEnvVarOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	t.Setenv("MONBOORU_UI_PAGE_SIZE", "55")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.UI.PageSize != 55 {
		t.Errorf("PageSize = %v, want 55", cfg.UI.PageSize)
	}
}

func TestInvalidBindAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	os.WriteFile(path, []byte(`
[server]
bind_address = "notavalidaddress"
`), 0644)
	if _, err := Load(path); err == nil {
		t.Errorf("expected error for invalid bind address")
	}
}

func TestDefault_AllFields(t *testing.T) {
	cfg := Default()
	if cfg.Server.BindAddress == "" {
		t.Error("default BindAddress empty")
	}
	if cfg.Paths.DataPath == "" {
		t.Error("default DataPath empty")
	}
	if cfg.Gallery.MaxFileSizeMB <= 0 {
		t.Error("default MaxFileSizeMB <= 0")
	}
	if cfg.UI.PageSize <= 0 {
		t.Error("default PageSize <= 0")
	}
	if len(cfg.Galleries) != 1 || cfg.Galleries[0].Name != "default" {
		t.Errorf("default Galleries = %+v", cfg.Galleries)
	}
	if cfg.DefaultGallery != "default" {
		t.Errorf("default DefaultGallery = %q", cfg.DefaultGallery)
	}
}

func TestLoadMultipleGalleries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	content := `
default_gallery = "stock"

[[galleries]]
name = "default"
gallery_path = "/gallery"

[[galleries]]
name = "stock"
gallery_path = "/gallery2"

[server]
bind_address = "127.0.0.1:8080"

[paths]
data_path  = "/data"
model_path = "/models"
`
	os.WriteFile(path, []byte(content), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.Galleries) != 2 {
		t.Fatalf("Galleries = %+v", cfg.Galleries)
	}
	if cfg.DefaultGallery != "stock" {
		t.Errorf("DefaultGallery = %q", cfg.DefaultGallery)
	}
	stock := cfg.FindGallery("stock")
	if stock == nil || stock.DBPath != "/data/stock/monbooru.db" {
		t.Errorf("stock DBPath = %q", stock)
	}
}

func TestDefaultGalleryFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	os.WriteFile(path, []byte(`
default_gallery = "missing"

[[galleries]]
name = "only"
gallery_path = "/gallery"
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.DefaultGallery != "only" {
		t.Errorf("DefaultGallery should fall back to only gallery, got %q", cfg.DefaultGallery)
	}
}

func TestLoadRejectsDuplicateGalleryNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	os.WriteFile(path, []byte(`
[[galleries]]
name = "a"
gallery_path = "/gallery1"

[[galleries]]
name = "a"
gallery_path = "/gallery2"
`), 0644)
	if _, err := Load(path); err == nil {
		t.Error("expected error for duplicate gallery name")
	}
}

func TestDerivePaths(t *testing.T) {
	cfg := Default()
	db, thumbs := cfg.DerivePaths("stock")
	if db != "/data/stock/monbooru.db" {
		t.Errorf("DerivePaths db = %q", db)
	}
	if thumbs != "/data/stock/thumbnails" {
		t.Errorf("DerivePaths thumbs = %q", thumbs)
	}
}

func TestValidateGalleryName(t *testing.T) {
	for _, n := range []string{"default", "a", "stock_1", "StockSet-2"} {
		if err := ValidateGalleryName(n); err != nil {
			t.Errorf("ValidateGalleryName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range []string{"", "with space", "weird/slash", "dot.name", "accénté"} {
		if err := ValidateGalleryName(n); err == nil {
			t.Errorf("ValidateGalleryName(%q) = nil, want error", n)
		}
	}
}

func TestSave_ErrorOnBadDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, cannot test write permission denial")
	}
	if err := Save(Default(), "/nonexistent_root/deep/path/monbooru.toml"); err == nil {
		t.Error("expected error saving to non-existent directory hierarchy")
	}
}

func TestValidate_EmptyBindAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	os.WriteFile(path, []byte("[server]\nbind_address = \"\""), 0644)
	if _, err := Load(path); err == nil {
		t.Error("expected error for empty bind_address")
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	os.WriteFile(path, []byte("not valid toml ][[["), 0644)
	if _, err := Load(path); err == nil {
		t.Error("expected error for invalid TOML")
	}
}

func TestSave_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0555); err != nil {
		t.Skip("cannot change dir permissions")
	}
	defer os.Chmod(dir, 0755)
	if err := Save(Default(), filepath.Join(dir, "monbooru.toml")); err == nil {
		if os.Getuid() == 0 {
			t.Skip("running as root, chmod has no effect")
		}
		t.Error("expected error saving to read-only directory")
	}
}

func TestSaveAtomicTempInSameDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	cfg := Default()
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save failed: %v", err)
	}
	if cfg2.Server.BindAddress != cfg.Server.BindAddress {
		t.Errorf("round-trip BindAddress mismatch")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".monbooru.toml.") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
