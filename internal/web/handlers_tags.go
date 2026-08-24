package web

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// tagsPageData embeds baseData so the layout template sees its fields as
// struct members (matching galleryData / detailData) and the tags template
// can reach its own state via direct field access.
type tagsPageData struct {
	baseData
	Tags         []models.Tag
	Categories   []models.TagCategory
	Implications map[int64][]models.Implication // direct implications keyed by parent tag id
	Total        int
	Page         int
	TotalPages   int
	CategoryID   string
	Prefix       string
	Sort         string
	Order        string
	Origin       string
	Type         string
	// CreatedAfter is the raw query value ("24h" / "7d" / "30d" or an ISO
	// timestamp from a sweep-review link) so the sidebar chips highlight
	// on the spelling the URL carries.
	CreatedAfter string
	// Conflicts narrows to names living in more than one category;
	// ConflictsTotal is the badge count on the sidebar toggle.
	Conflicts      bool
	ConflictsTotal int
	// Stale narrows to tags with source-dropped usage ("has" / "full");
	// StaleTotal and FullyStaleTotal are the two sidebar badge counts.
	Stale           string
	StaleTotal      int
	FullyStaleTotal int
	// Folded narrows to the folded originals from the last scan; FoldedTotal
	// is the sidebar badge count.
	Folded       bool
	FoldedTotal  int
	OriginCounts []tags.OriginCount
	// UsedBy narrows to tags a source has applied; UsedByLabels is the
	// sidebar's label set and UsedBySources the per-row column, keyed by
	// tag id.
	UsedBy        string
	UsedByLabels  []string
	UsedBySources map[int64][]string
	// OriginKinds classifies each origin and used-by label on the page for
	// chip coloring: "user", "auto", "ptr", or "site".
	OriginKinds map[string]string
	ShowZero    bool
	ZeroOnly    bool
	// BackQS is the resolved listing state each See-detail link carries
	// as its `back` value, so the detail page can navigate relative to
	// this search.
	BackQS string
}

// originKinds buckets the given origin labels for the template's chip
// classes. Anything that is not the operator, the PTR, or a known
// auto-tagger attribution reads as a site / import label.
func (s *Server) originKinds(labels []string) map[string]string {
	kinds := make(map[string]string, len(labels))
	var unknown []string
	for _, l := range labels {
		switch l {
		case "":
		case "user":
			kinds[l] = "user"
		case "ptr":
			kinds[l] = "ptr"
		case "auto":
			kinds[l] = "auto"
		default:
			unknown = append(unknown, l)
		}
	}
	if len(unknown) > 0 {
		autoSet, err := s.tagSvc().AutoTaggerLabels(unknown)
		if err != nil {
			logx.Warnf("classify origin labels: %v", err)
		}
		for _, l := range unknown {
			if _, ok := autoSet[l]; ok {
				kinds[l] = "auto"
			} else {
				kinds[l] = "site"
			}
		}
	}
	return kinds
}

// createdAfterCutoff resolves the created_after query value: the quick
// range tokens the sidebar emits become a UTC cutoff, anything else
// (the ISO timestamp a sweep-review link carries) passes through.
func createdAfterCutoff(raw string) string {
	now := time.Now().UTC()
	switch raw {
	case "":
		return ""
	case "24h":
		return now.Add(-24 * time.Hour).Format(time.RFC3339)
	case "7d":
		return now.AddDate(0, 0, -7).Format(time.RFC3339)
	case "30d":
		return now.AddDate(0, 0, -30).Format(time.RFC3339)
	}
	return raw
}

// tagsSidebarCounts are the sidebar's badge counts and label sets. Every
// one of them aggregates over the whole catalog regardless of which page
// the listing is showing, so together they set the floor for a /tags
// render.
type tagsSidebarCounts struct {
	Conflicts  int
	Stale      int
	FullyStale int
	Folded     int
	Origins    []tags.OriginCount
	UsedBy     []string
	Err        error
}

// tagsSidebarLoad runs the badge queries side by side, the way the
// gallery sidebar loads its own aggregates: run in sequence their scans
// add up to more than the listing they decorate.
func (s *Server) tagsSidebarLoad(typeFilter string) tagsSidebarCounts {
	var c tagsSidebarCounts
	var mu sync.Mutex
	var wg sync.WaitGroup
	svc := s.tagSvc()
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				mu.Lock()
				c.Err = cmp.Or(c.Err, err)
				mu.Unlock()
			}
		}()
	}
	run(func() (err error) { c.Conflicts, err = svc.ConflictsCount(); return })
	run(func() (err error) { c.Stale, err = svc.StaleUsageCount(); return })
	run(func() (err error) { c.FullyStale, err = svc.FullyStaleCount(); return })
	run(func() (err error) { c.Folded, err = svc.FoldedDuplicatesCount(); return })
	run(func() (err error) { c.Origins, err = svc.OriginCounts(typeFilter); return })
	run(func() (err error) { c.UsedBy, err = svc.UsedByLabels(); return })
	wg.Wait()
	return c
}

func (s *Server) tagsHandler(w http.ResponseWriter, r *http.Request) {
	// The tags page reflects rapidly-changing state (category re-assignment,
	// merges). Opt out of browser caching so a reload after a mutation never
	// serves a stale render.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	catIDStr := q.Get("cat")
	prefix := q.Get("q")
	// `?q=character:` (a category prefix with no tag-name suffix) is a
	// dead end against tags.name (no tag carries a colon by spec). Mirror
	// the autocomplete's branch and route to the category-only filter so
	// the user's intent surfaces instead of "No tags found".
	if catIDStr == "" && prefix != "" && strings.HasSuffix(prefix, ":") && strings.Count(prefix, ":") == 1 {
		catName := strings.TrimSuffix(prefix, ":")
		if catName != "" && s.categoryExists(catName) {
			var catID int64
			if err := s.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, catName).Scan(&catID); err == nil {
				dst := r.URL
				vals := dst.Query()
				vals.Del("q")
				vals.Set("cat", strconv.FormatInt(catID, 10))
				dst.RawQuery = vals.Encode()
				http.Redirect(w, r, dst.String(), http.StatusSeeOther)
				return
			}
		}
	}
	p := tagListingParamsFrom(q)

	tagList, total, err := s.tagSvc().ListTags(s.tagListingFilter(p))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cats, _ := s.tagSvc().ListCategories()
	totalPages := (total + 99) / 100

	// Clamp past-the-end pages to the last valid one and re-run, mirroring
	// the gallery handler. Without this the header reads `Tags <total>`
	// while the body says "No tags found" when a stale ?page=N URL
	// survives a tag prune.
	if total > 0 && p.Page > totalPages {
		p.Page = totalPages
		tagList, total, err = s.tagSvc().ListTags(s.tagListingFilter(p))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	parentIDs := make([]int64, 0, len(tagList))
	for _, t := range tagList {
		if !t.IsAlias {
			parentIDs = append(parentIDs, t.ID)
		}
	}
	imps, err := s.tagSvc().ImplicationsForParents(parentIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	counts := s.tagsSidebarLoad(p.Type)
	if counts.Err != nil {
		http.Error(w, counts.Err.Error(), http.StatusInternalServerError)
		return
	}
	rowIDs := make([]int64, 0, len(tagList))
	for _, t := range tagList {
		rowIDs = append(rowIDs, t.ID)
	}
	usedBySources, err := s.tagSvc().UsedByForTags(rowIDs, counts.UsedBy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pageLabels := make([]string, 0, len(tagList)+len(counts.Origins)+len(counts.UsedBy))
	for _, t := range tagList {
		pageLabels = append(pageLabels, t.Origin)
	}
	for _, oc := range counts.Origins {
		pageLabels = append(pageLabels, oc.Label)
	}
	pageLabels = append(pageLabels, counts.UsedBy...)

	data := tagsPageData{
		baseData:        s.base(r, "tags", "Tags - "+s.booruName()),
		Tags:            tagList,
		Categories:      cats,
		Implications:    imps,
		Total:           total,
		Page:            p.Page,
		TotalPages:      totalPages,
		CategoryID:      p.CatID,
		Prefix:          p.Prefix,
		Sort:            p.Sort,
		Order:           p.Order,
		Origin:          p.Origin,
		Type:            p.Type,
		CreatedAfter:    p.CreatedAfter,
		Conflicts:       p.Conflicts,
		ConflictsTotal:  counts.Conflicts,
		Stale:           p.Stale,
		StaleTotal:      counts.Stale,
		FullyStaleTotal: counts.FullyStale,
		Folded:          p.Folded,
		FoldedTotal:     counts.Folded,
		OriginCounts:    counts.Origins,
		UsedBy:          p.UsedBy,
		UsedByLabels:    counts.UsedBy,
		UsedBySources:   usedBySources,
		OriginKinds:     s.originKinds(pageLabels),
		ShowZero:        p.ShowZero,
		ZeroOnly:        p.ZeroOnly,
		BackQS:          p.backQS(),
	}
	s.renderTemplate(w, "tags.html", data)
}

// tagListingParams are the /tags query values after the page's
// defaulting rules. The detail page re-resolves its back context
// through the same struct so prev/next walk the exact listing order.
type tagListingParams struct {
	CatID, Prefix, Sort, Order, Origin, Type, CreatedAfter, ZeroParam, Stale, UsedBy string
	HasType, Conflicts, ShowZero, ZeroOnly, Folded                                   bool
	Page                                                                             int
}

func tagListingParamsFrom(q url.Values) tagListingParams {
	p := tagListingParams{
		CatID:        q.Get("cat"),
		Prefix:       q.Get("q"),
		Sort:         q.Get("sort"),
		Order:        q.Get("order"),
		Origin:       q.Get("origin"),
		UsedBy:       q.Get("used_by"),
		Type:         q.Get("type"),
		HasType:      q.Has("type"),
		CreatedAfter: q.Get("created_after"),
		Conflicts:    q.Get("conflicts") == "1",
		ZeroParam:    q.Get("show_zero"),
		Page:         1,
	}
	if s := q.Get("stale"); s == "has" || s == "full" {
		p.Stale = s
	}
	p.Folded = q.Get("folded") == "1"
	p.Sort = cmp.Or(p.Sort, "usage")
	if p.Order != "asc" && p.Order != "desc" {
		// Default to the natural reading direction per sort: most-used /
		// newest / most recently applied first, alphabetical A→Z for name.
		switch p.Sort {
		case "usage", "created", "last_used":
			p.Order = "desc"
		default:
			p.Order = "asc"
		}
	}
	// Plain tags by default; alias rows surface via the explicit sidebar
	// filter (whose links always carry a type=, so "All" stays reachable).
	// The legacy origin=alias spelling opts out - it selects alias rows by
	// structure and would otherwise always come back empty.
	if !p.HasType && p.Origin != "alias" {
		p.Type = "tag"
	}
	// show_zero is tri-state: empty/"1" → Show (default so freshly-declared
	// tags surface without a filter flip); "0" → Hide; "only" → only zero-
	// usage rows (triage view).
	p.ZeroOnly = p.ZeroParam == "only"
	p.ShowZero = p.ZeroOnly || p.ZeroParam != "0"
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 0 {
		p.Page = n
	}
	return p
}

func (s *Server) tagListingFilter(p tagListingParams) tags.TagFilter {
	f := s.buildTagFilter(p.CatID, p.Prefix, p.Sort, p.Order, p.Origin, p.Type, p.CreatedAfter, p.ShowZero, p.ZeroOnly, p.Page, 100)
	f.ConflictsOnly = p.Conflicts
	f.Stale = p.Stale
	f.FoldedOnly = p.Folded
	f.UsedBy = p.UsedBy
	return f
}

// backQS encodes the resolved listing state as the `back` value the
// detail links carry. Keys whose absence differs from an empty value
// (type's all-vs-default split) are always written; the rest only when
// set, so the string stays short on the default view.
func (p tagListingParams) backQS() string {
	v := url.Values{}
	if p.Prefix != "" {
		v.Set("q", p.Prefix)
	}
	v.Set("sort", p.Sort)
	v.Set("order", p.Order)
	if p.HasType || p.Type != "" {
		v.Set("type", p.Type)
	}
	if p.CatID != "" {
		v.Set("cat", p.CatID)
	}
	if p.Origin != "" {
		v.Set("origin", p.Origin)
	}
	if p.UsedBy != "" {
		v.Set("used_by", p.UsedBy)
	}
	if p.CreatedAfter != "" {
		v.Set("created_after", p.CreatedAfter)
	}
	if p.Conflicts {
		v.Set("conflicts", "1")
	}
	if p.Stale != "" {
		v.Set("stale", p.Stale)
	}
	if p.Folded {
		v.Set("folded", "1")
	}
	if p.ZeroParam != "" {
		v.Set("show_zero", p.ZeroParam)
	}
	if p.Page > 1 {
		v.Set("page", strconv.Itoa(p.Page))
	}
	return v.Encode()
}

func (s *Server) buildTagFilter(catIDStr, prefix, sortStr, orderStr, originStr, typeStr, createdAfterRaw string, showZero, zeroOnly bool, page, limit int) tags.TagFilter {
	f := tags.TagFilter{
		Prefix:       prefix,
		Sort:         sortStr,
		Order:        orderStr,
		PageIndex:    page - 1,
		Limit:        limit,
		Origin:       originStr,
		Type:         typeStr,
		CreatedAfter: createdAfterCutoff(createdAfterRaw),
		ShowZero:     showZero,
		ZeroOnly:     zeroOnly,
	}
	if catIDStr != "" {
		// The sidebar buttons emit the id; a hand-edited URL is likelier
		// to carry the name, which every other /tags filter takes.
		if id, err := strconv.ParseInt(catIDStr, 10, 64); err == nil {
			f.CategoryID = &id
		} else if id, ok := s.categoryIDByName(catIDStr); ok {
			f.CategoryID = &id
		}
	}
	return f
}

// resolveCanonicalTagInput resolves a "name", "category:name", or id
// input to a tag id. With create set, a missing name is minted via
// GetOrCreateTag - the implications dialog's parseTagInput →
// GetOrCreateTag flow, so users can declare an alias or edge to a
// still-pending name; without it the input must name an existing tag.
// A numeric input always requires the id to exist (a typo'd id
// shouldn't silently mint a fresh tag).
func (s *Server) resolveCanonicalTagInput(input string, create bool) (int64, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, "Tag name is required."
	}
	if id, err := strconv.ParseInt(input, 10, 64); err == nil {
		var exists int
		if err := s.db().Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE id = ?`, id).Scan(&exists); err != nil || exists == 0 {
			return 0, "Tag not found: " + input
		}
		return id, ""
	}
	if idx := strings.Index(input, ":"); idx > 0 && s.categoryExists(input[:idx]) {
		catName := input[:idx]
		tagName := strings.TrimSpace(input[idx+1:])
		if tagName == "" {
			return 0, "Tag name is required after the category prefix."
		}
		var catID int64
		if err := s.db().Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, catName,
		).Scan(&catID); err != nil {
			return 0, "Category not found: " + catName
		}
		if !create {
			var id int64
			if err := s.db().Read.QueryRow(
				`SELECT id FROM tags WHERE name = ? AND category_id = ?`, tagName, catID,
			).Scan(&id); err != nil {
				return 0, "Tag not found: " + input
			}
			return id, ""
		}
		tag, err := s.tagSvc().GetOrCreateTag(tagName, catID)
		if err != nil {
			return 0, err.Error()
		}
		return tag.ID, ""
	}
	ids, err := db.QueryIDs(s.db().Read, `SELECT id FROM tags WHERE name = ?`, input)
	if err != nil {
		return 0, "Tag lookup failed: " + err.Error()
	}
	switch len(ids) {
	case 1:
		return ids[0], ""
	case 0:
		if !create {
			return 0, "Tag not found: " + input
		}
		cx := s.Active()
		if cx == nil || cx.GeneralCategoryID == 0 {
			return 0, "Could not resolve the general category."
		}
		tag, err := s.tagSvc().GetOrCreateTag(input, cx.GeneralCategoryID)
		if err != nil {
			return 0, err.Error()
		}
		return tag.ID, ""
	default:
		return 0, "Tag name " + input + " exists in multiple categories; use category:name or the tag ID"
	}
}

func (s *Server) createTagPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	catIDStr := r.FormValue("category_id")

	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		externalErr(w, r, "Invalid category.", http.StatusBadRequest)
		return
	}
	if _, err := s.tagSvc().GetOrCreateTag(name, catID); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, "Tag "+name+" created.", "/tags?q="+url.QueryEscape(name), "/tags")
}

func (s *Server) createAliasPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	catIDStr := r.FormValue("category_id")
	canonInput := strings.TrimSpace(r.FormValue("canonical_id"))

	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		externalErr(w, r, "Invalid category.", http.StatusBadRequest)
		return
	}
	canonID, msg := s.resolveCanonicalTagInput(canonInput, true)
	if msg != "" {
		externalErr(w, r, msg, http.StatusBadRequest)
		return
	}

	if _, err := s.tagSvc().CreateAlias(name, catID, canonID); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()

	hxDone(w, r, "Alias "+name+" created.", "/tags?type=alias&q="+url.QueryEscape(name), "/tags?type=alias")
}

// addTagAliasPost is the tag detail page's inline alias editor: each
// token in `name` becomes an alias pointing at {id}. Failures flash in
// place; success refreshes the page so the new rows render.
func (s *Server) addTagAliasPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	rawInput := strings.TrimSpace(r.FormValue("name"))
	if rawInput == "" {
		writeInlineFlash(w, "err", "Alias name is required.")
		return
	}
	catTags, parseErrMsg := s.parseTagInput(rawInput)
	if parseErrMsg != "" {
		writeInlineFlash(w, "err", parseErrMsg)
		return
	}

	added := 0
	var failures []string
	for _, ct := range catTags {
		if _, err := s.tagSvc().CreateAlias(ct.name, ct.catID, id); err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		added++
	}
	if added > 0 {
		s.Active().InvalidateCaches()
	}
	switch {
	case len(failures) == 0:
		noun := "alias"
		if added != 1 {
			noun = "aliases"
		}
		hxDone(w, r, strconv.Itoa(added)+" "+noun+" created.", "", fmt.Sprintf("/tags/%d", id))
	case added > 0:
		writeInlineFlash(w, "err", "Added "+strconv.Itoa(added)+". Failed: "+strings.Join(failures, "; "))
	default:
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
	}
}

// removeTagAliasesDelete deletes every alias in one origin subgroup of the
// detail page's "Aliases pointing here" list. Alias rows carry no images, so
// the deletes run inline.
func (s *Server) removeTagAliasesDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	origin, stale := relationGroupFilter(r)
	byCanonical, err := s.tagSvc().AliasesForTagIDs([]int64{id})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	removed := 0
	for _, a := range byCanonical[id] {
		if a.Origin != origin || a.Stale != stale {
			continue
		}
		if err := s.tagSvc().DeleteTag(a.ID); err != nil {
			logx.Warnf("delete alias %d: %v", a.ID, err)
			continue
		}
		removed++
	}
	if removed > 0 {
		s.Active().InvalidateCaches()
	}
	noun := "alias"
	if removed != 1 {
		noun = "aliases"
	}
	setFlashHeader(w, strconv.Itoa(removed)+" "+noun+" removed.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTagHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tag, _ := s.tagSvc().GetTag(id)
	if err := s.tagSvc().DeleteTag(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()
	// A deleted alias row changes its canonical's relation diff; the tag
	// detail PTR panel re-fetches on this. An alias delete also confirms
	// with a flash, matching the implication remove.
	if tag != nil && tag.IsAlias {
		setFlashHeader(w, "Alias removed.", "ok", map[string]any{"tag-relations-changed": ""})
	} else {
		w.Header().Set("HX-Trigger", "tag-relations-changed")
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteTagsSearchPost deletes every tag in scope - the checkbox
// selection when ids are posted, else everything matching the posted
// /tags filter. Mirrors the gallery's /internal/delete-search: resolve
// the id set up front, kick off a background "tag" job, return 202
// Accepted so the client surfaces progress via the job status bar.
func (s *Server) deleteTagsSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.startTagScopeRun(w, r, s.runDeleteTagsByIDs)
}

// runDeleteTagsByIDs deletes the supplied tag ids one by one, reporting
// progress through the job manager and honouring cancellation.
// DeleteTag handles cascade and usage-count cleanup per row.
func (s *Server) runDeleteTagsByIDs(ids []int64) {
	deleted, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "deleting tags…", 50, func(id int64) (bool, error) {
		if err := s.tagSvc().DeleteTag(id); err != nil {
			logx.Warnf("delete tag %d: %v", id, err)
			return false, err
		}
		return true, nil
	})

	s.Active().InvalidateCaches()
	summary := skippedSuffix(fmt.Sprintf("deleted %d tag(s)", deleted), skipped)
	s.finishTagScopeJob(deleted, reasons, cancelled, "delete tags", summary)
}

func (s *Server) renameTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		externalErr(w, r, "Name required.", http.StatusBadRequest)
		return
	}
	var err error
	if r.FormValue("keep_alias") == "1" {
		err = s.tagSvc().RenameTagKeepAlias(id, newName)
	} else {
		err = s.tagSvc().RenameTag(id, newName)
	}
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	// A tag rename moves it to a new literal-name match in the search
	// resolver, so a cached `?q=oldname` snapshot must drop too.
	s.Active().InvalidateCaches()
	// Refresh the current URL instead of redirecting to /tags so the
	// user's active filter - q, sort, origin, page - survives the
	// rename and the renamed row stays in scope.
	hxDone(w, r, "Renamed to "+newName+".", "", "/tags")
}
