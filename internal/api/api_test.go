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
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	record, _, err := gallery.Ingest(e.database, e.galleryDir, e.thumbDir, path, "png", "")
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
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
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
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
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
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
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
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

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

func TestCreateImage_JSONOriginRoundTrip(t *testing.T) {
	env := newTestEnv(t)

	// Pre-create a PNG on disk for JSON path-reference mode.
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	path := filepath.Join(env.galleryDir, "ext_source.png")
	f, _ := os.Create(path)
	png.Encode(f, img)
	f.Close()

	body, _ := json.Marshal(map[string]any{
		"path":   path,
		"source": "https://danbooru/12345",
	})
	req := httptest.NewRequest("POST", "/api/v1/images", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["origin"] != "https://danbooru/12345" {
		t.Errorf("origin = %v, want %q", resp["origin"], "https://danbooru/12345")
	}

	id := int64(resp["id"].(float64))
	getReq := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/images/%d", id), nil)
	gw := httptest.NewRecorder()
	env.mux.ServeHTTP(gw, getReq)
	if gw.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", gw.Code)
	}
	var got map[string]any
	json.NewDecoder(gw.Body).Decode(&got)
	if got["origin"] != "https://danbooru/12345" {
		t.Errorf("origin on GET = %v, want %q", got["origin"], "https://danbooru/12345")
	}
}

func TestAddImageTags_CarriesSource(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "tag_source.png", 10, 10)

	body, _ := json.Marshal(map[string]any{
		"tags":   []string{"from_app"},
		"source": "my_app",
	})
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var taggerName *string
	var isAuto int
	err := env.database.Read.QueryRow(`
		SELECT it.is_auto, it.tagger_name FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		WHERE it.image_id = ? AND t.name = 'from_app'`, id).Scan(&isAuto, &taggerName)
	if err != nil {
		t.Fatalf("scan image_tags: %v", err)
	}
	if isAuto != 0 {
		t.Errorf("is_auto = %d, want 0", isAuto)
	}
	if taggerName == nil || *taggerName != "my_app" {
		t.Errorf("tagger_name = %v, want %q", taggerName, "my_app")
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

// findImageTag returns the (name, category) for the first tag attached to id
// whose name matches want, or ("","") if none. Used by the colon-fallback
// tests to confirm that a tag containing `:` was stored whole instead of
// being split into a category/name pair.
func findImageTag(t *testing.T, env *testEnv, id int64, want string) (string, string) {
	t.Helper()
	rows, err := env.database.Read.Query(
		`SELECT t.name, tc.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ?`, id)
	if err != nil {
		t.Fatalf("query image tags: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n, c string
		if err := rows.Scan(&n, &c); err != nil {
			t.Fatal(err)
		}
		if n == want {
			return n, c
		}
	}
	return "", ""
}

func TestAddImageTags_ColonFallbackLiteral(t *testing.T) {
	// A tag whose prefix before `:` doesn't match any category must be
	// stored whole in the general category, so names like `nier:automata`
	// round-trip instead of silently splitting into an `automata` tag in
	// a non-existent `nier` category.
	env := newTestEnv(t)
	id := env.createTestImage(t, "colon_fallback.png", 10, 10)

	body, _ := json.Marshal(map[string]any{"tags": []string{"nier:automata", ":3"}})
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	for _, name := range []string{"nier:automata", ":3"} {
		gotName, gotCat := findImageTag(t, env, id, name)
		if gotName != name {
			t.Errorf("tag %q not stored on image", name)
		}
		if gotCat != "general" {
			t.Errorf("tag %q category = %q, want general", name, gotCat)
		}
	}
}

func TestAddImageTags_CategoryPrefixStillSplits(t *testing.T) {
	// A prefix that IS a real category (artist in this case) must still
	// split so API callers can create tags in non-general categories the
	// same way the web UI does.
	env := newTestEnv(t)
	id := env.createTestImage(t, "colon_split.png", 10, 10)

	body, _ := json.Marshal(map[string]any{"tags": []string{"artist:john_doe"}})
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	gotName, gotCat := findImageTag(t, env, id, "john_doe")
	if gotName != "john_doe" {
		t.Error("tag john_doe not stored on image")
	}
	if gotCat != "artist" {
		t.Errorf("category = %q, want artist", gotCat)
	}
	// And the literal form must NOT have been stored as a general tag.
	if n, _ := findImageTag(t, env, id, "artist:john_doe"); n != "" {
		t.Error("literal artist:john_doe must not be stored when artist is a real category")
	}
}

func TestRemoveImageTags_ColonFallbackLiteral(t *testing.T) {
	// The removal path mirrors the addition path: a colon-bearing tag
	// whose prefix isn't a category must be matched literally against
	// names on the image, not split into a category-qualified lookup
	// that finds nothing.
	env := newTestEnv(t)
	id := env.createTestImage(t, "colon_rm.png", 10, 10)

	addBody, _ := json.Marshal(map[string]any{"tags": []string{"nier:automata"}})
	addReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(addBody))
	addReq.Header.Set("Content-Type", "application/json")
	env.mux.ServeHTTP(httptest.NewRecorder(), addReq)

	remBody, _ := json.Marshal(map[string]any{"tags": []string{"nier:automata"}})
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(remBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if n, _ := findImageTag(t, env, id, "nier:automata"); n != "" {
		t.Error("nier:automata should have been removed from the image")
	}
}

func TestRemoveImageTags_CategoryMissFallsThroughToLiteral(t *testing.T) {
	// When the prefix IS a real category (artist) but the image holds
	// no (artist, foo) pair, resolution must fall through to a literal
	// name match so a general-category tag stored whole as "artist:foo"
	// is still removable. Typical source: a .txt auto-tagger whose
	// label file listed "artist:xxx" - tagger writes bypass the input
	// splitter so the literal form lands in general.
	env := newTestEnv(t)
	id := env.createTestImage(t, "colon_collide.png", 10, 10)

	var genID int64
	if err := env.database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&genID); err != nil {
		t.Fatal(err)
	}
	svc := tags.New(env.database)
	tag, err := svc.GetOrCreateTag("artist:foo", genID)
	if err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if err := svc.AddTagToImage(id, tag.ID, false, nil); err != nil {
		t.Fatalf("attach tag: %v", err)
	}

	remBody, _ := json.Marshal(map[string]any{"tags": []string{"artist:foo"}})
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(remBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if n, _ := findImageTag(t, env, id, "artist:foo"); n != "" {
		t.Error("artist:foo should have been removed from the image after category-miss fall-through")
	}
}

// --- searchImages with parameters ---

func TestSearchImages_WithSort(t *testing.T) {
	env := newTestEnv(t)
	// Different widths produce different SHAs so each image is distinct.
	env.createTestImage(t, "sort1.png", 10, 10) // file_size S1
	env.createTestImage(t, "sort2.png", 30, 30) // file_size S2 > S1

	get := func(t *testing.T, sort, order string) []any {
		t.Helper()
		u := "/api/v1/images/search?sort=" + sort
		if order != "" {
			u += "&order=" + order
		}
		req := httptest.NewRequest("GET", u, nil)
		w := httptest.NewRecorder()
		env.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("sort=%s order=%s: expected 200, got %d: %s", sort, order, w.Code, w.Body.String())
		}
		var resp map[string]any
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		r, _ := resp["results"].([]any)
		return r
	}

	// Spec §8.3 recognises exactly three sorts: newest (default), filesize,
	// random. Each must produce a 200 AND an ordered result.
	t.Run("newest desc puts second upload first", func(t *testing.T) {
		got := get(t, "newest", "desc")
		if len(got) != 2 {
			t.Fatalf("expected 2 results, got %d", len(got))
		}
		first := got[0].(map[string]any)["canonical_path"].(string)
		if !strings.HasSuffix(first, "sort2.png") {
			t.Errorf("newest desc first = %q, want …/sort2.png", first)
		}
	})
	t.Run("filesize desc puts larger file first", func(t *testing.T) {
		got := get(t, "filesize", "desc")
		first := got[0].(map[string]any)["canonical_path"].(string)
		if !strings.HasSuffix(first, "sort2.png") {
			t.Errorf("filesize desc first = %q, want …/sort2.png", first)
		}
	})
	t.Run("random returns a 200 with both results", func(t *testing.T) {
		got := get(t, "random", "")
		if len(got) != 2 {
			t.Errorf("random expected 2 results, got %d", len(got))
		}
	})
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
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Assert both the presence of `limit` and its upper bound: the `if ok`
	// guard used to let a missing field silently pass.
	raw, ok := resp["limit"]
	if !ok {
		t.Fatal("response missing 'limit'")
	}
	limit, ok := raw.(float64)
	if !ok {
		t.Fatalf("limit had type %T, want float64", raw)
	}
	if limit > 200 {
		t.Errorf("limit = %v, want <= 200 (spec §8.3 API max)", limit)
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
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
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
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
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

	subDir := filepath.Join(env.galleryDir, "sub2024")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 13, 13))
	imgPath := filepath.Join(subDir, "sub_img.png")
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	record, _, err := gallery.Ingest(env.database, env.galleryDir, env.thumbDir, imgPath, "png", "")
	if err != nil {
		t.Fatal(err)
	}
	// Remove the file off disk so the sub-folder is empty after the DB delete.
	if err := os.Remove(imgPath); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("DELETE",
		fmt.Sprintf("/api/v1/images/%d?delete_empty_folder=true", record.ID), nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)

	// With the folder empty, the handler must return 200 + the folder_deleted
	// payload (images.go:431). 204 would mean the folder was not removed.
	if w.Code != http.StatusOK {
		t.Fatalf("empty-folder delete expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["folder_deleted"] != true {
		t.Errorf("folder_deleted = %v, want true", body["folder_deleted"])
	}
	if body["folder"] != "sub2024" {
		t.Errorf("folder = %v, want sub2024", body["folder"])
	}
	if _, err := os.Stat(subDir); !os.IsNotExist(err) {
		t.Errorf("sub-folder should have been removed, stat err = %v", err)
	}
}
