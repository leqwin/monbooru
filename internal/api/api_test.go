package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/tags"
)

// fixedResolver returns a ResolverFunc that always hands back the same
// Gallery regardless of the requested name, which is how every test env is
// wired (single gallery). The resolver mirrors the web.Server behaviour:
// empty name falls back to the active gallery; unknown name is a miss.
func fixedResolver(g Gallery) ResolverFunc {
	return func(name string) (Gallery, bool) {
		if name == "" || name == g.Name {
			return g, true
		}
		return Gallery{}, false
	}
}

// testEnv holds a fully wired test environment.
type testEnv struct {
	handler    *Handler
	mux        http.Handler
	database   *db.DB
	cfg        *config.Config
	galleryDir string
	thumbDir   string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()

	galleryDir := filepath.Join(dir, "gallery")
	thumbDir := filepath.Join(dir, "thumbs")
	os.MkdirAll(galleryDir, 0755)
	os.MkdirAll(thumbDir, 0755)

	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	cfg.Galleries[0].GalleryPath = galleryDir
	cfg.Galleries[0].DBPath = filepath.Join(dir, "test.db")
	cfg.Galleries[0].ThumbnailsPath = thumbDir
	cfg.Gallery.MaxFileSizeMB = 100
	cfg.Auth.APIToken = testAPIToken

	g := Gallery{
		Name:           cfg.DefaultGallery,
		GalleryPath:    galleryDir,
		ThumbnailsPath: thumbDir,
		DB:             database,
		TagSvc:         tags.New(database),
	}
	h := New(cfg, jobs.NewManager(), fixedResolver(g))
	raw := http.NewServeMux()
	h.Mount(raw)
	// Wrap the mux so every request carries the bearer token by default.
	mux := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+testAPIToken)
		}
		raw.ServeHTTP(w, r)
	}))

	return &testEnv{handler: h, mux: mux, database: database, cfg: cfg,
		galleryDir: galleryDir, thumbDir: thumbDir}
}

const testAPIToken = "test-api-token"

// createTestImage creates a minimal PNG file in the gallery dir and ingests it.
// Returns the image ID.
func (e *testEnv) createTestImage(t *testing.T, name string, w, h int) int64 {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	path := filepath.Join(e.galleryDir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	png.Encode(f, img)
	f.Close()

	record, _, err := gallery.Ingest(e.database, e.galleryDir, e.thumbDir, path, "png")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return record.ID
}

func newTestMux(t *testing.T) http.Handler {
	return newTestEnv(t).mux
}

func TestOpenAPIJSON(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/api/v1/openapi.json", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "openapi") {
		t.Error("response missing 'openapi' key")
	}
}

func TestGetImageNotFound(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/api/v1/images/99999", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSearchImagesReturnsEnvelope(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/api/v1/images/search?q=", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, key := range []string{"page", "limit", "total", "results"} {
		if !strings.Contains(body, key) {
			t.Errorf("response missing key %q", key)
		}
	}
}

func TestListTagsReturnsEnvelope(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, key := range []string{"page", "limit", "total", "results"} {
		if !strings.Contains(body, key) {
			t.Errorf("response missing key %q", key)
		}
	}
}

func TestAPIDisabledWhenNoToken(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.Bootstrap(database)
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	cfg.Auth.APIToken = ""
	h := New(cfg, jobs.NewManager(), fixedResolver(Gallery{Name: cfg.DefaultGallery, DB: database, TagSvc: tags.New(database)}))
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when API token is empty, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "api_disabled") {
		t.Errorf("response missing 'api_disabled' code: %s", w.Body.String())
	}
}

func TestBearerAuthRejectsInvalidToken(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.Bootstrap(database)
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	cfg.Auth.APIToken = "secret-token"
	h := New(cfg, jobs.NewManager(), fixedResolver(Gallery{Name: cfg.DefaultGallery, DB: database, TagSvc: tags.New(database)}))
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestBearerAuthAcceptsValidToken(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.Bootstrap(database)
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	cfg.Auth.APIToken = "secret-token"
	h := New(cfg, jobs.NewManager(), fixedResolver(Gallery{Name: cfg.DefaultGallery, DB: database, TagSvc: tags.New(database)}))
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// --- openAPIDocs ---

func TestOpenAPIDocs(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/api/v1/docs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Monbooru API") {
		t.Error("docs response missing API title")
	}
	if !strings.Contains(body, "/api/v1/openapi.json") {
		t.Error("docs response should link to the raw OpenAPI spec")
	}
	if !strings.Contains(body, "/images/search") {
		t.Error("docs response should list the search endpoint")
	}
	if strings.Contains(body, "unpkg.com") || strings.Contains(body, "cdn.") {
		t.Error("docs response should not load any external assets")
	}
}

// --- getImage with valid ID ---

func TestGetImage_ValidID(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "get_test.png", 10, 10)

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/images/%d", id), nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["id"] == nil {
		t.Error("response missing 'id'")
	}
}

func TestGetImage_InvalidID(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/api/v1/images/notanumber", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- createImage via JSON path ---

func TestCreateImage_JSONPath(t *testing.T) {
	env := newTestEnv(t)

	// Create a real PNG file in the gallery dir
	imgPath := filepath.Join(env.galleryDir, "new_api.png")
	img := image.NewRGBA(image.Rect(0, 0, 15, 15))
	f, _ := os.Create(imgPath)
	png.Encode(f, img)
	f.Close()

	body, _ := json.Marshal(map[string]any{
		"path": imgPath,
		"tags": []string{"test_tag"},
	})
	req := httptest.NewRequest("POST", "/api/v1/images", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateImage_JSONPath_Duplicate(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "dup_api.png", 20, 20)
	_ = id

	// Try to ingest the same file again
	var canonPath string
	env.database.Read.QueryRow(`SELECT canonical_path FROM images LIMIT 1`).Scan(&canonPath)

	body, _ := json.Marshal(map[string]any{"path": canonPath})
	req := httptest.NewRequest("POST", "/api/v1/images", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for duplicate, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateImage_MissingPath(t *testing.T) {
	env := newTestEnv(t)
	body, _ := json.Marshal(map[string]any{"path": ""})
	req := httptest.NewRequest("POST", "/api/v1/images", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestCreateImage_InvalidJSON(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest("POST", "/api/v1/images", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- deleteImage ---

func TestDeleteImage(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "del_test.png", 10, 10)

	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/images/%d", id), nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify it's gone
	req2 := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/images/%d", id), nil)
	w2 := httptest.NewRecorder()
	env.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", w2.Code)
	}
}

func TestDeleteImage_NotFound(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("DELETE", "/api/v1/images/99999", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDeleteImage_InvalidID(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("DELETE", "/api/v1/images/bad", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- addImageTags / removeImageTags ---

func TestAddImageTags(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_add_test.png", 10, 10)

	body, _ := json.Marshal(map[string]any{"tags": []string{"red", "blue"}})
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Response should be a tag array
	var tags []any
	json.NewDecoder(w.Body).Decode(&tags)
	if len(tags) < 2 {
		t.Errorf("expected >= 2 tags in response, got %d", len(tags))
	}
}

func TestAddImageTags_InvalidID(t *testing.T) {
	mux := newTestMux(t)
	body, _ := json.Marshal(map[string]any{"tags": []string{"red"}})
	req := httptest.NewRequest("POST", "/api/v1/images/bad/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAddImageTags_InvalidBody(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_add_bad.png", 10, 10)
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/images/%d/tags", id), strings.NewReader("bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAddImageTags_ImageNotFound(t *testing.T) {
	mux := newTestMux(t)
	body, _ := json.Marshal(map[string]any{"tags": []string{"red"}})
	req := httptest.NewRequest("POST", "/api/v1/images/99999/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestRemoveImageTags(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_rem_test.png", 10, 10)

	// First add a tag
	addBody, _ := json.Marshal(map[string]any{"tags": []string{"to_remove"}})
	addReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(addBody))
	addReq.Header.Set("Content-Type", "application/json")
	env.mux.ServeHTTP(httptest.NewRecorder(), addReq)

	// Now remove it
	remBody, _ := json.Marshal(map[string]any{"tags": []string{"to_remove"}})
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(remBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRemoveImageTags_InvalidID(t *testing.T) {
	mux := newTestMux(t)
	body, _ := json.Marshal(map[string]any{"tags": []string{"red"}})
	req := httptest.NewRequest("DELETE", "/api/v1/images/bad/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRemoveImageTags_InvalidBody(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "rem_bad.png", 10, 10)
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRemoveImageTags_ImageNotFound(t *testing.T) {
	mux := newTestMux(t)
	body, _ := json.Marshal(map[string]any{"tags": []string{"x"}})
	req := httptest.NewRequest("DELETE", "/api/v1/images/99999/tags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// --- searchImages with parameters ---

func TestSearchImages_WithSort(t *testing.T) {
	env := newTestEnv(t)
	env.createTestImage(t, "sort1.png", 10, 10)
	env.createTestImage(t, "sort2.png", 11, 10)

	for _, sort := range []string{"newest", "oldest", "most_tags", "random"} {
		req := httptest.NewRequest("GET", "/api/v1/images/search?sort="+sort, nil)
		w := httptest.NewRecorder()
		env.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("sort=%s: expected 200, got %d", sort, w.Code)
		}
	}
}

func TestSearchImages_WithPagination(t *testing.T) {
	env := newTestEnv(t)
	env.createTestImage(t, "pag1.png", 10, 10)

	req := httptest.NewRequest("GET", "/api/v1/images/search?page=1&limit=10", nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestSearchImages_LimitCapped(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest("GET", "/api/v1/images/search?limit=9999", nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if limit, ok := resp["limit"].(float64); ok && limit > 200 {
		t.Errorf("limit should be capped at 200, got %v", limit)
	}
}

// --- parsePage and parseInt ---

func TestParsePage(t *testing.T) {
	req := httptest.NewRequest("GET", "/?page=3&limit=20", nil)
	offset, limit := parsePage(req, 40, 200)
	if offset != 40 {
		t.Errorf("offset = %d, want 40", offset)
	}
	if limit != 20 {
		t.Errorf("limit = %d, want 20", limit)
	}
}

func TestParsePage_Defaults(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	offset, limit := parsePage(req, 40, 200)
	if offset != 0 {
		t.Errorf("default offset = %d, want 0", offset)
	}
	if limit != 40 {
		t.Errorf("default limit = %d, want 40", limit)
	}
}

func TestParsePage_LimitCapped(t *testing.T) {
	req := httptest.NewRequest("GET", "/?limit=9999", nil)
	_, limit := parsePage(req, 40, 100)
	if limit != 100 {
		t.Errorf("capped limit = %d, want 100", limit)
	}
}

func TestParsePage_InvalidValues(t *testing.T) {
	req := httptest.NewRequest("GET", "/?page=bad&limit=also_bad", nil)
	offset, limit := parsePage(req, 40, 200)
	// Invalid values should use defaults
	if offset != 0 {
		t.Errorf("invalid page offset = %d, want 0", offset)
	}
	if limit != 40 {
		t.Errorf("invalid limit = %d, want 40", limit)
	}
}

func TestParseInt_Valid(t *testing.T) {
	n, err := parseInt("42")
	if err != nil || n != 42 {
		t.Errorf("parseInt(42) = %d, %v", n, err)
	}
}

func TestParseInt_Invalid(t *testing.T) {
	_, err := parseInt("notanumber")
	if err == nil {
		t.Error("expected error for non-numeric input")
	}
}

// --- CORS auth ---

func TestCORSRejectsBadOrigin(t *testing.T) {
	dir := t.TempDir()
	database, _ := db.Open(dir + "/test.db")
	db.Bootstrap(database)
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	cfg.Server.BaseURL = "https://myapp.example.com"
	h := New(cfg, jobs.NewManager(), fixedResolver(Gallery{Name: cfg.DefaultGallery, DB: database, TagSvc: tags.New(database)}))
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for bad CORS origin, got %d", w.Code)
	}
}

func TestBearerAuth_MissingHeader(t *testing.T) {
	dir := t.TempDir()
	database, _ := db.Open(dir + "/test.db")
	db.Bootstrap(database)
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	cfg.Auth.APIToken = "required-token"
	h := New(cfg, jobs.NewManager(), fixedResolver(Gallery{Name: cfg.DefaultGallery, DB: database, TagSvc: tags.New(database)}))
	mux := http.NewServeMux()
	h.Mount(mux)

	// No authorization header at all
	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing auth header, got %d", w.Code)
	}
}

// --- createImage multipart upload ---

func TestCreateImage_Multipart(t *testing.T) {
	env := newTestEnv(t)

	// Create PNG image bytes in memory
	var imgBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 12, 12))
	if err := png.Encode(&imgBuf, img); err != nil {
		t.Fatal(err)
	}

	// Build multipart body
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", "upload.png")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(imgBuf.Bytes())

	// Add tags field
	writer.WriteField("tags", `["multipart_tag"]`)
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/images", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201 for multipart upload, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateImage_Multipart_MissingFile(t *testing.T) {
	env := newTestEnv(t)

	// Multipart body without a "file" field
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("other_field", "value")
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/images", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing file field, got %d: %s", w.Code, w.Body.String())
	}
}

// --- listTags with category filter ---

func TestListTags_WithCategory(t *testing.T) {
	env := newTestEnv(t)

	req := httptest.NewRequest("GET", "/api/v1/tags?category=general", nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "results") {
		t.Errorf("response missing 'results': %s", body)
	}
}

func TestListTags_WithUnknownCategory(t *testing.T) {
	env := newTestEnv(t)

	// Unknown category → SQL query returns no row → catID stays 0, CategoryID not set
	req := httptest.NewRequest("GET", "/api/v1/tags?category=nonexistent_cat_xyz", nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for unknown category, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteImage_DeleteEmptyFolder(t *testing.T) {
	env := newTestEnv(t)

	// Create image in a subfolder
	subDir := filepath.Join(env.galleryDir, "sub2024")
	os.MkdirAll(subDir, 0755)
	img := image.NewRGBA(image.Rect(0, 0, 13, 13))
	imgPath := filepath.Join(subDir, "sub_img.png")
	f, _ := os.Create(imgPath)
	png.Encode(f, img)
	f.Close()

	record, _, err := gallery.Ingest(env.database, env.galleryDir, env.thumbDir, imgPath, "png")
	if err != nil {
		t.Fatal(err)
	}

	// Delete the file from disk first so the folder becomes empty after db delete
	os.Remove(imgPath)

	req := httptest.NewRequest("DELETE",
		fmt.Sprintf("/api/v1/images/%d?delete_empty_folder=true", record.ID), nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	// Should be 200 (folder deleted) or 204 (folder still not empty or doesn't exist)
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Errorf("expected 200 or 204 for delete with empty folder, got %d: %s",
			w.Code, w.Body.String())
	}
}
