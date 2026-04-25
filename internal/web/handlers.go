package web

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	meta "github.com/leqwin/monbooru/internal/metadata"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tagger"
	"github.com/leqwin/monbooru/internal/tags"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Auth.EnablePassword {
		// Render the login form with an inline notice and the input/submit
		// disabled instead of silently redirecting - a user who bookmarked
		// /login after disabling auth otherwise gets no explanation for why
		// the page vanished, and leaving the fields live makes it look like
		// the 'login' somehow worked when the server just redirects to /.
		s.renderTemplate(w, "login.html", map[string]any{
			"CSRFToken":    s.csrfToken("anon"),
			"Error":        "Password authentication is disabled. Enable it from Settings → Authentication.",
			"AuthDisabled": true,
		})
		return
	}
	s.renderTemplate(w, "login.html", map[string]any{
		"CSRFToken": s.csrfToken("anon"),
	})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Auth.EnablePassword {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	ip := clientIP(r)
	if !s.loginRL.check(ip) {
		logx.Warnf("login rate-limited from %s", ip)
		s.renderTemplate(w, "login.html", map[string]any{
			"Error":     "Too many attempts. Please wait before trying again.",
			"CSRFToken": s.csrfToken("anon"),
		})
		return
	}

	password := r.FormValue("password")
	if err := bcrypt.CompareHashAndPassword(
		[]byte(s.cfg.Auth.PasswordHash), []byte(password),
	); err != nil {
		s.loginRL.recordFailure(ip)
		logx.Warnf("login failed from %s", ip)
		s.renderTemplate(w, "login.html", map[string]any{
			"Error":     "Invalid password",
			"CSRFToken": s.csrfToken("anon"),
		})
		return
	}
	s.loginRL.recordSuccess(ip)
	logx.Infof("login success from %s", ip)

	sessID, err := s.sessions.NewSession(s.cfg.Auth.SessionLifetimeDays)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "monbooru_session",
		Value:    sessID,
		Path:     "/",
		MaxAge:   s.cfg.Auth.SessionLifetimeDays * 86400,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	sessID := sessionFromContext(r.Context())
	s.sessions.DeleteSession(sessID)
	http.SetCookie(w, &http.Cookie{
		Name:   "monbooru_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type galleryData struct {
	baseData
	Query         string
	Sort          string
	Order         string
	RandomSeed    int64
	Page          int
	TotalPages    int
	Result        *models.SearchResult
	SidebarTags   []models.Tag
	FolderTree    []gallery.FolderNode
	SourceCounts  gallery.SourceCounts
	SavedSearches []models.SavedSearch
	SidebarURL    string // populated on full-page renders so the placeholder can lazy-load the sidebar
}

func (s *Server) galleryHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "newest"
	}
	orderStr := q.Get("order")
	if orderStr == "" {
		orderStr = "desc"
	}
	pageStr := q.Get("page")
	page := 1
	if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
		page = p
	}

	// For random sort, use a stable seed so the order doesn't change on every reload.
	// Generate a seed if not present in the URL, and redirect to add it (or set HX-Push-Url).
	var randomSeed int64
	if sortStr == "random" {
		if seedStr := q.Get("seed"); seedStr != "" {
			if s, err := strconv.ParseInt(seedStr, 10, 64); err == nil && s != 0 {
				randomSeed = s
			}
		}
		if randomSeed == 0 {
			seedBytes := make([]byte, 8)
			if _, err := rand.Read(seedBytes); err == nil {
				randomSeed = int64(binary.BigEndian.Uint64(seedBytes) >> 1) // >>1 to keep positive
			} else {
				randomSeed = time.Now().UnixNano()
			}
			if randomSeed < 0 {
				randomSeed = -randomSeed
			}
			newQ := r.URL.Query()
			newQ.Set("seed", strconv.FormatInt(randomSeed, 10))
			if isHTMXRequest(r) {
				// Push URL with seed so the next poll keeps the same order.
				w.Header().Set("HX-Push-Url", "/?"+newQ.Encode())
			} else {
				http.Redirect(w, r, "/?"+newQ.Encode(), http.StatusSeeOther)
				return
			}
		}
	}

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		logx.Warnf("gallery search parse: %v", parseErr)
	}
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		RandomSeed: randomSeed,
		Page:       page,
		Limit:      s.cfg.UI.PageSize,
	}
	// Unfiltered browse hits the full-visible count on every page; serve it
	// from the per-gallery cache to skip the O(N) index scan.
	if expr == nil {
		if cx := s.Active(); cx != nil {
			if n, err := cx.VisibleCount(); err == nil {
				sq.PresetTotal = &n
			}
		}
	}

	htmxGridTarget := isHTMXRequest(r) && r.Header.Get("HX-Target") == "gallery-grid"

	result, err := search.Execute(s.db(), sq)
	if err != nil {
		logx.Errorf("gallery search: %v", err)
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}

	totalPages := 1
	if s.cfg.UI.PageSize > 0 {
		totalPages = (result.Total + s.cfg.UI.PageSize - 1) / s.cfg.UI.PageSize
	}

	// If a concurrent ingestion or delete shrank the result set out from under
	// the user's current page (e.g. the auto-refresh re-fetches page 3 after
	// deletions dropped the total to 1 page), re-run at the last valid page
	// so the grid isn't empty while "N images" still shows a non-zero count.
	if result.Total > 0 && page > totalPages {
		page = totalPages
		sq.Page = page
		result, err = search.Execute(s.db(), sq)
		if err != nil {
			logx.Errorf("gallery search (clamped): %v", err)
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
	}

	// Full-page renders ship the sidebar as a placeholder that lazy-loads via
	// GET /internal/sidebar, so first paint isn't blocked on the folder-tree
	// aggregation. Search/pagination HTMX responses still need the sidebar
	// content in the same payload because gallery_htmx.html OOB-swaps it into
	// the live page.
	var (
		sidebarTags   []models.Tag
		folderTree    []gallery.FolderNode
		sourceCounts  gallery.SourceCounts
		savedSearches []models.SavedSearch
	)
	if htmxGridTarget {
		ids := make([]int64, 0, len(result.Results))
		for _, img := range result.Results {
			ids = append(ids, img.ID)
		}
		sidebarTags, folderTree, sourceCounts, savedSearches = s.sidebarLoad(ids)
	}

	data := galleryData{
		baseData:      s.base(r, "gallery", "Images - Monbooru"),
		Query:         queryStr,
		Sort:          sortStr,
		Order:         orderStr,
		RandomSeed:    randomSeed,
		Page:          page,
		TotalPages:    totalPages,
		Result:        result,
		SidebarTags:   sidebarTags,
		FolderTree:    folderTree,
		SourceCounts:  sourceCounts,
		SavedSearches: savedSearches,
	}

	if htmxGridTarget {
		s.renderTemplate(w, "partials/gallery_htmx.html", data)
		return
	}
	data.SidebarURL = buildSidebarURL(queryStr, sortStr, orderStr, pageStr, q.Get("seed"))
	s.renderTemplate(w, "gallery.html", data)
}

// sidebarLoad runs the reads that populate the gallery sidebar - current-page
// tags, folder tree, source counts, saved searches - in parallel across the
// read pool. Called from galleryHandler on HTMX grid swaps (to keep the OOB
// sidebar update) and from gallerySidebar (lazy-load on full-page render).
func (s *Server) sidebarLoad(pageImageIDs []int64) ([]models.Tag, []gallery.FolderNode, gallery.SourceCounts, []models.SavedSearch) {
	var (
		tags    []models.Tag
		folders []gallery.FolderNode
		sources gallery.SourceCounts
		saved   []models.SavedSearch
	)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		tags, _ = search.SidebarTagsWithGlobalCount(s.db(), pageImageIDs)
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			folders, _ = cx.FolderTree()
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			sources, _ = cx.SourceCounts()
		}
	}()
	go func() {
		defer wg.Done()
		ssRows, ssErr := s.db().Read.Query(`SELECT id, name, query FROM saved_searches ORDER BY name`)
		if ssErr != nil {
			return
		}
		defer ssRows.Close()
		for ssRows.Next() {
			var ss models.SavedSearch
			if err := ssRows.Scan(&ss.ID, &ss.Name, &ss.Query); err != nil {
				logx.Warnf("sidebar saved searches scan: %v", err)
				continue
			}
			saved = append(saved, ss)
		}
	}()
	wg.Wait()
	return tags, folders, sources, saved
}

// gallerySidebar renders the gallery sidebar partial on demand. Initial
// full-page gallery renders ship an empty #sidebar-inner placeholder that
// hx-gets this endpoint on load - same pattern as the detail page's
// related-images lazy fetch.
func (s *Server) gallerySidebar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "newest"
	}
	orderStr := q.Get("order")
	if orderStr == "" {
		orderStr = "desc"
	}
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}
	var randomSeed int64
	if sortStr == "random" {
		if seed, err := strconv.ParseInt(q.Get("seed"), 10, 64); err == nil {
			randomSeed = seed
		}
	}

	expr, _ := search.Parse(queryStr)
	// Sidebar render only needs the page's image IDs to build per-page tag
	// aggregation; Total is never surfaced. Skipping COUNT(*) cuts the work
	// on filtered searches where the count pass is itself a full filter
	// evaluation.
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		RandomSeed: randomSeed,
		Page:       page,
		Limit:      s.cfg.UI.PageSize,
		SkipCount:  true,
	}

	result, err := search.Execute(s.db(), sq)
	if err != nil {
		logx.Errorf("sidebar search: %v", err)
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}

	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}
	sidebarTags, folderTree, sourceCounts, savedSearches := s.sidebarLoad(ids)

	s.renderTemplate(w, "partials/sidebar_content.html", map[string]any{
		"Query":         queryStr,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"SidebarTags":   sidebarTags,
		"FolderTree":    folderTree,
		"SourceCounts":  sourceCounts,
		"SavedSearches": savedSearches,
	})
}

// sidebarBrowse renders the folder/source/saved-searches sections only -
// no per-page tag groups. Lazy-loaded from the detail page so its sidebar
// gets the same browse shortcuts the gallery sidebar does without paying
// the folder-tree aggregation cost on first paint.
func (s *Server) sidebarBrowse(w http.ResponseWriter, r *http.Request) {
	queryStr := r.URL.Query().Get("q")

	var (
		folders []gallery.FolderNode
		sources gallery.SourceCounts
		saved   []models.SavedSearch
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			folders, _ = cx.FolderTree()
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			sources, _ = cx.SourceCounts()
		}
	}()
	go func() {
		defer wg.Done()
		rows, err := s.db().Read.Query(`SELECT id, name, query FROM saved_searches ORDER BY name`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var ss models.SavedSearch
			if err := rows.Scan(&ss.ID, &ss.Name, &ss.Query); err != nil {
				logx.Warnf("sidebar-browse saved searches scan: %v", err)
				continue
			}
			saved = append(saved, ss)
		}
	}()
	wg.Wait()

	s.renderTemplate(w, "partials/sidebar_browse.html", map[string]any{
		"Query":         queryStr,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"FolderTree":    folders,
		"SourceCounts":  sources,
		"SavedSearches": saved,
	})
}

// buildSidebarURL constructs the URL the #sidebar-inner placeholder hx-gets
// on full-page gallery renders, mirroring buildGalleryURL's encoding style
// so the sidebar handler sees the same q/sort/order/page/seed the page does.
func buildSidebarURL(q, sort, order, page, seed string) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if sort != "" {
		v.Set("sort", sort)
	}
	if order != "" {
		v.Set("order", order)
	}
	if page != "" {
		v.Set("page", page)
	}
	if seed != "" {
		v.Set("seed", seed)
	}
	if len(v) == 0 {
		return "/internal/sidebar"
	}
	return "/internal/sidebar?" + v.Encode()
}

type detailData struct {
	baseData
	Image          models.Image
	Filename       string // basename of the canonical path, shown on the detail page topbar
	ImageTags      []models.ImageTag
	SDMeta         *models.SDMetadata
	ComfyMeta      *models.ComfyUIMetadata
	ComfyNodes     []models.ComfyNode
	GenericMeta    []models.SDParam
	ImagePaths     []models.ImagePath
	ThumbnailURL   string
	PrevID         *int64
	NextID         *int64
	Position       int    // 1-based rank of Image within the referring search; 0 = unknown (no back-* context)
	PositionTotal  int    // total matching rows in the referring search
	RefURL         string // predecessor detail URL when the user arrived via a Similar-images click; drives the "← Previous image" back link and Escape
	Ref            string // raw ref=<sourceID> value when valid; forwarded on the delete button so the post-delete redirect returns to the source instead of an arbitrary neighbour
	BackQuery      string
	BackSort       string
	BackOrder      string
	BackPage       string
	BackSeed       string
	EnabledTaggers []tagger.TaggerStatus // enabled+available taggers offered in the auto-tag control
	ImageTaggers   []string              // distinct tagger names currently on this image's auto-tags
	HasUserTags    bool                  // true when at least one manual (non-auto) tag is on this image
}

func (s *Server) detailHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	img, err := loadImage(ctx, s.db(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Prev/next navigation is only computed when the referring gallery context
	// is carried through via back_* query params. Resolve the values now so
	// the parallel block below can launch the adjacency lookup alongside the
	// other reads instead of after them.
	backQ := r.URL.Query().Get("back_q")
	backSort := r.URL.Query().Get("back_sort")
	backOrder := r.URL.Query().Get("back_order")
	backPage := r.URL.Query().Get("back_page")
	backSeed := r.URL.Query().Get("back_seed")

	// A "ref" query param points at the detail page the user just came from
	// (a Similar-images click). When set and valid, the gallery-context UI
	// (X/Y counter, prev/next arrows, "← Images" back link) is suppressed
	// because the user just switched contexts - the current image may not
	// even be in the referring search. back_* still flows through so the
	// rebuilt refURL lands the user back on the source with its original
	// gallery context when they click "← Previous image".
	refURL := ""
	refStrValid := ""
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if refID, err := strconv.ParseInt(refStr, 10, 64); err == nil && refID != id {
			refURL = buildDetailURL(refID, backQ, backSort, backOrder, backPage, backSeed)
			refStrValid = strconv.FormatInt(refID, 10)
		}
	}

	wantAdjacent := refURL == "" && (backSort != "" || backQ != "")
	if wantAdjacent {
		if backSort == "" {
			backSort = "newest"
		}
		if backOrder == "" {
			backOrder = "desc"
		}
	}

	// The remaining reads are independent of each other and target different
	// tables (or the filesystem for ExtractGeneric). Run them in parallel
	// across the read pool. Related images are fetched lazily via
	// /images/{id}/related so the page paints before that join finishes -
	// on libraries with millions of image_tags rows it was the single
	// largest contributor to detail-page latency. comfyNodes parsing stays
	// sequential - it's pure CPU on the comfyMeta payload and only matters
	// once that read returns.
	var (
		imageTags     []models.ImageTag
		sdMeta        *models.SDMetadata
		comfyMeta     *models.ComfyUIMetadata
		genericMeta   []models.SDParam
		imagePaths    []models.ImagePath
		prevID        *int64
		nextID        *int64
		position      int
		positionTotal int
	)
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); imageTags, _ = s.tagSvc().GetImageTags(id) }()
	go func() { defer wg.Done(); sdMeta = loadSDMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); comfyMeta = loadComfyMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); imagePaths = loadImagePaths(ctx, s.db(), id) }()
	go func() {
		defer wg.Done()
		genericMeta = meta.ExtractGeneric(img.CanonicalPath, img.FileType)
	}()
	if wantAdjacent {
		wg.Add(2)
		go func() {
			defer wg.Done()
			prevID, nextID = s.findAdjacentImages(id, backQ, backSort, backOrder, backSeed)
		}()
		go func() {
			defer wg.Done()
			position, positionTotal = s.findImagePosition(id, backQ, backSort, backOrder, backSeed)
		}()
	}
	wg.Wait()

	var comfyNodes []models.ComfyNode
	if comfyMeta != nil && comfyMeta.RawWorkflow != "" {
		comfyNodes = meta.ParseComfyWorkflowNodes(comfyMeta.RawWorkflow)
	}

	enabledTaggers := tagger.EnabledTaggers(s.cfg)
	imageTaggers := distinctAutoTaggerNames(imageTags)
	hasUserTags := false
	for _, t := range imageTags {
		if !t.IsAuto {
			hasUserTags = true
			break
		}
	}

	baseName := filepath.Base(img.CanonicalPath)
	data := detailData{
		baseData:       s.base(r, "gallery", fmt.Sprintf("%s - Monbooru", baseName)),
		Image:          *img,
		Filename:       baseName,
		ImageTags:      imageTags,
		SDMeta:         sdMeta,
		ComfyMeta:      comfyMeta,
		ComfyNodes:     comfyNodes,
		GenericMeta:    genericMeta,
		ImagePaths:     imagePaths,
		ThumbnailURL:   fmt.Sprintf("/thumbnails/%s/%d.jpg", s.activeName, id),
		PrevID:         prevID,
		NextID:         nextID,
		Position:       position,
		PositionTotal:  positionTotal,
		RefURL:         refURL,
		Ref:            refStrValid,
		BackQuery:      backQ,
		BackSort:       backSort,
		BackOrder:      backOrder,
		BackPage:       backPage,
		BackSeed:       backSeed,
		EnabledTaggers: enabledTaggers,
		ImageTaggers:   imageTaggers,
		HasUserTags:    hasUserTags,
	}
	s.renderTemplate(w, "detail.html", data)
}

// relatedImagesHandler returns the Similar-images mini-grid for an image,
// fetched lazily from the detail page so the initial render isn't blocked
// on the shared-tag aggregation over image_tags.
func (s *Server) relatedImagesHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	related, _ := s.tagSvc().RelatedImages(id, 9)
	q := r.URL.Query()
	s.renderTemplate(w, "partials/related_images.html", map[string]any{
		"Images":        related,
		"ActiveGallery": s.activeName,
		// Each similar-image link carries ref=<current image> so the
		// destination detail page swaps "← Images" for "← Previous image"
		// (pointing back here) and Escape walks browser history. back_*
		// flow through so that "← Previous image" link restores this
		// page's own gallery context when clicked.
		"SourceID":  id,
		"BackQuery": q.Get("back_q"),
		"BackSort":  q.Get("back_sort"),
		"BackOrder": q.Get("back_order"),
		"BackPage":  q.Get("back_page"),
		"BackSeed":  q.Get("back_seed"),
	})
}

// distinctAutoTaggerNames returns the unique tagger names seen in the
// image's auto-tag rows, preserving the first-seen order from the sorted
// tag list.
func distinctAutoTaggerNames(tags []models.ImageTag) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		if !t.IsAuto || t.TaggerName == "" || seen[t.TaggerName] {
			continue
		}
		seen[t.TaggerName] = true
		out = append(out, t.TaggerName)
	}
	return out
}

// catTag pairs a resolved category ID with a tag name for creation/application.
type catTag struct {
	catID int64
	name  string
}

// parseTagInput parses multi-token tag input.
//
// Tokens are separated by whitespace. Each token becomes its own tag: a
// bare word, a "category:name" pair, or a double-quoted span whose
// internal spaces are collapsed to underscores (so `"red hair"` →
// `red_hair`). Quotes can follow a category prefix
// (`artist:"john doe"`).
//
// Examples:
//
//	red hair                 -> [{general, "red"}, {general, "hair"}]
//	"red hair" blue_eyes     -> [{general, "red_hair"}, {general, "blue_eyes"}]
//	artist:"john doe" 1girl  -> [{artist, "john_doe"}, {general, "1girl"}]
func (s *Server) parseTagInput(tagInput string) ([]catTag, string) {
	tokens, err := splitTagTokens(tagInput)
	if err != nil {
		return nil, err.Error()
	}

	var generalID int64
	s.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)

	var catTags []catTag
	for _, tok := range tokens {
		name := tok.name
		if idx := strings.Index(name, ":"); idx > 0 {
			catName := name[:idx]
			tagName := name[idx+1:]
			var catID int64
			if err := s.db().Read.QueryRow(
				`SELECT id FROM tag_categories WHERE name=?`, catName,
			).Scan(&catID); err == nil {
				if tagName != "" {
					catTags = append(catTags, catTag{catID, tagName})
				}
				continue
			}
			// Prefix isn't a known category; treat the whole token as a
			// literal general-category tag (e.g. "nier:automata").
		}
		catTags = append(catTags, catTag{generalID, name})
	}

	return catTags, ""
}

// parsedTagToken is one tokenizer output: its resolved name.
type parsedTagToken struct {
	name string
}

// splitTagTokens splits tag-input into whitespace-separated tokens while
// respecting double-quoted spans. Inside a quoted span, internal spaces
// are replaced with underscores. Quoted spans may be preceded by a
// category prefix (`artist:"john doe"`). Unterminated quotes return an
// error.
func splitTagTokens(s string) ([]parsedTagToken, error) {
	var tokens []parsedTagToken
	var buf strings.Builder
	quoted := false
	inToken := false

	flush := func() {
		if !inToken {
			return
		}
		tokens = append(tokens, parsedTagToken{name: buf.String()})
		buf.Reset()
		inToken = false
	}

	for _, r := range s {
		if r == '"' {
			quoted = !quoted
			inToken = true
			continue
		}
		if quoted {
			if r == ' ' || r == '\t' {
				buf.WriteRune('_')
			} else {
				buf.WriteRune(r)
			}
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			flush()
			continue
		}
		buf.WriteRune(r)
		inToken = true
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quote in tag input")
	}
	flush()
	return tokens, nil
}

func (s *Server) addTagToImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tag"))
	if tagInput == "" {
		http.Error(w, "tag required", http.StatusBadRequest)
		return
	}

	catTags, parseErrMsg := s.parseTagInput(tagInput)

	addErrMsg := parseErrMsg
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			logx.Warnf("add tag %q: %v", ct.name, err)
			addErrMsg = err.Error()
			continue
		}
		// Single write-pool transaction does the INSERT OR IGNORE and
		// reports whether the row was actually new, so parallel adds
		// can't both claim they were the first writer.
		added, err := s.tagSvc().AddTagToImageReportingDup(id, tag.ID, false, nil, "")
		if err != nil {
			logx.Warnf("add tag %q to image %d: %v", ct.name, id, err)
			addErrMsg = err.Error()
			continue
		}
		if !added {
			addErrMsg = "tag already on image: " + ct.name
		}
	}

	s.renderTagListWithSidebar(w, r, id, addErrMsg, addErrMsg == "")
}

// renderTagListWithSidebar renders the image tag list partial and always emits
// OOB swaps of the detail sidebar and danger zone so tag groups and remove-tag
// buttons stay in sync without a page reload.
// errMsg is shown as an inline flash if non-empty; clearInput resets the add-tag input.
func (s *Server) renderTagListWithSidebar(w http.ResponseWriter, r *http.Request, id int64, errMsg string, clearInput bool) {
	imageTags, _ := s.tagSvc().GetImageTags(id)
	hasUserTags := false
	for _, t := range imageTags {
		if !t.IsAuto {
			hasUserTags = true
			break
		}
	}
	var folderPath string
	_ = s.db().Read.QueryRow(`SELECT folder_path FROM images WHERE id = ?`, id).Scan(&folderPath)
	q := r.URL.Query()
	s.renderTemplate(w, "partials/tag_list.html", map[string]any{
		"ImageID":       id,
		"ImageTags":     imageTags,
		"SidebarTags":   true,
		"DangerZone":    true,
		"HasUserTags":   hasUserTags,
		"ImageTaggers":  distinctAutoTaggerNames(imageTags),
		"BackQuery":     q.Get("back_q"),
		"BackSort":      q.Get("back_sort"),
		"BackOrder":     q.Get("back_order"),
		"BackPage":      q.Get("back_page"),
		"BackSeed":      q.Get("back_seed"),
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"EditMode":      true,
		"ErrMsg":        errMsg,
		"ClearInput":    clearInput,
		"CurrentFolder": folderPath,
	})
}

// removeAutoTagsFromImageHandler removes auto-tagged rows from one image,
// optionally filtered by the caller-supplied `taggers` query parameter
// (comma-separated tagger names). Empty filter removes every auto-tag.
func (s *Server) removeAutoTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	raw := r.URL.Query().Get("taggers")
	var names []string
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if err := s.tagSvc().RemoveAutoTagsFromImage(id, names); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", false)
}

func (s *Server) removeUserTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RemoveUserTagsFromImage(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", false)
}

func (s *Server) removeAllTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RemoveAllTagsFromImage(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", false)
}

func (s *Server) removeTagFromImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	tagIDStr := r.PathValue("tagID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tagID, err := strconv.ParseInt(tagIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad tagID", http.StatusBadRequest)
		return
	}

	if err := s.tagSvc().RemoveTagFromImage(id, tagID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", false)
}

func (s *Server) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Toggle atomically and read the new value in a single statement using RETURNING,
	// avoiding a separate read query and any read-after-write race.
	var newFav int
	if err := s.db().Write.QueryRow(
		`UPDATE images SET is_favorited = 1 - is_favorited WHERE id = ? RETURNING is_favorited`, id,
	).Scan(&newFav); err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if newFav == 1 {
		w.Write([]byte(`<button type="submit" id="fav-btn" class="btn-fav active" title="Unfavorite">★</button>`))
	} else {
		w.Write([]byte(`<button type="submit" id="fav-btn" class="btn-fav" title="Favorite">☆</button>`))
	}
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	backQ := r.URL.Query().Get("back_q")
	backSort := r.URL.Query().Get("back_sort")
	backOrder := r.URL.Query().Get("back_order")
	backPage := r.URL.Query().Get("back_page")
	backSeed := r.URL.Query().Get("back_seed")

	// When the caller arrived via a Similar-images click the URL carries
	// ref=<sourceID> and the back_* params describe the source's gallery
	// context, not a search the current image is part of. Snapshotting the
	// valid source id up front keeps the post-delete redirect aimed at the
	// original image instead of jumping to an arbitrary neighbour of the
	// unrelated back_* search.
	var refID *int64
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if parsed, err := strconv.ParseInt(refStr, 10, 64); err == nil && parsed != id {
			refID = &parsed
		}
	}

	// Compute the neighbour before the delete so we don't miss it once the
	// current row is gone. When the caller carried back_* params the detail
	// page had a search context; we keep the user in that stream by jumping
	// to the adjacent image instead of bouncing back to the gallery. Ref
	// takes precedence over adjacency: the current image may not even be in
	// the referring search.
	var prevID, nextID *int64
	if refID == nil && (backSort != "" || backQ != "") {
		sortStr := backSort
		if sortStr == "" {
			sortStr = "newest"
		}
		orderStr := backOrder
		if orderStr == "" {
			orderStr = "desc"
		}
		prevID, nextID = s.findAdjacentImages(id, backQ, sortStr, orderStr, backSeed)
	}

	result, err := gallery.DeleteImage(s.db(), s.thumbnailsPath(), id, s.tagSvc().RemoveAllTagsFromImage)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.Active().InvalidateCaches()

	if !result.IsMissing {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), result.FolderPath)
	}

	redirectURL := ""
	switch {
	case refID != nil:
		redirectURL = buildDetailURL(*refID, backQ, backSort, backOrder, backPage, backSeed)
	case nextID != nil:
		redirectURL = buildDetailURL(*nextID, backQ, backSort, backOrder, backPage, backSeed)
	case prevID != nil:
		redirectURL = buildDetailURL(*prevID, backQ, backSort, backOrder, backPage, backSeed)
	default:
		redirectURL = buildGalleryURL(backQ, backSort, backOrder, backPage, backSeed)
	}

	if isHTMXRequest(r) {
		// Ref case: the user arrived here via a Similar-images click, which
		// itself may be any depth into a chain. Redirecting to the source
		// would push a fresh history entry that drops the ref chain - the
		// post-delete source page then has no data-ref and Escape escapes
		// straight to the gallery. Fire a delete-go-back trigger instead so
		// the client can prefer history.back(), landing on the source's
		// original URL (with its own ref intact) and keeping the chain
		// walkable. The fallback URL handles the cold-load case where the
		// browser has no predecessor (direct link, bookmarked tab).
		if refID != nil {
			w.Header().Set("HX-Trigger", fmt.Sprintf(`{"delete-go-back":{"fallback":%q}}`, redirectURL))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("HX-Redirect", redirectURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// buildDetailURL constructs a detail-page URL with back_* params so the
// destination page keeps the same gallery context (prev/next adjacency,
// back-link target) the user came in with.
func buildDetailURL(id int64, q, sort, order, page, seed string) string {
	base := fmt.Sprintf("/images/%d", id)
	v := url.Values{}
	if q != "" {
		v.Set("back_q", q)
	}
	if sort != "" {
		v.Set("back_sort", sort)
	}
	if order != "" {
		v.Set("back_order", order)
	}
	if page != "" {
		v.Set("back_page", page)
	}
	if seed != "" {
		v.Set("back_seed", seed)
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

func (s *Server) promoteCanonical(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	newCanonical := r.FormValue("path")
	if newCanonical == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	// Refuse to promote anything that isn't already a tracked alias of this
	// image. Without this check, a user could write an arbitrary string
	// into images.canonical_path and coerce serveImageFile into serving a
	// sibling file whose path happens to live inside the gallery root.
	var aliasExists int
	if err := s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_paths WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	).Scan(&aliasExists); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if aliasExists == 0 {
		http.Error(w, "path is not an alias of this image", http.StatusBadRequest)
		return
	}

	newFolder := gallery.FolderPath(s.galleryPath(), newCanonical)

	tx, err := s.db().Write.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ?`, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
		newCanonical, newFolder, id,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	pathIDStr := r.PathValue("pathID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	pathID, err := strconv.ParseInt(pathIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad pathID", http.StatusBadRequest)
		return
	}

	// Refuse to remove the canonical path; callers must promote another alias
	// first, otherwise the image would lose its on-disk reference entirely.
	var isCanon int
	var aliasPath string
	s.db().Read.QueryRow(
		`SELECT is_canonical, path FROM image_paths WHERE id = ? AND image_id = ?`, pathID, id,
	).Scan(&isCanon, &aliasPath)
	if isCanon == 1 {
		http.Error(w, "cannot delete canonical path", http.StatusBadRequest)
		return
	}

	s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, pathID)

	if aliasPath != "" {
		if err := os.Remove(aliasPath); err != nil && !os.IsNotExist(err) {
			logx.Warnf("delete alias file %q: %v", aliasPath, err)
		}
	}

	if isHTMXRequest(r) {
		// Empty body for HTMX outerHTML swap - removes the row.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(""))
		return
	}
	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

// tagsPageData embeds baseData so the layout template sees its fields as
// struct members (matching galleryData / detailData) and the tags template
// can reach its own state via direct field access.
type tagsPageData struct {
	baseData
	Tags       []models.Tag
	Categories []models.TagCategory
	Total      int
	Page       int
	TotalPages int
	CategoryID string
	Prefix     string
	Sort       string
	Origin     string
}

func (s *Server) tagsHandler(w http.ResponseWriter, r *http.Request) {
	// The tags page reflects rapidly-changing state (category re-assignment,
	// merges). Opt out of browser caching so a reload after a mutation never
	// serves a stale render.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	catIDStr := q.Get("cat")
	prefix := q.Get("q")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "usage"
	}
	originStr := q.Get("origin")
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}

	filter := s.buildTagFilter(catIDStr, prefix, sortStr, originStr, page, 100)

	tagList, total, err := s.tagSvc().ListTags(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cats, _ := s.tagSvc().ListCategories()
	totalPages := (total + 99) / 100

	data := tagsPageData{
		baseData:   s.base(r, "tags", "Tags - Monbooru"),
		Tags:       tagList,
		Categories: cats,
		Total:      total,
		Page:       page,
		TotalPages: totalPages,
		CategoryID: catIDStr,
		Prefix:     prefix,
		Sort:       sortStr,
		Origin:     originStr,
	}
	s.renderTemplate(w, "tags.html", data)
}

func (s *Server) buildTagFilter(catIDStr, prefix, sortStr, originStr string, page, limit int) tags.TagFilter {
	f := tags.TagFilter{
		Prefix:    prefix,
		Sort:      sortStr,
		PageIndex: page - 1,
		Limit:     limit,
		Origin:    originStr,
	}
	if catIDStr != "" {
		if id, err := strconv.ParseInt(catIDStr, 10, 64); err == nil {
			f.CategoryID = &id
		}
	}
	return f
}

func (s *Server) mergeTagsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	aliasIDStr := r.FormValue("alias_id")
	canonInput := strings.TrimSpace(r.FormValue("canonical_id"))

	aliasID, err := strconv.ParseInt(aliasIDStr, 10, 64)
	if err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">Invalid source tag.</div>`))
			return
		}
		http.Error(w, "bad alias id", http.StatusBadRequest)
		return
	}

	// Accept canonical_id as a tag ID, "category:name", or a plain name.
	// Plain names must be unique across categories.
	var canonID int64
	mergeErr := func(msg string) {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(msg) + `</div>`))
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
	}
	if id, err := strconv.ParseInt(canonInput, 10, 64); err == nil {
		canonID = id
	} else if idx := strings.Index(canonInput, ":"); idx > 0 && s.categoryExists(canonInput[:idx]) {
		catName := canonInput[:idx]
		tagName := canonInput[idx+1:]
		if err := s.db().Read.QueryRow(
			`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
			 WHERE t.name = ? AND tc.name = ?`, tagName, catName,
		).Scan(&canonID); err != nil {
			mergeErr("Tag not found: " + canonInput)
			return
		}
	} else {
		rows, err := s.db().Read.Query(`SELECT id FROM tags WHERE name = ?`, canonInput)
		if err != nil {
			mergeErr("Tag lookup failed: " + err.Error())
			return
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				logx.Warnf("merge tags lookup scan: %v", err)
				continue
			}
			ids = append(ids, id)
		}
		rows.Close()
		switch len(ids) {
		case 0:
			mergeErr("Tag not found: " + canonInput)
			return
		case 1:
			canonID = ids[0]
		default:
			mergeErr("Tag name " + canonInput + " exists in multiple categories; use category:name or the tag ID")
			return
		}
	}

	if err := s.tagSvc().MergeTags(aliasID, canonID); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/tags")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) categoriesHandler(w http.ResponseWriter, r *http.Request) {
	cats, err := s.tagSvc().ListCategories()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	base := s.base(r, "categories", "Categories - Monbooru")
	data := map[string]any{
		"Title":         base.Title,
		"ActiveNav":     base.ActiveNav,
		"CSRFToken":     base.CSRFToken,
		"AuthEnabled":   base.AuthEnabled,
		"Degraded":      base.Degraded,
		"Version":       base.Version,
		"RepoURL":       base.RepoURL,
		"Variant":       base.Variant,
		"ActiveGallery": base.ActiveGallery,
		"Galleries":     s.galleryList(),
		"VisibleCount":  base.VisibleCount,
		"TagCount":      base.TagCount,
		"SavedCount":    base.SavedCount,
		"Categories":    cats,
	}
	s.renderTemplate(w, "categories.html", data)
}

func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	base := s.base(r, "settings", "Settings - Monbooru")
	taggers := tagger.AvailableTaggers(s.cfg)
	enabledCount := 0
	for _, t := range taggers {
		if t.Enabled && t.Available {
			enabledCount++
		}
	}
	data := map[string]any{
		"Title":          base.Title,
		"ActiveNav":      base.ActiveNav,
		"CSRFToken":      base.CSRFToken,
		"AuthEnabled":    base.AuthEnabled,
		"Degraded":       base.Degraded,
		"Version":        base.Version,
		"RepoURL":        base.RepoURL,
		"Variant":        base.Variant,
		"ActiveGallery":  base.ActiveGallery,
		"Galleries":      s.galleryList(),
		"VisibleCount":   base.VisibleCount,
		"TagCount":       base.TagCount,
		"SavedCount":     base.SavedCount,
		"Config":         s.cfg,
		"Taggers":        taggers,
		"EnabledCount":   enabledCount,
		"ScheduleStatus": s.ScheduleStatus(),
	}
	s.renderTemplate(w, "settings.html", data)
}

func (s *Server) settingsSchedulePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	timeVal := strings.TrimSpace(r.FormValue("time"))
	if timeVal == "" {
		timeVal = "01:00"
	}
	if err := config.ValidateScheduleTime(timeVal); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">%s</div>`, html.EscapeString(err.Error()))
		return
	}
	s.cfgMu.Lock()
	s.cfg.Schedule.Time = timeVal
	s.cfg.Schedule.SyncGallery = r.FormValue("sync_gallery") == "on"
	s.cfg.Schedule.RemoveOrphans = r.FormValue("remove_orphans") == "on"
	s.cfg.Schedule.RunAutoTaggers = r.FormValue("run_auto_taggers") == "on"
	s.cfg.Schedule.RecomputeTags = r.FormValue("recompute_tags") == "on"
	s.cfg.Schedule.MergeGeneralTags = r.FormValue("merge_general_tags") == "on"
	s.cfg.Schedule.VacuumDB = r.FormValue("vacuum_db") == "on"
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: schedule updated (time=%s)", timeVal)
	w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
}

func (s *Server) settingsGalleryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.cfgMu.Lock()
	s.cfg.Gallery.WatchEnabled = r.FormValue("watch_enabled") == "on"
	if n, err := strconv.Atoi(r.FormValue("max_file_size_mb")); err == nil {
		s.cfg.Gallery.MaxFileSizeMB = n
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: gallery updated")
	w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
}

func (s *Server) settingsTaggerPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	// Parse per-tagger form fields: tagger_<name>_enabled and tagger_<name>_threshold.
	// The list of known tagger names is sent as hidden "tagger_names" fields so
	// unchecked checkboxes still produce a disabled entry.
	names := r.Form["tagger_names"]

	newUseCUDA := r.FormValue("use_cuda") == "on"
	// Probe CUDA before persisting the enable so the user sees any library/GPU
	// issue immediately instead of waiting for a tagger run to fail. ORT env
	// init is not re-entrant so refuse while a tagger job is holding it.
	if newUseCUDA && !s.cfg.Tagger.UseCUDA {
		if s.jobs.IsRunning() {
			w.Write([]byte(`<div class="flash flash-err">A job is running; try again when it finishes.</div>`))
			return
		}
		if err := tagger.CheckCUDAAvailable(); err != nil {
			fmt.Fprintf(w, `<div class="flash flash-err">Cannot enable GPU: %s</div>`, html.EscapeString(err.Error()))
			return
		}
	}

	s.cfgMu.Lock()

	s.cfg.Tagger.UseCUDA = newUseCUDA
	if n, err := strconv.Atoi(r.FormValue("parallel")); err == nil && n >= 1 {
		s.cfg.Tagger.Parallel = n
	}

	// Rebuild the taggers slice from submitted names so deleted subfolders drop out.
	// Seed from DiscoverTaggers so subfolders present on disk but not yet in TOML
	// keep their discovery defaults (Enabled=true) instead of collapsing to the
	// zero value when the form is saved.
	byName := map[string]config.TaggerInstance{}
	for _, t := range tagger.DiscoverTaggers(s.cfg) {
		byName[t.Name] = t.TaggerInstance
	}
	newList := make([]config.TaggerInstance, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t := byName[name]
		t.Name = name
		// Enabled state is owned by the per-row Enable/Disable endpoints,
		// not the Save form, so preserve whatever the config already holds.
		if f, err := strconv.ParseFloat(r.FormValue("tagger_"+name+"_threshold"), 64); err == nil {
			t.ConfidenceThreshold = f
		}
		newList = append(newList, t)
	}
	s.cfg.Tagger.Taggers = newList
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: tagger updated (%d taggers, use_cuda=%t)", len(newList), s.cfg.Tagger.UseCUDA)
	w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
	s.renderTemplate(w, "partials/tagger_mode_badge.html", map[string]any{
		"UseCUDA": s.cfg.Tagger.UseCUDA,
		"OOB":     true,
	})
}

// settingsTaggerEnablePost flips one tagger's enabled flag to true without
// going through the full tagger form. Mirrors settingsTaggerDisablePost.
func (s *Server) settingsTaggerEnablePost(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	s.cfgMu.Lock()
	found := false
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Name == name {
			s.cfg.Tagger.Taggers[i].Enabled = true
			found = true
			break
		}
	}
	if !found {
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers, config.TaggerInstance{
			Name:                name,
			Enabled:             true,
			ConfidenceThreshold: tagger.DefaultConfidenceThreshold,
		})
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: tagger %q enabled", name)
	w.Header().Set("HX-Refresh", "true")
	w.Write([]byte(`<div class="flash flash-ok">Tagger ` + html.EscapeString(name) + ` enabled.</div>`))
}

// settingsTaggerDisablePost flips one tagger's enabled flag to false without
// going through the full tagger form. An HX-Refresh header re-renders the
// settings page so the row's enabled state and Actions column reflect the
// new state.
func (s *Server) settingsTaggerDisablePost(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	s.cfgMu.Lock()
	found := false
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Name == name {
			s.cfg.Tagger.Taggers[i].Enabled = false
			found = true
			break
		}
	}
	if !found {
		// Tagger existed on disk but had no TOML entry yet - add a disabled
		// one so the preference persists.
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers, config.TaggerInstance{
			Name:                name,
			Enabled:             false,
			ConfidenceThreshold: tagger.DefaultConfidenceThreshold,
		})
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: tagger %q disabled", name)
	w.Header().Set("HX-Refresh", "true")
	w.Write([]byte(`<div class="flash flash-ok">Tagger ` + html.EscapeString(name) + ` disabled.</div>`))
}

func (s *Server) settingsPasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	currentPass := r.FormValue("current_password")
	newPass := r.FormValue("new_password")
	if newPass == "" {
		w.Write([]byte(`<div class="flash flash-err">New password required.</div>`))
		return
	}
	// If a password is already set, require the current one for verification.
	if s.cfg.Auth.EnablePassword && s.cfg.Auth.PasswordHash != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.Auth.PasswordHash), []byte(currentPass)); err != nil {
			w.Write([]byte(`<div class="flash flash-err">Current password is incorrect.</div>`))
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error hashing password.</div>`))
		return
	}
	s.cfgMu.Lock()
	s.cfg.Auth.PasswordHash = string(hash)
	s.cfg.Auth.EnablePassword = true
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: password updated from %s", r.RemoteAddr)
	w.Write([]byte(`<div class="flash flash-ok">Password updated.</div>`))
	s.renderAuthPasswordOOB(w, r)
}

func (s *Server) settingsTokenPost(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		logx.Errorf("generating API token: %v", err)
		w.Write([]byte(`<div class="flash flash-err">Failed to generate token.</div>`))
		return
	}
	token := fmt.Sprintf("%x", buf)
	s.cfgMu.Lock()
	s.cfg.Auth.APIToken = token
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: API token regenerated from %s", r.RemoteAddr)
	w.Header().Set("Cache-Control", "no-store")
	s.renderTemplate(w, "partials/flash_token.html", map[string]any{"Token": token})
}

func (s *Server) settingsUIPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.cfgMu.Lock()
	if n, err := strconv.Atoi(r.FormValue("page_size")); err == nil && n > 0 {
		s.cfg.UI.PageSize = n
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: UI updated")
	w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
}

func (s *Server) pruneMissingImagesPost(w http.ResponseWriter, r *http.Request) {
	// Fetch all missing image IDs first so we can clean up tags and thumbnails.
	rows, err := s.db().Read.Query(`SELECT id FROM images WHERE is_missing = 1`)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(scanErr.Error()) + `</div>`))
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	// Bail before running the deletes when the cursor itself errored
	// part-way through the iteration; otherwise we'd report N removed
	// against a silently-truncated id list.
	if iterErr := rows.Err(); iterErr != nil {
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(iterErr.Error()) + `</div>`))
		return
	}

	// Delete in chunked IN (...) batches so the whole job is a handful of
	// write transactions rather than one per missing row. The schema cascades
	// image_tags / image_paths / sd_metadata / comfyui_metadata on image
	// delete, so a single DELETE FROM images clears the dependent rows.
	const chunkSize = 500
	removed := 0
	affectedTags := map[int64]struct{}{}
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		tx, err := s.db().Write.Begin()
		if err != nil {
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		// Collect the tag IDs so we can scope the post-delete recalculation
		// instead of walking the whole tags table.
		tagRows, err := tx.Query(
			`SELECT DISTINCT tag_id FROM image_tags WHERE image_id IN (`+placeholders+`)`, args...,
		)
		if err != nil {
			tx.Rollback()
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		for tagRows.Next() {
			var tid int64
			if err := tagRows.Scan(&tid); err == nil {
				affectedTags[tid] = struct{}{}
			}
		}
		tagRows.Close()
		res, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			tx.Rollback()
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		if err := tx.Commit(); err != nil {
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
		for _, id := range chunk {
			os.Remove(gallery.ThumbnailPath(s.thumbnailsPath(), id))
			os.Remove(gallery.HoverPath(s.thumbnailsPath(), id))
		}
	}
	// Reconcile usage counts only on the tags we actually touched.
	if len(affectedTags) > 0 {
		tagIDs := make([]int64, 0, len(affectedTags))
		for id := range affectedTags {
			tagIDs = append(tagIDs, id)
		}
		if err := s.tagSvc().RecalcAndPruneIDs(tagIDs); err != nil {
			logx.Warnf("prune-missing recalc IDs: %v", err)
		}
	}
	if removed > 0 {
		s.Active().InvalidateCaches()
	}
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Removed %d missing image(s).</div>`, removed)))
}

func (s *Server) pruneOrphanedThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	thumbDir := s.thumbnailsPath()
	entries, err := os.ReadDir(thumbDir)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error reading thumbnails directory: ` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	// Load the full set of image ids once, then diff against on-disk files
	// instead of issuing one SELECT per thumbnail.
	known := map[int64]struct{}{}
	idRows, err := s.db().Read.Query(`SELECT id FROM images`)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err == nil {
			known[id] = struct{}{}
		}
	}
	idRows.Close()

	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Parse image ID from "123.jpg" or "123_hover.webp"
		var idStr string
		if strings.HasSuffix(name, "_hover.webp") {
			idStr = strings.TrimSuffix(name, "_hover.webp")
		} else if strings.HasSuffix(name, ".jpg") {
			idStr = strings.TrimSuffix(name, ".jpg")
		} else {
			continue
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if removeErr := os.Remove(filepath.Join(thumbDir, name)); removeErr == nil {
			removed++
		}
	}
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Removed %d orphaned thumbnail(s).</div>`, removed)))
}

func (s *Server) recalcTagsPost(w http.ResponseWriter, r *http.Request) {
	updated, pruned := s.tagSvc().RecalcAndPruneCount()
	w.Write([]byte(fmt.Sprintf(
		`<div class="flash flash-ok">Recalculated %d tag count(s); pruned %d unused tag(s).</div>`,
		updated, pruned,
	)))
}

func (s *Server) mergeGeneralTagsPost(w http.ResponseWriter, r *http.Request) {
	merged, err := s.tagSvc().MergeGeneralIntoCategorized()
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	w.Write([]byte(fmt.Sprintf(
		`<div class="flash flash-ok">Merged %d general tag(s) into categorized counterparts.</div>`,
		merged,
	)))
}

func (s *Server) settingsRemovePasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	// Require current password to disable authentication.
	currentPass := r.FormValue("current_password")
	if s.cfg.Auth.EnablePassword && s.cfg.Auth.PasswordHash != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.Auth.PasswordHash), []byte(currentPass)); err != nil {
			w.Write([]byte(`<div class="flash flash-err">Current password is incorrect.</div>`))
			return
		}
	}
	s.cfgMu.Lock()
	s.cfg.Auth.EnablePassword = false
	s.cfg.Auth.PasswordHash = ""
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: password removed from %s", r.RemoteAddr)
	// Invalidate all sessions so nobody is locked out of the now-open instance
	s.sessions.Clear()
	w.Write([]byte(`<div class="flash flash-ok">Password removed. Authentication is now disabled.</div>`))
	s.renderAuthPasswordOOB(w, r)
}

// renderAuthPasswordOOB writes an out-of-band swap for the password subsection
// so the "currently enabled/disabled" text and form fields reflect the latest
// auth state without requiring a page reload.
func (s *Server) renderAuthPasswordOOB(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/auth_password_section.html", map[string]any{
		"AuthEnabled": s.cfg.Auth.EnablePassword,
		"CSRFToken":   s.csrfToken(sessionFromContext(r.Context())),
		"OOB":         true,
	})
}

func (s *Server) categoryCountHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	count, err := s.tagSvc().GetCategoryTagCount(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"count":%d}`, count)
}

func (s *Server) duplicatesListHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db().Read.Query(`
		SELECT i.id, i.canonical_path, ip.id as path_id, ip.path
		FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0
		ORDER BY i.id, ip.id
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type aliasRow struct {
		ImageID       int64
		CanonicalPath string
		PathID        int64
		AliasPath     string
	}
	var aliases []aliasRow
	for rows.Next() {
		var a aliasRow
		if err := rows.Scan(&a.ImageID, &a.CanonicalPath, &a.PathID, &a.AliasPath); err != nil {
			logx.Warnf("duplicates list scan: %v", err)
			continue
		}
		aliases = append(aliases, a)
	}

	s.renderTemplate(w, "partials/duplicates_list.html", map[string]any{
		"Aliases": aliases,
	})
}

func (s *Server) removeDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	// Remove a specific subset when the form carries path_id values (one per
	// listed row), or every non-canonical row when the form carries
	// `all=true`. Refusing to fall through unless one of the two is set
	// keeps a stray POST with just a CSRF token from wiping the whole
	// library of alias files at once.
	selected := r.Form["path_id"]
	allFlag := r.FormValue("all") == "true"
	if len(selected) == 0 && !allFlag {
		w.Write([]byte(`<div class="flash flash-err">No duplicate paths selected.</div>`))
		return
	}

	var (
		rows *sql.Rows
		err  error
	)
	if allFlag {
		rows, err = s.db().Read.Query(`
			SELECT ip.id, ip.path
			FROM image_paths ip
			WHERE ip.is_canonical = 0
		`)
	} else {
		// Build an IN (?,?,...) query restricted to the supplied path_ids
		// that still aren't canonical - callers can't use this endpoint to
		// remove the canonical path for an image.
		placeholders := strings.Repeat("?,", len(selected))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(selected))
		for _, s := range selected {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				args = append(args, id)
			}
		}
		if len(args) == 0 {
			w.Write([]byte(`<div class="flash flash-err">No valid path_ids in request.</div>`))
			return
		}
		rows, err = s.db().Read.Query(
			`SELECT ip.id, ip.path FROM image_paths ip
			 WHERE ip.is_canonical = 0 AND ip.id IN (`+placeholders+`)`,
			args...,
		)
	}
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	defer rows.Close()

	type pathRow struct {
		ID   int64
		Path string
	}
	var paths []pathRow
	for rows.Next() {
		var p pathRow
		if err := rows.Scan(&p.ID, &p.Path); err != nil {
			logx.Warnf("remove duplicates scan: %v", err)
			continue
		}
		paths = append(paths, p)
	}
	rows.Close()

	removed := 0
	for _, p := range paths {
		if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, p.ID); err != nil {
			logx.Warnf("remove duplicate %d: %v", p.ID, err)
			continue
		}
		if p.Path != "" {
			if err := os.Remove(p.Path); err != nil && !os.IsNotExist(err) {
				logx.Warnf("remove duplicate %q: %v", p.Path, err)
			}
		}
		removed++
	}
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Removed %d duplicate path(s).</div>`, removed)))
}

func (s *Server) rebuildThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	if err := s.startRebuildThumbsJob(s.Active()); err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">Thumbnail rebuild started.</div>`))
}

// startRebuildThumbsJob queues a rebuild-thumbs job against the supplied
// gallery context, reading images and writing thumbnails from that gallery's
// own DB + thumbnails dir. Reused by the manual handler (active gallery) and
// the post-import hook (imported non-active gallery).
func (s *Server) startRebuildThumbsJob(cx *galleryCtx) error {
	if cx == nil || cx.DB == nil {
		return fmt.Errorf("no gallery context")
	}
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
	}
	rows, err := cx.DB.Read.Query(
		`SELECT id, canonical_path, file_type FROM images WHERE is_missing = 0`)
	if err != nil {
		return err
	}
	var imgs []imgRow
	for rows.Next() {
		var img imgRow
		if err := rows.Scan(&img.ID, &img.Path, &img.FileType); err != nil {
			rows.Close()
			return err
		}
		imgs = append(imgs, img)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := s.jobs.Start("rebuild-thumbs"); err != nil {
		return fmt.Errorf("a job is already running")
	}
	thumbnailsPath := cx.ThumbnailsPath
	galleryName := cx.Name
	go func() {
		ctx := s.jobs.Context()
		processed := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("[%s] rebuild cancelled (%d/%d rebuilt)", galleryName, processed, total))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("[%s] rebuilding %d/%d", galleryName, processed, total))
			if err := gallery.Generate(img.Path, thumbnailsPath, img.ID, img.FileType); err != nil {
				logx.Warnf("rebuild thumbnail for %d: %v", img.ID, err)
			}
			processed++
		}
		s.jobs.Complete(fmt.Sprintf("[%s] rebuilt %d thumbnail(s).", galleryName, processed))
	}()
	return nil
}

func (s *Server) vacuumDBPost(w http.ResponseWriter, r *http.Request) {
	beforeSize := dbFileSize(s.dbPath())
	if _, err := s.db().Write.Exec(`VACUUM`); err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	// VACUUM in WAL mode writes the rebuilt pages into the -wal file, so the
	// user sees no drop in on-disk footprint until the WAL is consolidated.
	// Truncate the WAL explicitly so the reclaimed space is actually released.
	if _, err := s.db().Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		logx.Warnf("vacuum wal_checkpoint: %v", err)
	}
	afterSize := dbFileSize(s.dbPath())
	freed := beforeSize - afterSize
	if freed < 0 {
		freed = 0
	}
	w.Write([]byte(fmt.Sprintf(
		`<div class="flash flash-ok">Database vacuumed. Reclaimed %s.</div>`, humanBytesFmt(freed),
	)))
}

// dbFileSize returns the total on-disk footprint of the SQLite database -
// the main file plus the WAL and shared-memory sidecars. A post-VACUUM
// "reclaimed N" figure that only counts the main file misleads the user
// whenever the WAL holds the bulk of the pages (common after mass deletes).
func dbFileSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// humanBytesFmt mirrors the humanBytes template helper from the router for
// use in handler responses. Kept tiny so the two formatters stay trivially
// consistent.
func humanBytesFmt(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (s *Server) reExtractMetadataPost(w http.ResponseWriter, r *http.Request) {
	// Stream rows into a slice of lightweight structs so the DB cursor is closed
	// before the long-running goroutine starts. This avoids holding a read
	// connection open for the entire re-extraction job while keeping memory
	// usage proportional to the number of images (IDs + short paths only).
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
		// Current persisted hashes; we use them to skip the rewrite when the
		// new extraction would produce the same generation_hash - most runs
		// on an unchanged library now turn into pure reads.
		sdHash    string
		comfyHash string
		source    string
	}

	rows, err := s.db().Read.Query(`
		SELECT i.id, i.canonical_path, i.file_type, i.source_type,
		       COALESCE(sm.generation_hash, ''),
		       COALESCE(cm.generation_hash, '')
		FROM images i
		LEFT JOIN sd_metadata sm ON sm.image_id = i.id
		LEFT JOIN comfyui_metadata cm ON cm.image_id = i.id
		WHERE i.is_missing = 0
	`)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	var imgs []imgRow
	for rows.Next() {
		var img imgRow
		if err := rows.Scan(&img.ID, &img.Path, &img.FileType, &img.source, &img.sdHash, &img.comfyHash); err != nil {
			logx.Warnf("re-extract scan: %v", err)
			continue
		}
		imgs = append(imgs, img)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	if err := s.jobs.Start("re-extract"); err != nil {
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}

	database := s.db()
	go func() {
		ctx := s.jobs.Context()
		processed := 0
		updated := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("re-extraction cancelled (%d/%d processed, %d updated)", processed, total, updated))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("Processing %d/%d…", processed, total))
			sdMeta, comfyMeta, _ := meta.Extract(img.Path, img.FileType)

			sourceType := models.SourceTypeNone
			if sdMeta != nil && comfyMeta != nil {
				sourceType = models.SourceTypeBoth
			} else if sdMeta != nil {
				sourceType = models.SourceTypeA1111
			} else if comfyMeta != nil {
				sourceType = models.SourceTypeComfyUI
			}

			newSDHash := ""
			if sdMeta != nil {
				newSDHash = sdMeta.GenerationHash
			}
			newComfyHash := ""
			if comfyMeta != nil {
				newComfyHash = comfyMeta.GenerationHash
			}
			// Skip the delete+insert churn when the new extraction lines up
			// with what the DB already holds. Any pipeline change that adds
			// or drops fields changes the generation hash, so this stays
			// responsive to real metadata schema updates.
			if newSDHash == img.sdHash && newComfyHash == img.comfyHash && sourceType == img.source {
				processed++
				continue
			}

			// Single transaction per image so a mid-flight failure can't leave
			// images.source_type updated against a half-deleted metadata table
			// or a deleted-but-not-reinserted row.
			if err := reExtractApply(ctx, database, img.ID, sourceType, sdMeta, comfyMeta); err != nil {
				logx.Warnf("re-extract image %d: %v", img.ID, err)
				processed++
				continue
			}
			processed++
			updated++
		}
		s.jobs.Complete(fmt.Sprintf("Re-extracted metadata for %d image(s) (%d updated).", processed, updated))
	}()

	w.Write([]byte(`<div class="flash flash-ok">Re-extraction started.</div>`))
}

// reExtractApply commits a re-extracted image's source_type, deletes the
// previous SD/ComfyUI rows, and reinserts whichever the parser produced.
// All four steps run in one transaction so a partial failure (writer
// contention, ctx cancellation mid-statement) never leaves the row with
// updated source_type but missing metadata.
func reExtractApply(ctx context.Context, database *db.DB, imageID int64, sourceType string, sdMeta *models.SDMetadata, comfyMeta *models.ComfyUIMetadata) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE images SET source_type = ? WHERE id = ?`, sourceType, imageID); err != nil {
		return fmt.Errorf("update source_type: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sd_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("delete sd_metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM comfyui_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("delete comfyui_metadata: %w", err)
	}
	if sdMeta != nil {
		sdMeta.ImageID = imageID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sdMeta.ImageID, sdMeta.Prompt, sdMeta.NegativePrompt, sdMeta.Model,
			sdMeta.Seed, sdMeta.Sampler, sdMeta.Steps, sdMeta.CFGScale, sdMeta.RawParams, sdMeta.GenerationHash,
		); err != nil {
			return fmt.Errorf("insert sd_metadata: %w", err)
		}
	}
	if comfyMeta != nil {
		comfyMeta.ImageID = imageID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			comfyMeta.ImageID, comfyMeta.Prompt, comfyMeta.ModelCheckpoint,
			comfyMeta.Seed, comfyMeta.Sampler, comfyMeta.Steps, comfyMeta.CFGScale, comfyMeta.RawWorkflow, comfyMeta.GenerationHash,
		); err != nil {
			return fmt.Errorf("insert comfyui_metadata: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Server) jobDismissPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Dismiss()
	w.WriteHeader(http.StatusNoContent)
}

// jobCancelPost aborts the running job by cancelling its context. Workers
// observing ctx.Done() wrap up and call Complete/Fail themselves.
func (s *Server) jobCancelPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createCategoryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.FormValue("name")
	color := r.FormValue("color")
	if color == "" {
		color = "#888888"
	}
	if _, err := s.tagSvc().CreateCategory(name, color); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/categories")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}

func (s *Server) updateCategoryPatch(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	color := r.FormValue("color")
	if err := s.tagSvc().UpdateCategoryColor(id, color); err != nil {
		logx.Warnf("update category %d color: %v", id, err)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">Updated.</div>`))
}

func (s *Server) deleteCategoryDelete(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	action := r.FormValue("action") // "move" | "delete_all"
	if action == "" {
		action = "move"
	}
	var targetID int64
	if ts := r.FormValue("target_id"); ts != "" {
		targetID, _ = strconv.ParseInt(ts, 10, 64)
	}
	if err := s.tagSvc().DeleteCategoryMoveOrDelete(id, action, targetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/tags")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) deleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := s.db().Write.Exec(`DELETE FROM saved_searches WHERE id = ?`, id); err != nil {
		logx.Warnf("delete saved search %d: %v", id, err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	// 200 + empty body - HTMX outerHTML swap removes the element.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) createSavedSearch(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	query := strings.TrimSpace(r.FormValue("query"))
	if name == "" || query == "" {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">Name and query required.</div>`))
			return
		}
		http.Error(w, "name and query required", http.StatusBadRequest)
		return
	}
	if _, err := s.db().Write.Exec(
		`INSERT OR REPLACE INTO saved_searches (name, query) VALUES (?, ?)`, name, query,
	); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if isHTMXRequest(r) {
		w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) deleteTagHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().DeleteTag(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) renameTagPost(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">Name required.</div>`))
			return
		}
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RenameTag(id, newName); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) renameCategoryPost(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">Name required.</div>`))
			return
		}
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RenameCategory(id, newName); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/categories")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}


func (s *Server) helpHandler(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "help.html", s.base(r, "help", "Help - Monbooru"))
}

func (s *Server) jobStatusHandler(w http.ResponseWriter, r *http.Request) {
	// Mark before Get so the first render of a completed state starts the
	// short post-view dismiss timer. Subsequent views don't re-arm it.
	s.jobs.MarkViewed()
	state := s.jobs.Get()
	s.renderTemplate(w, "partials/job_status.html", state)
}

func (s *Server) syncTrigger(w http.ResponseWriter, r *http.Request) {
	if cx := s.Active(); cx == nil || cx.Degraded {
		http.Error(w, "sync unavailable: gallery path is unreadable", http.StatusServiceUnavailable)
		return
	}
	if err := s.jobs.Start("sync"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	// Snapshot the active gallery's state under the request's RLock so the
	// background goroutine is not racing a subsequent swap. The IsRunning
	// guard in SwitchGallery refuses swaps while the sync runs, so these
	// handles stay valid for the job's lifetime.
	cx := s.Active()
	database := s.db()
	galleryPath := s.galleryPath()
	thumbnailsPath := s.thumbnailsPath()
	maxFileSizeMB := s.cfg.Gallery.MaxFileSizeMB
	go func() {
		ctx := s.jobs.Context()
		result, err := gallery.Sync(ctx, database, galleryPath, thumbnailsPath, maxFileSizeMB, s.jobs.Update)
		cx.InvalidateCaches()
		if ctx.Err() != nil {
			s.jobs.Complete("sync cancelled")
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("%d added, %d missing, %d moved",
			result.Added, result.Removed, result.Moved))
	}()

	redirectTo := r.Referer()
	if redirectTo == "" {
		redirectTo = "/"
	}
	if isHTMXRequest(r) {
		// Signal the client to reload the gallery when the job finishes.
		w.Header().Set("HX-Trigger", "syncStarted")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}


func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	idStrs := r.Form["ids"]

	var targets []search.DeleteTarget
	for _, idStr := range idStrs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		t := search.DeleteTarget{ID: id}
		var isMissing int
		if err := s.db().Read.QueryRow(
			`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
		).Scan(&t.CanonicalPath, &t.FolderPath, &isMissing); err != nil {
			continue
		}
		t.IsMissing = isMissing == 1
		targets = append(targets, t)
	}

	s.startBulkDelete(w, targets)
}

func (s *Server) deleteSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	queryStr := r.FormValue("q")

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		logx.Warnf("delete-search parse: %v", parseErr)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
			html.EscapeString(parseErr.Error()) + `</div>`))
		return
	}

	// Stream the matching targets off the cursor so very large result sets
	// don't allocate a second intermediate copy on top of whatever the
	// bulk-delete worker holds.
	var targets []search.DeleteTarget
	err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
		targets = append(targets, t)
		return nil
	})
	if err != nil {
		logx.Errorf("delete-search: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
		return
	}

	s.startBulkDelete(w, targets)
}

// startBulkDelete kicks off a background delete job for the given targets and
// writes the response. The job reports progress via jobs.Manager; the client
// sees the running state in the top-right status bar.
func (s *Server) startBulkDelete(w http.ResponseWriter, targets []search.DeleteTarget) {
	if len(targets) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start("delete"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBulkDelete(targets)
	w.WriteHeader(http.StatusAccepted)
}

// runBulkDelete processes targets in chunks with one transaction per chunk.
// The images schema cascades image_tags / image_paths / sd_metadata /
// comfyui_metadata on image delete, so a single DELETE FROM images clears the
// dependent rows. Tag usage counts are reconciled at the end by a targeted
// recalc scoped to the tag IDs actually touched by the cascade (collected
// from image_tags before the DELETE), avoiding a full-table RecalcAndPrune
// that would walk every tag in the library.
func (s *Server) runBulkDelete(targets []search.DeleteTarget) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(targets)
	processed := 0
	folders := map[string]struct{}{}
	affectedTags := map[int64]struct{}{}
	cancelled := false

	s.jobs.Update(0, total, fmt.Sprintf("deleting 0/%d…", total))

	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunk := targets[start:end]

		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, t := range chunk {
			args[i] = t.ID
		}

		tx, err := s.db().Write.Begin()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		rows, err := tx.Query(`SELECT DISTINCT tag_id FROM image_tags WHERE image_id IN (`+placeholders+`)`, args...)
		if err != nil {
			tx.Rollback()
			s.jobs.Fail(err.Error())
			return
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				tx.Rollback()
				s.jobs.Fail(err.Error())
				return
			}
			affectedTags[id] = struct{}{}
		}
		rows.Close()
		if _, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...); err != nil {
			tx.Rollback()
			s.jobs.Fail(err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			s.jobs.Fail(err.Error())
			return
		}

		for _, t := range chunk {
			os.Remove(gallery.ThumbnailPath(s.thumbnailsPath(), t.ID))
			os.Remove(gallery.HoverPath(s.thumbnailsPath(), t.ID))
			if !t.IsMissing && t.CanonicalPath != "" {
				if err := os.Remove(t.CanonicalPath); err != nil && !os.IsNotExist(err) {
					logx.Warnf("bulk delete file %q: %v", t.CanonicalPath, err)
				}
			}
			if !t.IsMissing && t.FolderPath != "" {
				folders[t.FolderPath] = struct{}{}
			}
		}

		processed = end
		s.jobs.Update(processed, total, fmt.Sprintf("deleting %d/%d…", processed, total))
	}

	if len(affectedTags) > 0 {
		s.jobs.Update(processed, total, "reconciling tag counts…")
		ids := make([]int64, 0, len(affectedTags))
		for id := range affectedTags {
			ids = append(ids, id)
		}
		if err := s.tagSvc().RecalcAndPruneIDs(ids); err != nil {
			logx.Warnf("bulk delete recalc IDs: %v", err)
		}
	}

	for fp := range folders {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), fp)
	}

	if processed > 0 {
		s.Active().InvalidateCaches()
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("delete cancelled (%d/%d deleted)", processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("Deleted %d image(s).", processed))
}

// batchMove kicks off a background `move` job that relocates the selected
// image IDs into the requested folder. Collisions on filename auto-suffix via
// UniqueDestPath. The watcher suppresses its events while this job runs so
// the Rename pairs don't flap the images as missing in transit.
func (s *Server) batchMove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	idStrs := r.Form["ids"]
	targetFolder := strings.TrimSpace(r.FormValue("folder"))

	// Validate the folder once up-front so the user sees the error inline
	// rather than as a per-image log entry once the job starts.
	if _, err := gallery.ResolveSubdir(s.galleryPath(), targetFolder); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	var ids []int64
	for _, idStr := range idStrs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if err := s.jobs.Start("move"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchMove(ids, targetFolder)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchMove processes move targets one image at a time. Each MoveImage has
// its own small write txn + Rename; per-image failures are logged and counted
// but don't stop the run so a single unreadable file can't strand the rest.
// Empty source folders are cleaned up at the end, matching single-image move.
func (s *Server) runBatchMove(ids []int64, targetFolder string) {
	ctx := s.jobs.Context()
	total := len(ids)
	moved, failed := 0, 0
	cancelled := false
	oldFolders := map[string]struct{}{}

	s.jobs.Update(0, total, fmt.Sprintf("moving 0/%d…", total))

	for i, id := range ids {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		res, err := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder)
		if err != nil {
			logx.Warnf("batch move %d: %v", id, err)
			failed++
			continue
		}
		if res.OldFolderPath != res.NewFolderPath && res.OldFolderPath != "" {
			oldFolders[res.OldFolderPath] = struct{}{}
		}
		moved++
		if (i+1)%25 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, fmt.Sprintf("moving %d/%d…", i+1, total))
		}
	}

	for fp := range oldFolders {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), fp)
	}

	if moved > 0 {
		s.Active().InvalidateCaches()
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("move cancelled (%d/%d moved)", moved, total))
		return
	}
	summary := fmt.Sprintf("Moved %d image(s).", moved)
	if failed > 0 {
		summary = fmt.Sprintf("Moved %d image(s), %d failed.", moved, failed)
	}
	s.jobs.Complete(summary)
}

// moveImage relocates the one image at {id} into the requested folder. A `move`
// job is used even for single-image moves to reuse the watcher suppression
// pattern from batch moves; the job is brief and auto-dismisses like any other.
func (s *Server) moveImage(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	targetFolder := strings.TrimSpace(r.FormValue("folder"))

	if err := s.jobs.Start("move"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}

	res, moveErr := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder)
	if moveErr != nil {
		s.jobs.Fail(moveErr.Error())
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(moveErr.Error()) + `</div>`))
		return
	}
	if res.OldFolderPath != res.NewFolderPath && res.OldFolderPath != "" {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), res.OldFolderPath)
	}
	s.Active().InvalidateCaches()
	s.jobs.Complete("Moved image.")

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/images/"+idStr)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

// foldersSuggest returns up to 10 existing folder paths whose name or leading
// segments match the typed prefix. Drives the autocomplete dropdown on the
// move dialogs. Root (” folder_path) is excluded from suggestions because it
// maps to an empty input anyway.
func (s *Server) foldersSuggest(w http.ResponseWriter, r *http.Request) {
	prefix := strings.TrimSpace(r.URL.Query().Get("prefix"))
	rows, err := s.db().Read.Query(
		`SELECT DISTINCT folder_path FROM images
		 WHERE is_missing = 0 AND folder_path != '' AND folder_path LIKE ?
		 ORDER BY folder_path LIMIT 10`,
		prefix+"%",
	)
	if err != nil {
		logx.Warnf("folders suggest: %v", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer rows.Close()
	var folders []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			continue
		}
		folders = append(folders, fp)
	}
	if len(folders) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderTemplate(w, "partials/folder_suggest.html", map[string]any{
		"Folders": folders,
	})
}

func (s *Server) deleteFolderPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	folderPath := r.FormValue("folder")

	if folderPath == "" {
		http.Error(w, "invalid folder path", http.StatusBadRequest)
		return
	}

	// Share the gallery-root validator that also powers the upload path so
	// `foo..bar` (legal filename) is no longer rejected by a substring
	// check, while a sibling directory that shares the gallery prefix
	// (`/data/gallery_backup`) is still refused via filepath.Rel.
	absPath, err := gallery.ResolveSubdir(s.galleryPath(), folderPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.Remove(absPath); err != nil {
		// Treat "already gone" as success so a stale UI can re-issue the
		// delete without an error toast. ENOTEMPTY (raised by Linux when
		// the directory still has children) maps to the same 409 the UI
		// already surfaces. Anything else is a real failure - permission
		// denied, busy, etc. - and must not silently masquerade as a
		// successful redirect.
		switch {
		case os.IsNotExist(err):
			// nothing to do - fall through to the success redirect
		case errors.Is(err, syscall.ENOTEMPTY):
			http.Error(w, "directory not empty", http.StatusConflict)
			return
		default:
			logx.Warnf("delete folder %q: %v", absPath, err)
			http.Error(w, "could not delete folder: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) tagSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Accept the input's value however it arrives: q=, tag=, or canonical_id=
	prefix := q.Get("q")
	if prefix == "" {
		prefix = q.Get("tag")
	}
	if prefix == "" {
		prefix = q.Get("canonical_id")
	}

	// If the prefix contains "category:name" and the prefix is a real
	// category, filter by category. Otherwise suggest literal tags whose
	// full name matches the raw input (so tags like "nier:automata" still
	// surface while the user types).
	var catName, tagPrefix string
	if idx := strings.Index(prefix, ":"); idx > 0 && s.categoryExists(prefix[:idx]) {
		catName = prefix[:idx]
		tagPrefix = prefix[idx+1:]
	} else {
		tagPrefix = prefix
	}

	var suggestions []models.Tag
	if catName != "" {
		suggestions, _ = s.tagSvc().SuggestTagsInCategory(tagPrefix, catName, 10)
	} else {
		suggestions, _ = s.tagSvc().SuggestTags(tagPrefix, 10)
	}

	// Attribute each suggestion with its category so selecting a non-general
	// tag adds it in the right category on submit.
	if catName != "" {
		for i := range suggestions {
			suggestions[i].Name = catName + ":" + suggestions[i].Name
		}
	} else {
		for i := range suggestions {
			if suggestions[i].CategoryName != "" && suggestions[i].CategoryName != "general" {
				suggestions[i].Name = suggestions[i].CategoryName + ":" + suggestions[i].Name
			}
		}
	}

	s.renderTemplate(w, "partials/tag_suggest.html", map[string]any{
		"Suggestions": suggestions,
	})
}

func (s *Server) searchSuggest(w http.ResponseWriter, r *http.Request) {
	// Pin the swap target server-side. When an auto-refresh fires concurrently
	// with the debounced input request, htmx has been observed to resolve the
	// input's hx-target to the form-inherited #gallery-grid, which lands the
	// dropdown inside the grid with no way to dismiss it. HX-Retarget forces
	// the response back onto #search-suggest regardless of what the client
	// computed at request time.
	w.Header().Set("HX-Retarget", "#search-suggest")
	w.Header().Set("HX-Reswap", "innerHTML")

	input := r.URL.Query().Get("q")
	// Split the input: everything except the last word is the "context"
	// that must also match, and the last word is the prefix being typed.
	// The last word has its leading "-" stripped so the suggestion list works
	// while the user is still typing the negated tag.
	words := strings.Fields(input)
	prefix := ""
	var catFilter string // category name if user typed "catname:prefix"
	var contextTokens []string
	if len(words) > 0 {
		last := words[len(words)-1]
		contextTokens = words[:len(words)-1]
		last = strings.TrimPrefix(last, "-")
		if colonIdx := strings.IndexByte(last, ':'); colonIdx >= 0 {
			key := strings.ToLower(last[:colonIdx])
			val := last[colonIdx+1:]
			// For value-only filter keywords, don't suggest tags. The set
			// is shared with alreadyTypedTags via search.IsFilterKeyword
			// so the two helpers stay in lockstep.
			if search.IsFilterKeyword(key) {
				// no tag suggestions
			} else {
				// Category-qualified only when the prefix actually names
				// a category; otherwise suggest literal tags that match
				// the whole "key:val" string (e.g. "nier:aut…" →
				// "nier:automata").
				if colonIdx > 0 && s.categoryExists(key) {
					catFilter = key
					prefix = val
				} else {
					prefix = last
				}
			}
		} else {
			prefix = last
		}
	}
	if prefix == "" && catFilter == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Parse the preceding tokens as a query. Empty context → expr is nil and
	// the combination filter degrades to a plain global-usage suggestion.
	contextExpr, _ := search.Parse(strings.Join(contextTokens, " "))

	suggestions, _ := search.SuggestTagsWithFilter(s.db(), contextExpr, prefix, catFilter, 10)

	// Prefix non-general tags (or category-qualified searches) so clicking a
	// suggestion appends the correct token to the search bar.
	for i := range suggestions {
		if catFilter != "" {
			suggestions[i].Name = catFilter + ":" + suggestions[i].Name
		} else if suggestions[i].CategoryName != "" && suggestions[i].CategoryName != "general" {
			suggestions[i].Name = suggestions[i].CategoryName + ":" + suggestions[i].Name
		}
	}

	// Drop suggestions whose formatted name is already present in the search
	// bar - otherwise typing a partial tag that overlaps an existing one would
	// re-suggest what the user already picked.
	if alreadyTyped := alreadyTypedTags(contextTokens); len(alreadyTyped) > 0 {
		out := suggestions[:0]
		for _, sug := range suggestions {
			if _, ok := alreadyTyped[sug.Name]; ok {
				continue
			}
			out = append(out, sug)
		}
		suggestions = out
	}

	s.renderTemplate(w, "partials/search_suggest.html", map[string]any{
		"Suggestions": suggestions,
	})
}

// alreadyTypedTags normalizes the preceding search-bar tokens into the same
// shape as formatted suggestion names (plain "tag" or "category:tag") so the
// suggest filter can drop tags the user has already committed. Filter keywords
// (fav:true, folder:..., etc.) are skipped because they aren't tag names and
// would never match a suggestion anyway.
func alreadyTypedTags(contextTokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(contextTokens))
	for _, tok := range contextTokens {
		t := strings.TrimPrefix(tok, "-")
		if t == "" {
			continue
		}
		// Skip filter keywords; only tag tokens belong in the de-dup set.
		// Shares search.IsFilterKeyword with searchSuggest's value-only check.
		if colonIdx := strings.IndexByte(t, ':'); colonIdx > 0 {
			if search.IsFilterKeyword(strings.ToLower(t[:colonIdx])) {
				continue
			}
		}
		set[t] = struct{}{}
	}
	return set
}

func (s *Server) changeTagCategory(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	catIDStr := r.FormValue("category_id")
	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad category_id", http.StatusBadRequest)
		return
	}
	// Route through the tag service for validation and consistency.
	if err := s.tagSvc().ChangeTagCategory(id, catID); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if isHTMXRequest(r) {
		w.Write([]byte(`<div class="flash flash-ok">Category updated.</div>`))
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}


func (s *Server) getImageTagsHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", false)
}

// findAdjacentImages finds the prev/next image IDs in the given search context
// via cursor-style LIMIT 1 queries - O(log n) per side instead of loading the
// full matching ID list. seedStr carries the random-sort seed forward from the
// referring gallery so the same shuffle resolves to the same neighbours.
func (s *Server) findAdjacentImages(currentID int64, queryStr, sortStr, orderStr, seedStr string) (prevID, nextID *int64) {
	sq := adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr)
	prevID, nextID, _ = search.ExecuteAdjacent(s.db(), sq, currentID)
	return
}

// findImagePosition returns the 1-based rank of currentID in the referring
// search and the total matching rows. Used by the detail page's X/Y counter.
// Shares the same query-building path as findAdjacentImages so both numbers
// agree with the prev/next arrows they sit next to.
func (s *Server) findImagePosition(currentID int64, queryStr, sortStr, orderStr, seedStr string) (pos, total int) {
	sq := adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr)
	pos, total, _ = search.ExecutePosition(s.db(), sq, currentID)
	return
}

func adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr string) search.Query {
	expr, _ := search.Parse(queryStr)
	sq := search.Query{
		Expr:  expr,
		Sort:  sortStr,
		Order: orderStr,
	}
	if sortStr == "random" && seedStr != "" {
		if seed, err := strconv.ParseInt(seedStr, 10, 64); err == nil {
			sq.RandomSeed = seed
		}
	}
	return sq
}

// buildGalleryURL constructs a properly URL-encoded gallery redirect URL.
func buildGalleryURL(q, sort, order, page, seed string) string {
	if q == "" && sort == "" && order == "" && page == "" && seed == "" {
		return "/"
	}
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if sort != "" {
		v.Set("sort", sort)
	}
	if order != "" {
		v.Set("order", order)
	}
	if page != "" {
		v.Set("page", page)
	}
	if seed != "" {
		v.Set("seed", seed)
	}
	return "/?" + v.Encode()
}

func loadImage(ctx context.Context, database *db.DB, id int64) (*models.Image, error) {
	var img models.Image
	var isMissing, isFav int
	var width, height *int
	var autoTaggedAt *string
	var ingestedAt string

	err := database.Read.QueryRowContext(ctx,
		`SELECT id, sha256, canonical_path, folder_path, file_type,
		        width, height, file_size, is_missing, is_favorited,
		        auto_tagged_at, source_type, origin, ingested_at
		 FROM images WHERE id = ?`, id,
	).Scan(
		&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType,
		&width, &height, &img.FileSize, &isMissing, &isFav,
		&autoTaggedAt, &img.SourceType, &img.Origin, &ingestedAt,
	)
	if err != nil {
		return nil, err
	}
	img.IsMissing = isMissing == 1
	img.IsFavorited = isFav == 1
	img.Width = width
	img.Height = height
	if autoTaggedAt != nil {
		t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
		img.AutoTaggedAt = &t
	}
	img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	return &img, nil
}

func loadSDMeta(ctx context.Context, database *db.DB, id int64) *models.SDMetadata {
	var m models.SDMetadata
	var rawParams, genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash
		 FROM sd_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.NegativePrompt, &m.Model, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &rawParams, &genHash)
	if err != nil {
		return nil
	}
	if rawParams != nil {
		m.RawParams = *rawParams
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	if m.RawParams != "" {
		m.ParsedParams = meta.ParseAllSDParams(m.RawParams)
	}
	return &m
}

func loadComfyMeta(ctx context.Context, database *db.DB, id int64) *models.ComfyUIMetadata {
	var m models.ComfyUIMetadata
	var genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash
		 FROM comfyui_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.ModelCheckpoint, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &m.RawWorkflow, &genHash)
	if err != nil {
		return nil
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	return &m
}

func loadImagePaths(ctx context.Context, database *db.DB, id int64) []models.ImagePath {
	rows, err := database.Read.QueryContext(ctx,
		`SELECT id, image_id, path, is_canonical FROM image_paths WHERE image_id = ? ORDER BY is_canonical DESC, id`,
		id,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var paths []models.ImagePath
	for rows.Next() {
		var p models.ImagePath
		var isCanon int
		if err := rows.Scan(&p.ID, &p.ImageID, &p.Path, &isCanon); err != nil {
			logx.Warnf("load image paths scan: %v", err)
			continue
		}
		p.IsCanonical = isCanon == 1
		paths = append(paths, p)
	}
	return paths
}
