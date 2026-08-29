package web

import (
	"cmp"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// galleryHiddenIndicatorBudget caps the time the first gallery
// search.Execute may spend before the handler skips the second
// no-ceiling COUNT that drives the "N hidden" indicator. The render
// degrades to the existing matches-only line instead of paying a
// second slow pass when the first already ate the search budget.
const galleryHiddenIndicatorBudget = 300 * time.Millisecond

// batchGapMinutes is the inter-file inactivity threshold that closes
// one inbox cluster and opens the next. Hardcoded for v1 - retune by
// recompile if 15 minutes turns out wrong in practice.
const batchGapMinutes = 15

type galleryData struct {
	baseData
	Query string
	// SearchWarning flags a closed-vocabulary filter value that matched
	// nothing because the value itself is unrecognised (type:video), so the
	// empty result doesn't read as "no images" when it means "bad value".
	SearchWarning     string
	Sort              string
	Order             string
	ThumbnailFit      string // "square" | "natural"; drives the grid's thumb-fit CSS class
	RandomSeed        int64
	Page              int
	TotalPages        int
	Result            *models.SearchResult
	SidebarTags       []models.Tag
	FolderTree        []gallery.FolderNode
	SourceLabelCounts []gallery.SourceLabelCount
	SavedSearches     []models.SavedSearch
	SidebarURL        string                // populated on full-page renders so the placeholder can lazy-load the sidebar
	EnabledTaggers    []tagger.TaggerStatus // gates the gallery's Auto-tag controls; mirrors detailData.EnabledTaggers
	TaggersPresent    bool                  // mirrors detailData.TaggersPresent
	TaggerReason      string
	ActiveTagTerms    map[string]bool // top-level AND-positive terms in the current query, keyed by both "category:name" and bare "name"; drives the sidebar + / - toggle
	// InboxClusterAtIdx is the parallel-to-Result.Results slice that
	// pins a cluster header in front of the cards that start a fresh
	// time cluster; nil entries are the in-cluster rows that don't
	// emit a header. Populated only when the inbox-cluster activation
	// gate triggers (q carries a positive inbox:true leaf and the
	// sort is newest-DESC); nil slice otherwise.
	InboxClusterAtIdx []*inboxCluster
	// InboxUploadActive lights the inline drop zone that renders at
	// the top of the gallery grid whenever the query positively
	// asserts inbox:true at the top level. Independent of sort/order
	// so changing sort doesn't make the upload affordance disappear
	// while the operator is still triaging the inbox.
	InboxUploadActive bool
	AcceptFileTypes   string // upload-zone accept= attribute; mirrors the value /upload uses
	// SimilarityPercent keys the page's own ids to their score against
	// the query's similar: seed. Nil for every other query; a missing
	// id scored nothing and gets no badge.
	SimilarityPercent map[int64]int
	// SortSelectOOB marks the shared sort-select partial as an
	// out-of-band swap, which the HTMX fragment needs and the full-page
	// render must not carry.
	SortSelectOOB bool
	// PluginSlot is the batch-bar mount point: the peers' relay buttons for
	// the current selection, absent when no peer offers any.
	PluginSlot pluginSlotView
}

// inboxCluster describes one batch of time-adjacent inbox entries
// the gallery groups visually so the operator can act on the whole
// batch via a single [Select] click. The struct rides parallel to
// the result slice; rendering happens in partials/thumbnail_grid.html.
type inboxCluster struct {
	Count      int
	DateLabel  string // "2026-05-22"
	RangeLabel string // "14:32 -> 14:36" (or just "14:32" for a singleton)
	// RangeLink resolves to a /?q=... URL that the cluster header's
	// date range doubles as so a cluster whose tail spans into the
	// next page can still be acted on as a whole via the batch bar's
	// scope=search path.
	RangeLink string
}

func (s *Server) galleryHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		logx.Warnf("gallery search parse: %v", parseErr)
	}
	sortStr := q.Get("sort")
	orderStr := q.Get("order")
	// A similar: term asks for a ranking, so it picks the sort the way a
	// collection: term picks collection order below. An explicit sort
	// still wins, which leaves `similar:1 sort=newest` a plain filtered
	// browse.
	if sortStr == "" && search.HasSimilarTerm(expr) {
		sortStr = "similarity"
	}
	// A similarity sort carried over from a previous query has nothing to
	// rank against, and the executor already falls back to newest. Fold it
	// here too so the Sort select doesn't advertise an order the grid is
	// not in.
	if sortStr == "similarity" && !search.HasSimilarTerm(expr) {
		sortStr = "newest"
	}
	// Collection shortcut links carry no sort/order; read them in series
	// order instead of ingest order. An explicit sort/order still wins.
	if sortStr == "" && orderStr == "" && collectionFilterActive(expr) {
		sortStr, orderStr = "order", "asc"
	}
	sortStr = cmp.Or(sortStr, "newest")
	orderStr = cmp.Or(orderStr, search.DefaultOrder(sortStr))
	pageStr := q.Get("page")
	page := 1
	pageNonPositive := false
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil {
			if p > 0 {
				page = p
			} else {
				// `?page=0` or `?page=-1` would render page 1 but leave
				// the URL pointing at the bogus value; flag for the
				// clamp+redirect path below so bookmark coherence matches
				// the past-end branch.
				pageNonPositive = true
			}
		}
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
				// Clamp to 31 bits (and force odd) so SQLite's int64
				// arithmetic on `(i.id * seed) & 2147483647` stays
				// inside int64 for any reasonable image id and the
				// low bits of the product remain uniform. A 63-bit
				// seed overflows on multiplication; the result coerces
				// to REAL and the low-bit mask becomes near-monotonic
				// in id, surfacing as identity-ordered "random" pages.
				randomSeed = int64(binary.BigEndian.Uint32(seedBytes) | 1)
			} else {
				randomSeed = time.Now().UnixNano() & 0x7FFFFFFF
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

	ceiling := resolveCeiling(r, s.Active())
	pinnedCollection := search.PinnedCollectionName(expr)
	expr = ceiling.Apply(expr)
	pageSize := s.pageSize()
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		RandomSeed: randomSeed,
		Page:       page,
		Limit:      pageSize,
		CacheKey:   search.BuildAdjacencyCacheKey(s.activeGallery(), queryStr, sortStr, orderStr, randomSeed, ceiling.Level()),
	}
	if sortStr == "order" {
		sq.OrderCollection = pinnedCollection
	}
	// Unfiltered browse hits the full-visible count on every page; serve it
	// from the per-gallery cache to skip the O(N) index scan. The cache
	// counts every visible image, so it overcounts when a ceiling is on -
	// fall back to fastCountCeiling in that case.
	if expr == nil {
		if cx := s.Active(); cx != nil {
			if n, err := cx.VisibleCount(); err == nil {
				sq.PresetTotal = &n
			}
		}
	}

	htmxGridTarget := isHTMXRequest(r) && r.Header.Get("HX-Target") == "gallery-grid"

	firstStart := time.Now()
	result, err := search.Execute(s.db(), sq)
	if err != nil {
		logx.Errorf("gallery search: %v", err)
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}
	firstElapsed := time.Since(firstStart)

	totalPages := 1
	if pageSize > 0 {
		totalPages = (result.Total + pageSize - 1) / pageSize
	}

	// If a concurrent ingestion or delete shrank the result set out from under
	// the user's current page (e.g. the auto-refresh re-fetches page 3 after
	// deletions dropped the total to 1 page), re-run at the last valid page
	// so the grid isn't empty while "N images" still shows a non-zero count.
	// Sync the URL so a bookmark of the clamped view doesn't keep replaying
	// the bogus page number. `?page=0` / `?page=-1` ride the same path so
	// the past-end and below-1 branches share the redirect.
	if (result.Total > 0 && page > totalPages) || pageNonPositive {
		if page > totalPages {
			page = totalPages
			sq.Page = page
			result, err = search.Execute(s.db(), sq)
			if err != nil {
				logx.Errorf("gallery search (clamped): %v", err)
				http.Error(w, "search error", http.StatusInternalServerError)
				return
			}
		}
		if page < 1 {
			// An empty result set leaves totalPages at 0, so the clamp
			// above can't lift a `?page=0` off itself and the redirect
			// would target the URL that triggered it.
			page = 1
		}
		clampedQ := r.URL.Query()
		clampedQ.Set("page", strconv.Itoa(page))
		clampedURL := "/?" + clampedQ.Encode()
		if isHTMXRequest(r) {
			w.Header().Set("HX-Push-Url", clampedURL)
		} else {
			http.Redirect(w, r, clampedURL, http.StatusSeeOther)
			return
		}
	}

	// Compute the "N hidden" indicator: unfiltered total minus the
	// ceiling-aware total. Filtered queries probe the bare-expr
	// adjacency cache, then fall back to a COUNT-only Execute. The
	// budget guard skips the fallback when the first Execute already
	// burned the search budget so the render degrades gracefully.
	hiddenByCeiling := 0
	if ceiling.IsActive() {
		rawTotal := -1
		bareExpr, _ := search.Parse(queryStr)
		switch {
		case bareExpr == nil:
			if cx := s.Active(); cx != nil {
				if n, err := cx.VisibleCount(); err == nil {
					rawTotal = n
				}
			}
		case firstElapsed < galleryHiddenIndicatorBudget:
			bareKey := search.BuildAdjacencyCacheKey(s.activeGallery(), queryStr, sortStr, orderStr, randomSeed, "")
			if cachedIDs, ok := search.AdjacencyCacheGet(bareKey); ok {
				rawTotal = len(cachedIDs)
			} else {
				rawResult, err := search.Execute(s.db(), search.Query{
					Expr: bareExpr, Sort: sortStr, Order: orderStr,
					RandomSeed: randomSeed, Page: 1, Limit: 1,
				})
				if err == nil {
					rawTotal = rawResult.Total
				}
			}
		}
		if rawTotal > result.Total {
			hiddenByCeiling = rawTotal - result.Total
		}
	}

	// Full-page renders ship the sidebar as a placeholder that lazy-loads via
	// GET /internal/sidebar, so first paint isn't blocked on the folder-tree
	// aggregation. Search/pagination HTMX responses still need the sidebar
	// content in the same payload because gallery_htmx.html OOB-swaps it into
	// the live page - unless the operator collapsed the column, in which case
	// both paths ship the placeholder and the reads wait until it is shown.
	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}

	var sb sidebarBundle
	if htmxGridTarget && !sidebarCollapsed(r) {
		sb = s.sidebarLoad(ids, ceiling)
	}

	taggerCfg := s.cfgSnapshot()
	data := galleryData{
		baseData:          s.base(r, "gallery", "Images - "+s.booruName()),
		Query:             queryStr,
		SearchWarning:     searchWarning(expr),
		Sort:              sortStr,
		Order:             orderStr,
		ThumbnailFit:      s.thumbnailFit(),
		RandomSeed:        randomSeed,
		Page:              page,
		TotalPages:        totalPages,
		Result:            result,
		SidebarTags:       sb.Tags,
		FolderTree:        sb.Folders,
		SourceLabelCounts: sb.SourceLabels,
		SavedSearches:     sb.Saved,
		EnabledTaggers:    tagger.EnabledTaggersForGallery(taggerCfg, s.activeGallery()),
		TaggersPresent:    tagger.Present(taggerCfg),
		TaggerReason:      tagger.UnavailableReason(taggerCfg),
		PluginSlot:        s.pluginSlot(r, config.SlotBatchBar, 0, ""),
		ActiveTagTerms:    computeActiveTagTerms(queryStr),
	}
	// A similar: query ranks by a number the grid otherwise never shows;
	// scoring the page's own ids puts it on the thumbnails.
	if seedID, ok := search.SimilaritySeedID(expr); ok {
		scores, err := tags.OverlapPercentsAgainst(s.db(), seedID, ids)
		if err != nil {
			logx.Debugf("gallery similarity scores: %v", err)
		}
		data.SimilarityPercent = scores
	}
	if inboxClustersActive(sortStr, orderStr, expr) {
		data.InboxClusterAtIdx = computeInboxClusters(result.Results, queryStr)
	}
	if inboxFilterActive(expr) {
		data.InboxUploadActive = true
		data.AcceptFileTypes = gallery.SupportedMIMETypes
	}
	data.HiddenByCeiling = hiddenByCeiling
	// Both paths carry it: the collapsed placeholder the HTMX response swaps in
	// has to hx-get this search's sidebar, not the one the page opened on.
	data.SidebarURL = buildSidebarURL(queryStr, sortStr, orderStr, pageStr, q.Get("seed"), ids)

	if htmxGridTarget {
		// The header is outside the swap target, so the sort select rides
		// the fragment as an out-of-band swap.
		data.SortSelectOOB = true
		s.renderTemplate(w, "partials/gallery_htmx.html", data)
		return
	}
	// The batch-strip dialog's source select rides SourceLabelCounts even on
	// the full-page render, where the sidebar (and its labels) is lazy-loaded;
	// fill it from the cheap cached, ceiling-blind count.
	if cx := s.Active(); cx != nil {
		data.SourceLabelCounts, _ = cx.SourceLabelCounts()
	}
	s.renderTemplate(w, "gallery.html", data)
}

// sidebarBundle is the parallel-fetched payload that populates the
// gallery sidebar - tags from the current page, folder tree, AI source
// breakdown, top series + source labels, favourite tallies, saved
// searches. Bundling them in one struct keeps the goroutine fan at
// sidebarLoad readable as the count grows.
type sidebarBundle struct {
	Tags         []models.Tag
	Folders      []gallery.FolderNode
	SourceLabels []gallery.SourceLabelCount
	Saved        []models.SavedSearch
}

// loadSavedSearches reads every saved search, name-ordered, for the
// sidebar renders. logLabel names the calling surface in scan warnings.
func (s *Server) loadSavedSearches(logLabel string) []models.SavedSearch {
	out, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (models.SavedSearch, error) {
		var ss models.SavedSearch
		err := rows.Scan(&ss.ID, &ss.Name, &ss.Query, &ss.Sort, &ss.Order, &ss.Seed)
		return ss, err
	}, `SELECT id, name, query, sort, sort_order, seed FROM saved_searches ORDER BY name`)
	if err != nil {
		logx.Warnf("%s saved searches: %v", logLabel, err)
	}
	return out
}

// sidebarLoad runs the reads that populate the gallery sidebar.
// Two background goroutines cover the work that always touches the
// DB - the per-page tag aggregation against image_tags and the
// saved_searches scan. Everything else reads the per-cx atomic
// caches that warmCaches primes at gallery open, so it runs inline
// instead of fanning out a goroutine per sub-query (each grabbing a
// slot against the read pool under c>1, which doubles the cheap-
// shape sidebar latency). On cold cache the inline reads pay the
// query cost sequentially - rare enough (sidebar warmup runs at
// gallery open and after every cache invalidation) that the warm-
// path simplification is the right tradeoff.
//
// ceiling drives the per-image aggregates so the sidebar reflects what
// the operator sees in the gallery. A nil or inactive ceiling reads
// the existing blind caches, leaving the no-ceiling steady state
// untouched.
func (s *Server) sidebarLoad(pageImageIDs []int64, ceiling *Ceiling) sidebarBundle {
	var sb sidebarBundle
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sb.Tags, _ = search.SidebarTagsWithGlobalCount(s.db(), pageImageIDs)
	}()
	go func() {
		defer wg.Done()
		sb.Saved = s.loadSavedSearches("sidebar")
	}()
	if cx := s.Active(); cx != nil {
		sb.Folders, _ = cx.FolderTreeUnder(ceiling)
		sb.SourceLabels, _ = cx.SourceLabelCountsUnder(ceiling)
	}
	wg.Wait()
	return sb
}

// gallerySidebar renders the gallery sidebar partial on demand. Initial
// full-page gallery renders ship an empty #sidebar-inner placeholder that
// hx-gets this endpoint on load - same pattern as the detail page's
// related-images lazy fetch.
func (s *Server) gallerySidebar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	ceiling := resolveCeiling(r, s.Active())

	// galleryHandler piggy-backs the page's image IDs on the lazy-load URL
	// so we don't re-run search.Execute just to enumerate them. A direct
	// hit (no ids param at all) falls back to the search call so the
	// endpoint still works on its own.
	var ids []int64
	if q.Has("ids") {
		if raw := q.Get("ids"); raw != "" {
			ids = make([]int64, 0, strings.Count(raw, ",")+1)
			for _, s := range strings.Split(raw, ",") {
				if id, err := strconv.ParseInt(s, 10, 64); err == nil {
					ids = append(ids, id)
				}
			}
		}
	} else {
		sortStr := q.Get("sort")
		sortStr = cmp.Or(sortStr, "newest")
		orderStr := q.Get("order")
		orderStr = cmp.Or(orderStr, search.DefaultOrder(sortStr))
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
		expr = ceiling.Apply(expr)
		sq := search.Query{
			Expr:       expr,
			Sort:       sortStr,
			Order:      orderStr,
			RandomSeed: randomSeed,
			Page:       page,
			Limit:      s.pageSize(),
			SkipCount:  true,
		}
		result, err := search.Execute(s.db(), sq)
		if err != nil {
			logx.Errorf("sidebar search: %v", err)
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
		ids = make([]int64, 0, len(result.Results))
		for _, img := range result.Results {
			ids = append(ids, img.ID)
		}
	}

	sb := s.sidebarLoad(ids, ceiling)

	s.renderTemplate(w, "partials/sidebar_content.html", map[string]any{
		"Query":             queryStr,
		"CSRFToken":         s.csrfToken(sessionFromContext(r.Context())),
		"SidebarTags":       sb.Tags,
		"FolderTree":        sb.Folders,
		"SourceLabelCounts": sb.SourceLabels,
		"SavedSearches":     sb.Saved,
		"ActiveTagTerms":    computeActiveTagTerms(queryStr),
	})
}

// sidebarBrowse renders the folder/source/saved-searches sections only -
// no per-page tag groups. Lazy-loaded from the detail page so its sidebar
// gets the same browse shortcuts the gallery sidebar does without paying
// the folder-tree aggregation cost on first paint.
func (s *Server) sidebarBrowse(w http.ResponseWriter, r *http.Request) {
	queryStr := r.URL.Query().Get("q")
	ceiling := resolveCeiling(r, s.Active())

	var (
		folders      []gallery.FolderNode
		sourceLabels []gallery.SourceLabelCount
		saved        []models.SavedSearch
	)
	// Same shape as sidebarLoad: only the saved-search read fans out. A
	// goroutine per cached cx read takes its own slot against the read pool,
	// which costs more under concurrent viewers than the reads save.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		saved = s.loadSavedSearches("sidebar-browse")
	}()
	if cx := s.Active(); cx != nil {
		folders, _ = cx.FolderTreeUnder(ceiling)
		sourceLabels, _ = cx.SourceLabelCountsUnder(ceiling)
	}
	wg.Wait()

	s.renderTemplate(w, "partials/sidebar_browse.html", map[string]any{
		"Query":             queryStr,
		"CSRFToken":         s.csrfToken(sessionFromContext(r.Context())),
		"FolderTree":        folders,
		"SourceLabelCounts": sourceLabels,
		"SavedSearches":     saved,
	})
}

// buildSidebarURL constructs the URL the #sidebar-inner placeholder hx-gets
// on full-page gallery renders, mirroring buildGalleryURL's encoding style
// so the sidebar handler sees the same q/sort/order/page/seed the page does.
// ids carries the page's image IDs as a comma-separated list so
// gallerySidebar can skip re-running search.Execute. The param is always
// set (even when empty) because absence is the signal for a direct URL hit
// that must fall back to the search call.
func buildSidebarURL(q, sort, order, page, seed string, ids []int64) string {
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
	var sb strings.Builder
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	v.Set("ids", sb.String())
	return "/internal/sidebar?" + v.Encode()
}

// computeActiveTagTerms walks the parsed query and collects the leaves a
// sidebar "+" button should render as "-" (toggle-off). Only top-level
// AND-positive exact leaves qualify - leaves nested under NOT or OR, and
// wildcarded tags, are skipped because removing one term in those shapes
// changes the search semantics in a way the toggle can't honestly express.
// Keys are lowercased so the lookup matches the sidebar's "category:name"
// and bare-name forms regardless of how the user typed them.
func computeActiveTagTerms(query string) map[string]bool {
	set := make(map[string]bool)
	expr, err := search.Parse(query)
	if err != nil || expr == nil {
		return set
	}
	search.WalkAndedLeaves(expr, func(e search.Expr) {
		switch v := e.(type) {
		case search.TagExpr:
			if v.Tag != "" && v.Wildcard == "" {
				set[v.Tag] = true
			}
		case search.FilterExpr:
			set[v.Key+":"+strings.ToLower(v.Val)] = true
		}
	})
	return set
}

// inboxFilterActive reports whether the parsed query positively
// asserts inbox:true at the top level. The walk descends into
// top-level AndExpr only, so `-inbox:true` (a NotExpr wrap) and
// inbox:true buried under OR don't trigger. Shared gate for both
// the inbox-cluster headers and the inline upload drop zone.
func inboxFilterActive(expr search.Expr) bool {
	if expr == nil {
		return false
	}
	var found bool
	search.WalkAndedLeaves(expr, func(e search.Expr) {
		if v, ok := e.(search.FilterExpr); ok && v.Key == "inbox" && strings.ToLower(v.Val) == "true" {
			found = true
		}
	})
	return found
}

// collectionFilterActive reports whether the parsed query positively
// asserts a non-empty collection: filter at the top level. The walk
// descends into top-level AndExpr only, so a negated or OR-nested
// collection leaf doesn't trigger. Gates the Order-ascending sort
// default for collection shortcut links.
func collectionFilterActive(expr search.Expr) bool {
	if expr == nil {
		return false
	}
	var found bool
	search.WalkAndedLeaves(expr, func(e search.Expr) {
		if v, ok := e.(search.FilterExpr); ok && v.Key == "collection" && v.Val != "" {
			found = true
		}
	})
	return found
}

// searchWarning returns a note when the query carries a closed-vocabulary
// filter whose value is unrecognised, so an empty result reads as "bad
// value" rather than "no images". Empty when the query is clean.
func searchWarning(expr search.Expr) string {
	unknown := unknownFilterValues(expr)
	if len(unknown) == 0 {
		return ""
	}
	return "Unknown filter value: " + strings.Join(unknown, ", ")
}

// unknownFilterValues collects key:value leaves whose value falls outside a
// closed-vocabulary keyword's set. Descends the whole tree so a negated or
// OR-nested bad value is caught too.
func unknownFilterValues(expr search.Expr) []string {
	if expr == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	search.WalkLeaves(expr, func(e search.Expr) bool {
		if v, ok := e.(search.FilterExpr); ok && !searchkw.ValueKnown(v.Key, v.Val) {
			token := v.Key + ":" + v.Val
			if !seen[token] {
				seen[token] = true
				out = append(out, token)
			}
		}
		return true
	})
	return out
}

// inboxClustersActive narrows inboxFilterActive to the only sort
// shape time-cluster headers make sense for: newest-DESC. Wildcarded
// sorts (filesize, random) still light the upload drop zone, but
// the time-cluster geometry only reads correctly in newest-DESC.
func inboxClustersActive(sort, order string, expr search.Expr) bool {
	if sort != "newest" || order != "desc" {
		return false
	}
	return inboxFilterActive(expr)
}

// computeInboxClusters walks the page's images in newest-DESC order
// and emits a header marker at every cluster boundary. Web-UI uploads
// carry a per-POST upload_batch token and break by that token (one
// drop is one cluster, whatever the timing); watcher / sync rows carry
// no token and break on a gap of more than batchGapMinutes. The
// returned slice is parallel to images; nil entries are the in-cluster
// rows that don't emit a header. queryStr is the operator's current
// search; the cluster header's date-range link extends it with a
// date:T1..T2 leaf so the batch bar's scope=search path can act on the
// whole cluster even when the tail crosses a page boundary.
func computeInboxClusters(images []models.Image, queryStr string) []*inboxCluster {
	if len(images) == 0 {
		return nil
	}
	markers := make([]*inboxCluster, len(images))
	gap := time.Duration(batchGapMinutes) * time.Minute
	start := 0
	for i := 1; i <= len(images); i++ {
		// Close the open cluster at i (one past its last row) at the
		// boundary between row i-1 (newer) and row i (older, newest-DESC).
		closeCluster := i == len(images)
		if !closeCluster {
			prevB, nextB := images[i-1].UploadBatch, images[i].UploadBatch
			switch {
			case prevB != nil || nextB != nil:
				// An upload batch boundary - a different token, or a batch
				// meeting a watcher/sync row - cuts the cluster regardless
				// of the time gap.
				closeCluster = !sameBatch(prevB, nextB)
			default:
				// Watcher/sync rows break on inactivity. >= so a schedule
				// landing on the exact gap boundary opens a fresh batch,
				// matching "every 15 minutes is a new cluster".
				closeCluster = images[i-1].IngestedAt.Sub(images[i].IngestedAt) >= gap
			}
		}
		if closeCluster {
			markers[start] = buildInboxCluster(images[start:i], queryStr)
			start = i
		}
	}
	return markers
}

// sameBatch reports whether two rows carry the same non-nil upload-batch
// token. Two nil tokens are not the same batch - those rows fall back to
// the time-gap rule.
func sameBatch(a, b *int64) bool {
	return a != nil && b != nil && *a == *b
}

func buildInboxCluster(rows []models.Image, queryStr string) *inboxCluster {
	// rows[0] is the newest entry (DESC), rows[len-1] the oldest.
	// Header labels and the date: bounds both read in the operator's
	// local zone - the date filter interprets its values there too.
	// The visible arrow reads oldest -> newest, left-to-right.
	newest := rows[0].IngestedAt.In(time.Local)
	oldest := rows[len(rows)-1].IngestedAt.In(time.Local)
	dateLabel := newest.Format("2006-01-02")
	rangeLabel := oldest.Format("15:04")
	if len(rows) > 1 && !oldest.Equal(newest) {
		rangeLabel = oldest.Format("15:04") + " -> " + newest.Format("15:04")
	}
	// Minute-precise bounds so a cluster spanning 19:23 -> 19:30 lands
	// on exactly the rows whose ingest minute falls in that window;
	// day-precise bounds would widen the link to the whole day.
	clusterQ := "inbox:true date:" + oldest.Format("2006-01-02T15:04") + ".." + newest.Format("2006-01-02T15:04")
	if queryStr != "" && queryStr != "inbox:true" {
		// Preserve any extra leaves the operator added while still
		// scoping to the cluster's date range; the search merges as
		// an implicit AND.
		clusterQ = queryStr + " date:" + oldest.Format("2006-01-02T15:04") + ".." + newest.Format("2006-01-02T15:04")
	}
	// RangeLink omits sort/order so the receiving gallery handler falls
	// through to its defaults (newest, desc). The cluster gate
	// inboxClustersActive enforces newest-DESC at render time; baking
	// it back into the URL would discard any future re-sort variation.
	return &inboxCluster{
		Count:      len(rows),
		DateLabel:  dateLabel,
		RangeLabel: rangeLabel,
		RangeLink:  "/?" + url.Values{"q": []string{clusterQ}}.Encode(),
	}
}
