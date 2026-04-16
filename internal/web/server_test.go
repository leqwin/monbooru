package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	mgr := jobs.NewManager()

	srv, err := NewServer(cfg, "./monbooru.toml", database, mgr, false)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func TestServerStartsAndServesStatic(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/static/main.css", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for /static/main.css, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct == "" {
		t.Error("expected Content-Type header for CSS")
	}
}

func TestLoginPageRendersWithoutAuth(t *testing.T) {
	srv := newTestServer(t)
	// Auth disabled by default → /login now renders an informational
	// notice rather than 303'ing so a user who bookmarked the page still
	// sees an explanation.
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from /login when auth disabled, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Password authentication is disabled") {
		t.Errorf("expected disabled notice, got:\n%s", w.Body.String())
	}
}

func TestGalleryReturns200(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET / expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestGalleryContainsExpectedElements(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	checks := []string{
		`id="search-input"`,
		`id="gallery-grid"`,
		`id="batch-bar"`,
		`MONBOORU`,
		`/static/main.css`,
		`/static/htmx.min.js`,
	}
	for _, s := range checks {
		if !strings.Contains(body, s) {
			t.Errorf("gallery page missing expected element: %q", s)
		}
	}
}

func TestGalleryHTMXPartialReturnsGrid(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/?q=test&sort=newest", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "gallery-grid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("HTMX partial expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Should return partial, not full page
	if strings.Contains(body, "<html") {
		t.Error("HTMX partial response should not contain <html>")
	}
	if !strings.Contains(body, "thumb-grid") {
		t.Error("HTMX partial should contain thumb-grid")
	}
}

func TestGalleryEmptyFolderDialogRendered(t *testing.T) {
	// Empty folders are now deleted automatically without a dialog prompt.
	// The empty_folder query param is no longer used; verify the page still loads.
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/?q=", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestGallerySearchParamsPreserved(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/?q=mycategory&sort=newest&order=asc&page=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "mycategory") {
		t.Error("search query should appear in rendered page")
	}
	if !strings.Contains(body, `value="newest"`) || !strings.Contains(body, "selected") {
		t.Error("sort option should be selected")
	}
}

func TestCSRFRejectsUnauthenticatedPost(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/internal/sync", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 CSRF rejection, got %d", w.Code)
	}
}

func TestSessionMiddlewareRedirectsWhenAuthEnabled(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Auth.EnablePassword = true
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected redirect to /login when auth enabled, got %d", w.Code)
	}
}

func TestAllPagesReturn200(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	pages := []string{"/", "/tags", "/categories", "/settings", "/help"}
	for _, page := range pages {
		req := httptest.NewRequest("GET", page, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s expected 200, got %d\nbody: %s", page, w.Code, w.Body.String())
		}
	}
}

func TestJobStatusPartialReturns200(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/internal/job/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("job status expected 200, got %d", w.Code)
	}
}

// insertTestImage inserts a minimal image row and returns its ID.
func insertTestImage(t *testing.T, database *db.DB) int64 {
	t.Helper()
	res, err := database.Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		VALUES ('/tmp/test.jpg', 'jpg', 1024, 'abc123', datetime('now'))`)
	if err != nil {
		t.Fatalf("insert test image: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestDetailPageReturns200(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db)
	h := srv.Handler()

	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d", id), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("detail page expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestDetailPageContainsMetadata(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db)
	h := srv.Handler()

	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d", id), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	checks := []string{"detail-topbar", "detail-media", "meta-table", "danger-zone"}
	for _, sel := range checks {
		if !strings.Contains(body, sel) {
			t.Errorf("detail page missing element %q", sel)
		}
	}
}

func TestDetailPageReturns404ForMissingImage(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/images/99999", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("missing image expected 404, got %d", w.Code)
	}
}

func TestDegradedModeBannerShown(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	mgr := jobs.NewManager()
	srv, err := NewServer(cfg, "./monbooru.toml", database, mgr, true) // degraded=true
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "degraded-banner") {
		t.Error("degraded mode: expected degraded-banner in page")
	}
	if strings.Contains(body, `action="/internal/sync"`) {
		t.Error("degraded mode: sync button should be hidden")
	}
}

func TestDegradedModeSyncBlocked(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("bootstrap db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := config.Default()
	mgr := jobs.NewManager()
	srv, err := NewServer(cfg, "./monbooru.toml", database, mgr, true) // degraded=true
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/internal/sync", nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("degraded mode sync expected 503, got %d", w.Code)
	}
}

func TestMissingImageBannerShown(t *testing.T) {
	srv := newTestServer(t)
	// Insert a missing image
	res, err := srv.db.Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
		VALUES ('/nonexistent/file.jpg', 'jpg', 1024, 'deadbeef', 1, datetime('now'))`)
	if err != nil {
		t.Fatalf("insert missing image: %v", err)
	}
	id, _ := res.LastInsertId()

	h := srv.Handler()
	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d", id), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("missing image detail expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "missing-banner") {
		t.Error("missing image: expected missing-banner in detail page")
	}
	if !strings.Contains(body, "no longer present on disk") {
		t.Error("missing image: expected missing file message in banner")
	}
}

func TestToggleFavoriteReturnsButton(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db)
	h := srv.Handler()

	// Auth is disabled in test server so session ID is always "anon".
	postReq := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/favorite", id), nil)
	postReq.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))

	postW := httptest.NewRecorder()
	h.ServeHTTP(postW, postReq)

	if postW.Code != http.StatusOK {
		t.Errorf("toggle favorite expected 200, got %d\nbody: %s", postW.Code, postW.Body.String())
	}
	body := postW.Body.String()
	if !strings.Contains(body, "fav-btn") {
		t.Errorf("toggle favorite response missing fav-btn, got: %s", body)
	}
}

func TestDeleteImage(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db)
	h := srv.Handler()

	req := httptest.NewRequest("DELETE", fmt.Sprintf("/images/%d", id), nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("delete image expected 200, got %d", w.Code)
	}
	// Verify image is gone
	var count int
	srv.db.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE id = ?`, id).Scan(&count)
	if count != 0 {
		t.Error("image should be deleted from DB")
	}
}

func TestUploadPageReturns200(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/upload", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /upload expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Upload Images") {
		t.Error("upload page missing expected heading")
	}
}

func TestSettingsGalleryPost(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	body := "_csrf=" + srv.csrfToken("anon") + "&watch_enabled=on&max_file_size_mb=200"
	req := httptest.NewRequest("POST", "/settings/gallery", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("settings gallery POST expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Saved") {
		t.Error("expected 'Saved' flash message")
	}
}

func TestSettingsUIPost(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	body := "_csrf=" + srv.csrfToken("anon") + "&page_size=60"
	req := httptest.NewRequest("POST", "/settings/ui", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("settings UI POST expected 200, got %d", w.Code)
	}
	if srv.cfg.UI.PageSize != 60 {
		t.Errorf("PageSize = %d, want 60", srv.cfg.UI.PageSize)
	}
}

func TestCSRFTokenValidation(t *testing.T) {
	srv := newTestServer(t)
	sess := "test-session-id"
	token := srv.csrfToken(sess)
	if !srv.validateCSRF(sess, token) {
		t.Error("validateCSRF should accept valid token")
	}
	if srv.validateCSRF(sess, "wrong-token") {
		t.Error("validateCSRF should reject invalid token")
	}
	if srv.validateCSRF("other-session", token) {
		t.Error("validateCSRF should reject token for different session")
	}
}

func TestCSRFTokensAreServerScoped(t *testing.T) {
	srvA := newTestServer(t)
	srvB := newTestServer(t)
	tok := srvA.csrfToken("anon")
	if srvB.validateCSRF("anon", tok) {
		t.Error("tokens issued by one Server must not validate against another")
	}
}

func TestSessionExpiry(t *testing.T) {
	store := NewSessionStore()
	id, err := store.NewSession(0) // 0 days = expires immediately
	if err != nil {
		t.Fatal(err)
	}
	// Session with 0-day lifetime should already be expired
	if _, ok := store.GetSession(id); ok {
		t.Error("session with 0-day lifetime should be expired")
	}

	// Create a valid session
	id2, _ := store.NewSession(1) // 1 day
	if _, ok := store.GetSession(id2); !ok {
		t.Error("session with 1-day lifetime should be valid")
	}

	// Test SweepExpired
	store.SweepExpired()
	if _, ok := store.GetSession(id2); !ok {
		t.Error("non-expired session should survive sweep")
	}
}

func TestPruneMissingImages(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	// Insert a missing image
	srv.db.Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
		VALUES ('/nonexistent/file.jpg', 'jpg', 1024, 'prune_test_hash', 1, datetime('now'))`)

	body := "_csrf=" + srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/settings/maintenance/prune-missing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("prune missing expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Removed") {
		t.Error("expected 'Removed' in response")
	}

	// Verify pruned
	var count int
	srv.db.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE sha256 = 'prune_test_hash'`).Scan(&count)
	if count != 0 {
		t.Error("missing image should have been pruned")
	}
}
