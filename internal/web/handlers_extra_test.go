package web

import (
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/gallery"
)

// --- moveImage handler ------------------------------------------------------

func TestMoveImage_RejectsAbsolutePath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "mv.png", 10, 10)

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "folder": {"/etc/passwd"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/move", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for absolute folder path, got %d: %s", w.Code, w.Body.String())
	}
	// Canonical path must still be at the root.
	cx := srv.Active()
	var canonPath string
	cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonPath)
	if !strings.HasSuffix(canonPath, "mv.png") || strings.Contains(canonPath, "passwd") {
		t.Errorf("image moved to an unexpected path: %s", canonPath)
	}
}

func TestMoveImage_RejectsTraversal(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "mv2.png", 10, 10)

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "folder": {"../escape"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/move", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for traversal, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMoveImage_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "shot.png", 10, 10)

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "folder": {"archive/2026"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/move", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for HX move, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("HX-Redirect") == "" {
		t.Error("HX move should set HX-Redirect")
	}
	cx := srv.Active()
	var canonPath, folderPath string
	cx.DB.Read.QueryRow(`SELECT canonical_path, folder_path FROM images WHERE id = ?`, id).Scan(&canonPath, &folderPath)
	if folderPath != "archive/2026" {
		t.Errorf("folder_path = %q, want archive/2026", folderPath)
	}
	want := filepath.Join(cx.GalleryPath, "archive", "2026", "shot.png")
	if canonPath != want {
		t.Errorf("canonical_path = %q, want %q", canonPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at new path: %v", err)
	}
}

// --- foldersSuggest --------------------------------------------------------

func TestFoldersSuggest_PrefixFilter(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	// Seed three folder paths via direct file drops + ingest. Each image
	// needs a distinct SHA-256, so use the loop index to vary one pixel.
	for i, folder := range []string{"2024/jan", "2024/feb", "2025/mar"} {
		sub := filepath.Join(cx.GalleryPath, folder)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		name := strings.ReplaceAll(folder, "/", "_") + ".png"
		p := filepath.Join(sub, name)
		img := image.NewRGBA(image.Rect(0, 0, 12+i*3, 12+i))
		for px := 0; px < len(img.Pix); px += 4 {
			img.Pix[px] = byte(i * 50)
		}
		f, _ := os.Create(p)
		_ = png.Encode(f, img)
		f.Close()
		if _, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, p, "png", ""); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest("GET", "/internal/folders/suggest?prefix=2024", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "2024/jan") || !strings.Contains(body, "2024/feb") {
		t.Errorf("expected both 2024 folders in response, got %s", body)
	}
	if strings.Contains(body, "2025/mar") {
		t.Errorf("2025/mar must not match prefix=2024, got %s", body)
	}
}

// --- saved-search CRUD -----------------------------------------------------

func TestSavedSearch_CreateAndDelete(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.csrfToken("anon")

	// Create (HTMX form → 200 with flash-ok body).
	form := url.Values{"_csrf": {csrf}, "name": {"my_cats"}, "query": {"cat"}}
	req := httptest.NewRequest("POST", "/search/saved", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create saved search expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var id int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM saved_searches WHERE name='my_cats'`).Scan(&id); err != nil {
		t.Fatalf("saved search not persisted: %v", err)
	}

	// Delete.
	delReq := httptest.NewRequest("DELETE", fmt.Sprintf("/search/saved/%d", id), nil)
	delReq.Header.Set("X-CSRF-Token", csrf)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, delReq)
	if w2.Code != http.StatusOK {
		t.Errorf("delete saved search expected 200, got %d", w2.Code)
	}
	var count int
	srv.db().Read.QueryRow(`SELECT COUNT(*) FROM saved_searches`).Scan(&count)
	if count != 0 {
		t.Errorf("saved_searches should be empty after delete, got %d", count)
	}
}

// --- category CRUD --------------------------------------------------------

func TestCreateCategory_Post(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.csrfToken("anon")

	form := url.Values{"_csrf": {csrf}, "name": {"mood"}, "color": {"#abcdef"}}
	req := httptest.NewRequest("POST", "/tags/categories", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	// HTMX path returns 204 + HX-Redirect to /categories.
	if w.Code != http.StatusNoContent {
		t.Fatalf("create category (HTMX) expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("HX-Redirect"); loc != "/categories" {
		t.Errorf("HX-Redirect = %q, want /categories", loc)
	}
	var id int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name='mood'`).Scan(&id); err != nil {
		t.Fatalf("category not persisted: %v", err)
	}
}

// --- jobDismiss + jobCancel ------------------------------------------------

func TestJobDismissPost_ClearsDoneSummary(t *testing.T) {
	srv := newTestServer(t)
	// Stage a completed job.
	if err := srv.jobs.Start("sync"); err != nil {
		t.Fatal(err)
	}
	srv.jobs.Complete("done")

	csrf := srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/internal/job/dismiss", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("dismiss expected 204, got %d", w.Code)
	}
	if state := srv.jobs.Get(); state != nil {
		t.Errorf("state should be nil after dismiss, got %+v", state)
	}
}

func TestJobCancelPost_CancelsRunning(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.jobs.Start("delete"); err != nil {
		t.Fatal(err)
	}
	ctx := srv.jobs.Context()
	csrf := srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/internal/job/cancel", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("cancel expected 204, got %d", w.Code)
	}
	if ctx.Err() == nil {
		t.Error("Cancel should have fired the job's context")
	}
	srv.jobs.Complete("cancelled test")
}

// --- re-extract replaces existing metadata rows ---------------------------

func TestReExtract_ReplacesExistingMetadata(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()

	// Stage an image and fabricate an SD metadata row with a stale prompt.
	id := seedImage(t, srv, "reext.png", 10, 10)
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO sd_metadata (image_id, prompt, raw_params, generation_hash)
		 VALUES (?, 'stale_prompt', 'stale_params', 'stale_hash12')`, id,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(`UPDATE images SET source_type = 'a1111' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	// Fire the re-extract handler.
	csrf := srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/settings/maintenance/re-extract-metadata", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("re-extract expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Wait for the background job to drain. Poll the manager state with a
	// real time budget so the test isn't racing the goroutine.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.jobs.IsRunning() {
		t.Fatal("re-extract job never drained")
	}
	// The plain PNG has no real SD metadata so re-extraction should drop the
	// stale row and flip source_type back to "none".
	var count int
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM sd_metadata WHERE image_id = ?`, id).Scan(&count)
	if count != 0 {
		t.Errorf("re-extract should have cleared the stale sd_metadata row for a plain PNG, count = %d", count)
	}
	var sourceType string
	cx.DB.Read.QueryRow(`SELECT source_type FROM images WHERE id = ?`, id).Scan(&sourceType)
	if sourceType != "none" {
		t.Errorf("source_type after re-extract = %q, want 'none'", sourceType)
	}
}

// --- gallery switch lifecycle ---------------------------------------------

func TestGallerySwitch_RejectedWhileJobRunning(t *testing.T) {
	srv := newMultiGalleryServer(t)
	// Hold the job manager lock.
	if err := srv.jobs.Start("sync"); err != nil {
		t.Fatal(err)
	}
	defer srv.jobs.Complete("test")

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "name": {"stock"}}
	req := httptest.NewRequest("POST", "/internal/gallery/switch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Handler returns 400 with the "a job is running" message.
	if w.Code != http.StatusBadRequest {
		t.Errorf("switch while job running expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "job is running") {
		t.Errorf("expected 'job is running' in body, got: %s", w.Body.String())
	}
	if srv.activeName != "default" {
		t.Errorf("activeName should not have changed, got %q", srv.activeName)
	}
}

func TestGallerySwitch_UnknownGalleryRejected(t *testing.T) {
	srv := newMultiGalleryServer(t)
	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "name": {"does-not-exist"}}
	req := httptest.NewRequest("POST", "/internal/gallery/switch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code < 400 || w.Code >= 500 {
		t.Errorf("unknown gallery switch expected 4xx, got %d: %s", w.Code, w.Body.String())
	}
}

// --- addTagToImage colon-in-name handling --------------------------------

// imageTagCategory returns the category name of a tag attached to id whose
// name equals want, or "" if no such row exists. Used by the colon tests to
// confirm the parser kept the literal name instead of splitting it.
func imageTagCategory(t *testing.T, srv *Server, id int64, want string) string {
	t.Helper()
	cx := srv.Active()
	var cat string
	err := cx.DB.Read.QueryRow(
		`SELECT tc.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND t.name = ?`, id, want).Scan(&cat)
	if err != nil {
		return ""
	}
	return cat
}

func TestAddTagToImage_ColonFallbackLiteral(t *testing.T) {
	// `nier` is not a category, so the token must be stored whole in
	// general. Previously this path rejected the input as "unknown
	// category"; after the last commit it falls through to a literal
	// tag-name insert.
	srv := newTestServer(t)
	id := seedImage(t, srv, "colon_literal.png", 10, 10)

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "tag": {"nier:automata"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/tags", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if cat := imageTagCategory(t, srv, id, "nier:automata"); cat != "general" {
		t.Errorf("nier:automata category = %q, want general", cat)
	}
}

func TestAddTagToImage_RealCategoryPrefixStillSplits(t *testing.T) {
	// `artist` IS a built-in category, so `artist:john_doe` must still
	// be split into an artist-category `john_doe` tag - otherwise the
	// detail page loses category-qualified input entirely.
	srv := newTestServer(t)
	id := seedImage(t, srv, "colon_artist.png", 10, 10)

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "tag": {"artist:john_doe"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/tags", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if cat := imageTagCategory(t, srv, id, "john_doe"); cat != "artist" {
		t.Errorf("john_doe category = %q, want artist", cat)
	}
	if cat := imageTagCategory(t, srv, id, "artist:john_doe"); cat != "" {
		t.Errorf("literal artist:john_doe must not be stored, got category %q", cat)
	}
}
