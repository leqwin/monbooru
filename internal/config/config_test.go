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
[server]
bind_address = "0.0.0.0:9090"
base_url = "http://example.com"

[paths]
gallery_path    = "/my/gallery"
db_path         = "/my/data/monbooru.db"
thumbnails_path = "/my/data/thumbnails"
model_path      = "/my/models"

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
	if cfg.Paths.GalleryPath != "/my/gallery" {
		t.Errorf("GalleryPath = %q", cfg.Paths.GalleryPath)
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

	// File must have been created
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

	content := `
[server]
bind_address = "notavalidaddress"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Errorf("expected error for invalid bind address")
	}
}

func TestDefault_AllFields(t *testing.T) {
	cfg := Default()
	if cfg.Server.BindAddress == "" {
		t.Error("default BindAddress empty")
	}
	if cfg.Paths.GalleryPath == "" {
		t.Error("default GalleryPath empty")
	}
	if cfg.Gallery.MaxFileSizeMB <= 0 {
		t.Error("default MaxFileSizeMB <= 0")
	}
	if cfg.Tagger.Taggers != nil {
		t.Errorf("default Taggers should be empty, got %+v", cfg.Tagger.Taggers)
	}
	if cfg.UI.PageSize <= 0 {
		t.Error("default PageSize <= 0")
	}
}

func TestEnvVarOverride_AllKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")

	t.Setenv("MONBOORU_SERVER_BIND_ADDRESS", "0.0.0.0:9999")
	t.Setenv("MONBOORU_SERVER_BASE_URL", "https://example.com")
	t.Setenv("MONBOORU_PATHS_GALLERY_PATH", "/test/gallery")
	t.Setenv("MONBOORU_PATHS_DB_PATH", "/test/data.db")
	t.Setenv("MONBOORU_PATHS_THUMBNAILS_PATH", "/test/thumbs")
	t.Setenv("MONBOORU_PATHS_MODEL_PATH", "/test/models")
	t.Setenv("MONBOORU_GALLERY_WATCH_ENABLED", "false")
	t.Setenv("MONBOORU_GALLERY_MAX_FILE_SIZE_MB", "250")

	t.Setenv("MONBOORU_AUTH_ENABLE_PASSWORD", "true")
	t.Setenv("MONBOORU_AUTH_PASSWORD_HASH", "$2a$10$test")
	t.Setenv("MONBOORU_AUTH_SESSION_LIFETIME_DAYS", "30")
	t.Setenv("MONBOORU_AUTH_API_TOKEN", "mytoken")
	t.Setenv("MONBOORU_UI_PAGE_SIZE", "80")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.BindAddress != "0.0.0.0:9999" {
		t.Errorf("BindAddress = %q", cfg.Server.BindAddress)
	}
	if cfg.Server.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q", cfg.Server.BaseURL)
	}
	if cfg.Paths.GalleryPath != "/test/gallery" {
		t.Errorf("GalleryPath = %q", cfg.Paths.GalleryPath)
	}
	if cfg.Paths.DBPath != "/test/data.db" {
		t.Errorf("DBPath = %q", cfg.Paths.DBPath)
	}
	if cfg.Paths.ThumbnailsPath != "/test/thumbs" {
		t.Errorf("ThumbnailsPath = %q", cfg.Paths.ThumbnailsPath)
	}
	if cfg.Paths.ModelPath != "/test/models" {
		t.Errorf("ModelPath = %q", cfg.Paths.ModelPath)
	}
	if cfg.Gallery.WatchEnabled {
		t.Error("WatchEnabled should be false")
	}
	if cfg.Gallery.MaxFileSizeMB != 250 {
		t.Errorf("MaxFileSizeMB = %d", cfg.Gallery.MaxFileSizeMB)
	}

	if !cfg.Auth.EnablePassword {
		t.Error("EnablePassword should be true")
	}
	if cfg.Auth.PasswordHash != "$2a$10$test" {
		t.Errorf("PasswordHash = %q", cfg.Auth.PasswordHash)
	}
	if cfg.Auth.SessionLifetimeDays != 30 {
		t.Errorf("SessionLifetimeDays = %d", cfg.Auth.SessionLifetimeDays)
	}
	if cfg.Auth.APIToken != "mytoken" {
		t.Errorf("APIToken = %q", cfg.Auth.APIToken)
	}
	if cfg.UI.PageSize != 80 {
		t.Errorf("PageSize = %d", cfg.UI.PageSize)
	}
}

func TestEnvVarOverride_InvalidValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")

	// Invalid values should be silently ignored
	t.Setenv("MONBOORU_GALLERY_MAX_FILE_SIZE_MB", "not_a_number")
	t.Setenv("MONBOORU_GALLERY_WATCH_ENABLED", "not_a_bool")
	t.Setenv("MONBOORU_UI_PAGE_SIZE", "nope")
	t.Setenv("MONBOORU_AUTH_SESSION_LIFETIME_DAYS", "bad")
	t.Setenv("MONBOORU_AUTH_ENABLE_PASSWORD", "nope")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	// Should use defaults
	if cfg.Gallery.MaxFileSizeMB != 500 {
		t.Errorf("invalid env should use default MaxFileSizeMB=500, got %d", cfg.Gallery.MaxFileSizeMB)
	}
}

func TestSave_ErrorOnBadDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, cannot test write permission denial")
	}
	cfg := Default()
	err := Save(cfg, "/nonexistent_root/deep/path/monbooru.toml")
	if err == nil {
		t.Error("expected error saving to non-existent directory hierarchy")
	}
}

func TestValidate_EmptyBindAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	content := "[server]\nbind_address = \"\""
	os.WriteFile(path, []byte(content), 0644)
	_, err := Load(path)
	if err == nil {
		t.Error("expected error for empty bind_address")
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	os.WriteFile(path, []byte("not valid toml ][[["), 0644)

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid TOML")
	}
}

func TestSave_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	// Make dir read-only so CreateTemp fails
	if err := os.Chmod(dir, 0555); err != nil {
		t.Skip("cannot change dir permissions")
	}
	defer os.Chmod(dir, 0755)

	path := filepath.Join(dir, "monbooru.toml")
	cfg := Default()
	err := Save(cfg, path)
	if err == nil {
		// If running as root, this may succeed — skip in that case
		if os.Getuid() == 0 {
			t.Skip("running as root, chmod has no effect")
		}
		t.Error("expected error saving to read-only directory")
	}
}

func TestLoad_PermissionError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, chmod has no effect")
	}
	dir := t.TempDir()
	restrictedDir := filepath.Join(dir, "restricted")
	if err := os.MkdirAll(restrictedDir, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(restrictedDir, 0755)

	path := filepath.Join(restrictedDir, "monbooru.toml")
	_, err := Load(path)
	if err == nil {
		t.Error("expected error for inaccessible path")
	}
}

func TestLoad_DefaultCreationFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, chmod has no effect")
	}
	dir := t.TempDir()
	// 0555 = r-xr-xr-x: allows directory traversal (execute) but not creation (write).
	// This means Stat of a non-existent path inside the dir → ENOENT (IsNotExist = true)
	// but MkdirAll/CreateTemp will fail because the dir is not writable.
	if err := os.Chmod(dir, 0555); err != nil {
		t.Skip("cannot change dir permissions")
	}
	defer os.Chmod(dir, 0755)

	// Path inside a non-existent subdirectory of the non-writable dir
	// os.Stat → ENOENT (IsNotExist = true)
	// Load tries Save → MkdirAll fails → writeErr != nil
	path := filepath.Join(dir, "newsubdir", "monbooru.toml")
	_, err := Load(path)
	if err == nil {
		t.Error("expected error when default config creation fails")
	}
}

func TestLoad_StatError(t *testing.T) {
	// Create a file that is a valid path but try to stat a path inside it
	// (i.e., use a regular file as a directory)
	dir := t.TempDir()
	filePath := filepath.Join(dir, "notadir")
	if err := os.WriteFile(filePath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// This path cannot be stat'd as a directory entry inside a regular file
	invalidPath := filepath.Join(filePath, "monbooru.toml")
	_, err := Load(invalidPath)
	// Should error (either from stat or from MkdirAll trying to create dir inside a file)
	if err == nil {
		t.Error("expected error for path inside a regular file")
	}
}

func TestSaveAtomicTempInSameDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")

	cfg := Default()
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify file exists and is valid TOML
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save failed: %v", err)
	}
	if cfg2.Server.BindAddress != cfg.Server.BindAddress {
		t.Errorf("round-trip BindAddress mismatch")
	}

	// Verify no temp files remain in dir
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".monbooru.toml.") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
