package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithDegraded(t, false)
}

// newTestServerWithDegraded builds a Server over a single in-memory gallery.
// When degraded=true the gallery_path points at a non-existent directory so
// the startup probe flips the context's Degraded flag.
func newTestServerWithDegraded(t *testing.T, degraded bool) *Server {
	t.Helper()
	dir := t.TempDir()
	galleryDir := filepath.Join(dir, "gallery")
	if degraded {
		galleryDir = filepath.Join(dir, "nonexistent_gallery")
	} else {
		os.MkdirAll(galleryDir, 0o755)
	}

	cfg := config.Default()
	cfg.Paths.DataPath = filepath.Join(dir, "data")
	cfg.Galleries[0].GalleryPath = galleryDir
	cfg.Galleries[0].DBPath = filepath.Join(cfg.Paths.DataPath, "default", "monbooru.db")
	cfg.Galleries[0].ThumbnailsPath = filepath.Join(cfg.Paths.DataPath, "default", "thumbnails")
	cfg.Gallery.WatchEnabled = false

	srv, err := NewServer(cfg, filepath.Join(dir, "monbooru.toml"), jobs.NewManager())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)
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
	// Content-Type must be text/html so HTMX's swap logic accepts it;
	// a JSON default would silently break grid updates.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("HTMX partial Content-Type = %q, want text/html…", ct)
	}
	body := w.Body.String()
	// Should return partial, not full page.
	if strings.Contains(body, "<html") {
		t.Error("HTMX partial response should not contain <html>")
	}
	if !strings.Contains(body, "thumb-grid") {
		t.Error("HTMX partial should contain thumb-grid")
	}
	// Partials include the OOB sidebar swap so filtering updates tag
	// counts without an extra round trip (spec §12.3).
	if !strings.Contains(body, "sidebar-inner") {
		t.Error("HTMX partial should carry the OOB sidebar swap")
	}
}

func TestGalleryEmptyFolderDialogRendered(t *testing.T) {
	// Empty folders are deleted automatically without a dialog prompt;
	// verify the page still loads cleanly when no `empty_folder` param is
	// supplied.
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

// TestJobStatusHandler_RunningMarkup covers the template branch that
// Playwright cannot reliably reach: racing a running indicator across the
// 2 s HTMX poll produces flaky tests. Here we stage the manager in
// "running" state directly and assert the template emits job-running plus
// a × cancel button for cancellable types.
func TestJobStatusHandler_RunningMarkup(t *testing.T) {
	srv := newTestServer(t)
	// Stage the manager in running re-extract state.
	if err := srv.jobs.Start("re-extract"); err != nil {
		t.Fatalf("jobs.Start: %v", err)
	}
	defer srv.jobs.Complete("done")
	srv.jobs.Update(3, 10, "reading metadata")

	req := httptest.NewRequest("GET", "/internal/job/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("job status expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="job-running`,           // running class flips on the wrapper
		`data-job-type="re-extract"`,   // job type surfaces for UI hooks
		`<button class="job-dismiss"`,  // × cancel button
		`reading metadata`,             // progress message rendered
	} {
		if !strings.Contains(body, want) {
			t.Errorf("running job partial missing %q\nbody: %s", want, body)
		}
	}
}

// TestJobStatusHandler_NoCancelForWatcher pins the complementary branch:
// the transient "watcher" pseudo-job is surfaced as a summary, not as a
// cancellable running job.
func TestJobStatusHandler_NoCancelForWatcher(t *testing.T) {
	srv := newTestServer(t)
	srv.jobs.SetWatcherMessage("added foo.png")

	req := httptest.NewRequest("GET", "/internal/job/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `class="job-running`) {
		t.Error("watcher pseudo-event must not render as a running job")
	}
	if !strings.Contains(body, "added foo.png") {
		t.Errorf("expected watcher summary in body, got: %s", body)
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
	id := insertTestImage(t, srv.db())
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
	id := insertTestImage(t, srv.db())
	h := srv.Handler()

	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d", id), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	checks := []string{"detail-topbar", "detail-media", "meta-table", "danger-zone",
		// Browse sections on the sidebar are lazy-loaded from this endpoint.
		`/internal/sidebar-browse`,
		// Search bar (trimmed copy of the gallery header) lives on detail too.
		`id="gallery-header"`, `id="search-form"`, `id="search-input"`,
	}
	for _, sel := range checks {
		if !strings.Contains(body, sel) {
			t.Errorf("detail page missing element %q", sel)
		}
	}
}

func TestSidebarBrowseReturnsBrowseSections(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/internal/sidebar-browse", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Folder tree + source filter + saved searches markup; per-page tag
	// groups must not leak in.
	for _, sel := range []string{"folder-tree-section", "source-filter-section"} {
		if !strings.Contains(body, sel) {
			t.Errorf("sidebar-browse missing %q\nbody: %s", sel, body)
		}
	}
	if strings.Contains(body, `id="tag-groups"`) {
		t.Error("sidebar-browse should not render the per-page tag-groups block")
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
	srv := newTestServerWithDegraded(t, true)
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
	srv := newTestServerWithDegraded(t, true)
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
	res, err := srv.db().Write.Exec(`
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
	id := insertTestImage(t, srv.db())
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
	id := insertTestImage(t, srv.db())
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
	srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images WHERE id = ?`, id).Scan(&count)
	if count != 0 {
		t.Error("image should be deleted from DB")
	}
	// Without back_* params there's no referring search context, so the
	// redirect still falls through to the gallery root.
	if got := w.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("no back-context redirect = %q, want /", got)
	}
}

// insertTwoDistinctImages inserts two image rows (different shas + paths,
// older then newer) and returns their IDs as (older, newer).
func insertTwoDistinctImages(t *testing.T, database *db.DB) (older, newer int64) {
	t.Helper()
	r1, err := database.Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		VALUES ('/tmp/older.jpg', 'jpg', 1024, 'sha_older', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert older: %v", err)
	}
	older, _ = r1.LastInsertId()
	r2, err := database.Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		VALUES ('/tmp/newer.jpg', 'jpg', 1024, 'sha_newer', '2025-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert newer: %v", err)
	}
	newer, _ = r2.LastInsertId()
	return
}

func TestDeleteImage_RedirectsToNextInSearch(t *testing.T) {
	srv := newTestServer(t)
	older, newer := insertTwoDistinctImages(t, srv.db())
	h := srv.Handler()

	// Delete the newer image with a referring-search context. Under
	// newest-desc, the next image after the deleted one is the older one,
	// so the redirect should land on its detail page with back_* preserved.
	url := fmt.Sprintf("/images/%d?back_q=&back_sort=newest&back_order=desc", newer)
	req := httptest.NewRequest("DELETE", url, nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete expected 200, got %d", w.Code)
	}
	got := w.Header().Get("HX-Redirect")
	wantPrefix := fmt.Sprintf("/images/%d", older)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("redirect = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "back_sort=newest") || !strings.Contains(got, "back_order=desc") {
		t.Errorf("redirect %q should carry back_sort/back_order", got)
	}
}

func TestDeleteImage_FallsBackToGalleryOnLastImage(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db())
	h := srv.Handler()

	// Single image + back_* context: no next, no prev → fall back to gallery.
	url := fmt.Sprintf("/images/%d?back_q=&back_sort=newest&back_order=desc", id)
	req := httptest.NewRequest("DELETE", url, nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete expected 200, got %d", w.Code)
	}
	got := w.Header().Get("HX-Redirect")
	if strings.HasPrefix(got, "/images/") {
		t.Errorf("last-image redirect = %q, want gallery URL", got)
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

func TestSettingsGeneralPost(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	body := "_csrf=" + srv.csrfToken("anon") +
		"&watch_enabled=on&max_file_size_mb=200&page_size=60"
	req := httptest.NewRequest("POST", "/settings/general", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("settings general POST expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Saved") {
		t.Error("expected 'Saved' flash message")
	}
	if !srv.cfg.Gallery.WatchEnabled {
		t.Error("WatchEnabled should be true after save")
	}
	if srv.cfg.Gallery.MaxFileSizeMB != 200 {
		t.Errorf("MaxFileSizeMB = %d, want 200", srv.cfg.Gallery.MaxFileSizeMB)
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
	srv.db().Write.Exec(`
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
	srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images WHERE sha256 = 'prune_test_hash'`).Scan(&count)
	if count != 0 {
		t.Error("missing image should have been pruned")
	}
}

// newMultiGalleryServer builds a Server with two galleries so the switch,
// add, and topbar-dialog paths are exercised. Watchers stay off for tests.
func newMultiGalleryServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	g1 := filepath.Join(dir, "g1")
	g2 := filepath.Join(dir, "g2")
	os.MkdirAll(g1, 0o755)
	os.MkdirAll(g2, 0o755)

	cfg := config.Default()
	cfg.Paths.DataPath = filepath.Join(dir, "data")
	cfg.Gallery.WatchEnabled = false
	cfg.Galleries = []config.Gallery{
		{
			Name: "default", GalleryPath: g1,
			DBPath:         filepath.Join(cfg.Paths.DataPath, "default", "monbooru.db"),
			ThumbnailsPath: filepath.Join(cfg.Paths.DataPath, "default", "thumbnails"),
		},
		{
			Name: "stock", GalleryPath: g2,
			DBPath:         filepath.Join(cfg.Paths.DataPath, "stock", "monbooru.db"),
			ThumbnailsPath: filepath.Join(cfg.Paths.DataPath, "stock", "thumbnails"),
		},
	}
	cfg.DefaultGallery = "default"

	srv, err := NewServer(cfg, filepath.Join(dir, "monbooru.toml"), jobs.NewManager())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestGallerySwitcherButtonShownWithMultipleGalleries(t *testing.T) {
	srv := newMultiGalleryServer(t)
	h := srv.Handler()

	for _, path := range []string{"/", "/upload", "/categories"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		body := w.Body.String()
		if !strings.Contains(body, `id="gallery-switch-btn"`) {
			t.Errorf("%s: layout should render the gallery switcher button with 2+ galleries", path)
		}
		if !strings.Contains(body, `id="gallery-switch-dialog"`) {
			t.Errorf("%s: layout should render the gallery switch dialog with 2+ galleries", path)
		}
	}
}

func TestGallerySwitchChangesActive(t *testing.T) {
	srv := newMultiGalleryServer(t)
	h := srv.Handler()

	body := "_csrf=" + srv.csrfToken("anon") + "&name=stock"
	req := httptest.NewRequest("POST", "/internal/gallery/switch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("switch expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("HX-Refresh") != "true" {
		t.Error("switch should respond with HX-Refresh: true")
	}
	if srv.activeName != "stock" {
		t.Errorf("activeName = %q, want stock", srv.activeName)
	}
}

func TestGallerySwitch_RedirectsHomeFromSearch(t *testing.T) {
	srv := newMultiGalleryServer(t)
	h := srv.Handler()

	body := "_csrf=" + srv.csrfToken("anon") + "&name=stock"
	req := httptest.NewRequest("POST", "/internal/gallery/switch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost/?q=cat")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("switch expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("HX-Redirect = %q, want /", got)
	}
	if w.Header().Get("HX-Refresh") != "" {
		t.Error("search-bearing URL should redirect, not refresh in place")
	}
}

func TestGallerySwitcherHiddenWithSingleGallery(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `id="gallery-switch-btn"`) {
		t.Error("gallery switcher button should be hidden when only one gallery is configured")
	}
}
