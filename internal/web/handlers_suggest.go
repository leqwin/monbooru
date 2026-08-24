package web

import (
	"cmp"
	"database/sql"
	"html/template"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/searchkw"
	"github.com/monbooru/monbooru/internal/tags"
)

// suggestItem is the uniform row shape of the shared suggest dropdown
// (partials/suggest_list.html). Name is both the visible label and the
// dataset value the click handler reads. Tag rows carry their category
// color and a usage count; `system:` cheat-sheet rows set Description
// instead and suppress the count; folder / label rows are name-only.
type suggestItem struct {
	Name        string
	Color       string
	Description string
	UsageCount  int
	ShowCount   bool
}

// renderSuggestList renders the shared suggest_list.html partial. attr
// names the data attribute carrying each row's value; onclick is the
// full click-handler attribute. Both are compile-time constants chosen
// per suggest surface (never user input), which is what makes the
// HTMLAttr trust markers safe; per-row Name stays with the autoescaper.
func (s *Server) renderSuggestList(w http.ResponseWriter, attr, onclick string, items []suggestItem) {
	s.renderTemplate(w, "partials/suggest_list.html", map[string]any{
		"Attr":    template.HTMLAttr(attr),
		"OnClick": template.HTMLAttr(onclick),
		"Items":   items,
	})
}

// renderSearchSuggest renders search-bar dropdown rows; shared by the
// tag, filter-keyword, and system: cheat-sheet paths of searchSuggest.
func (s *Server) renderSearchSuggest(w http.ResponseWriter, rows []suggestItem) {
	s.renderSuggestList(w, `data-tag-name`, `onclick="applySearchSuggest(this.dataset.tagName)"`, rows)
}

// suggestLabels runs a single-text-column suggest query and collects its
// non-blank values, owning the cursor. A row that will not scan is skipped
// rather than dropping the whole list: an autocomplete showing most of its
// matches beats one showing none. logLabel names the surface in the log.
func (s *Server) suggestLabels(q db.Querier, logLabel, query string, args ...any) []string {
	rows, err := q.Query(query, args...)
	if err != nil {
		logx.Warnf("%s suggest: %v", logLabel, err)
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			logx.Warnf("%s suggest: scan: %v", logLabel, err)
			continue
		}
		if v != "" {
			out = append(out, v)
		}
	}
	if err := rows.Err(); err != nil {
		logx.Warnf("%s suggest: iter: %v", logLabel, err)
	}
	return out
}

// foldersSuggest returns up to 10 existing folder paths whose name or leading
// segments match the typed prefix. Drives the autocomplete dropdown on the
// move dialogs. Root (empty folder_path) is excluded from suggestions because
// it maps to an empty input anyway.
//
// The half-open range form `folder_path >= prefix AND folder_path < prefix||X`
// (where X is one codepoint past the prefix's last char) lets SQLite seek to
// the first match and stop at the boundary - a `LIKE ?||'%'` form forces a
// full index scan because the default case-insensitive collation can't bound
// it. The empty-prefix branch keeps the simpler shape so the seek skips the
// tail-bound machinery; the planner already short-circuits via DISTINCT once
// 10 unique folder paths have surfaced. NOCASE on both bounds + the matching
// partial index keeps the suggest in step with the case-insensitive
// `folder:` search filter so capitalised paths surface from a lowercase
// prefix and vice versa.
func (s *Server) foldersSuggest(w http.ResponseWriter, r *http.Request) {
	prefix := strings.TrimSpace(r.URL.Query().Get("prefix"))
	var folders []string
	if prefix == "" {
		folders = s.suggestLabels(s.db().Read, "folders",
			`SELECT DISTINCT folder_path FROM images INDEXED BY idx_images_folder_nocase_visible
			 WHERE is_missing = 0 AND folder_path != ''
			 ORDER BY folder_path COLLATE NOCASE LIMIT 10`)
	} else {
		lo, hi := nocasePrefixRange(prefix)
		folders = s.suggestLabels(s.db().Read, "folders",
			`SELECT DISTINCT folder_path FROM images INDEXED BY idx_images_folder_nocase_visible
			 WHERE is_missing = 0
			   AND folder_path >= ? COLLATE NOCASE
			   AND folder_path < ? COLLATE NOCASE
			 ORDER BY folder_path COLLATE NOCASE LIMIT 10`,
			lo, hi)
	}
	if len(folders) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	items := make([]suggestItem, len(folders))
	for i, fp := range folders {
		items[i] = suggestItem{Name: fp}
	}
	s.renderSuggestList(w, `data-folder-path`, `onclick="applyLabelSuggest(this, 'folderPath')"`, items)
}

// collectionSuggest returns up to 10 distinct existing collection
// labels whose prefix matches what the user has typed. Drives the
// autocomplete dropdown on the detail-page collection-edit dialog and
// the batch-collection dialog. Empty prefix lists the alphabetically
// first labels so the dropdown has something to show on focus. The
// underlying column is still named `series` (kept for schema
// stability); only the user-facing vocabulary moved.
func (s *Server) collectionSuggest(w http.ResponseWriter, r *http.Request) {
	s.renderLabelSuggest(w, r, s.queryCollectionLabels)
}

// sourceSuggest mirrors collectionSuggest for the detail-page source
// edit dialog. Shares renderLabelSuggest because the rendered shape
// (one flat list of free-text labels) is identical; applyLabelSuggest
// in main.js is generic on the dropdown's nearest text input, so the
// same client handler covers both dialogs.
func (s *Server) sourceSuggest(w http.ResponseWriter, r *http.Request) {
	s.renderLabelSuggest(w, r, s.querySourceLabels)
}

// renderLabelSuggest reads the typed prefix, runs query for up to 10
// rows, and renders the shared suggest_list.html partial (204 on no
// matches). The data-series attribute name predates the collection
// rename and is kept for client-side stability.
func (s *Server) renderLabelSuggest(w http.ResponseWriter, r *http.Request, query func(string, int) []string) {
	labels := query(strings.TrimSpace(r.URL.Query().Get("prefix")), 10)
	if len(labels) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	items := make([]suggestItem, len(labels))
	for i, lbl := range labels {
		items[i] = suggestItem{Name: lbl}
	}
	s.renderSuggestList(w, `data-series`, `onclick="applyLabelSuggest(this, 'series')"`, items)
}

// queryDistinctLabels returns distinct non-empty values of a NOCASE label
// column, optionally bounded to a case-insensitive prefix, so the suggest
// stays in step with the matching case-insensitive filter. Reading the
// membership table (not the scalar mirror) surfaces secondary entries too.
func (s *Server) queryDistinctLabels(table, col, prefix string, limit int, logLabel string) []string {
	if prefix == "" {
		return s.suggestLabels(s.db().Read, logLabel,
			`SELECT DISTINCT `+col+` FROM `+table+` WHERE `+col+` != ''
			 ORDER BY `+col+` LIMIT ?`, limit)
	}
	lo, hi := nocasePrefixRange(prefix)
	return s.suggestLabels(s.db().Read, logLabel,
		`SELECT DISTINCT `+col+` FROM `+table+`
		 WHERE `+col+` >= ? AND `+col+` < ?
		 ORDER BY `+col+` LIMIT ?`, lo, hi, limit)
}

// querySourceLabels drives the `source:` autocomplete in the search-bar
// `system:` level-2 dropdown and the detail / batch source dialogs.
func (s *Server) querySourceLabels(prefix string, limit int) []string {
	return s.queryDistinctLabels("image_sources", "site", prefix, limit, "source")
}

// queryCollectionLabels drives the detail / batch dialogs and the
// search-bar `collection:` autocomplete.
func (s *Server) queryCollectionLabels(prefix string, limit int) []string {
	return s.queryDistinctLabels("image_collections", "name", prefix, limit, "collection")
}

// queryNameBasenames returns up to limit distinct lowercased file
// basenames whose name starts with prefix, sampled from non-missing
// images. Rides the basename_lower partial index via a half-open
// range seek so the underlying scan is bounded by the prefix's
// matching slice instead of the full library.
//
// The autocomplete is prefix-only on purpose: most operators type
// the start of a filename and pick from the alphabetical list. The
// name: filter itself still does substring matches; the
// autocomplete is just a way to surface candidate values fast.
func (s *Server) queryNameBasenames(prefix string, limit int) []string {
	d := s.db()
	if d == nil || prefix == "" {
		return nil
	}
	low := strings.ToLower(prefix)
	return s.suggestLabels(d.Read, "name",
		`SELECT DISTINCT basename_lower FROM images INDEXED BY idx_images_basename_lower_visible
		 WHERE is_missing = 0 AND basename_lower != ''
		   AND basename_lower >= ? AND basename_lower < ?
		 ORDER BY basename_lower LIMIT ?`,
		low, nextPrefix(low), limit)
}

// querySDStringField returns up to limit distinct values from the
// matching SD / ComfyUI metadata columns whose value matches prefix.
// substring=true switches to a `LIKE %prefix%` scan (used by `prompt:`
// where values are sentences); false uses a prefix-range scan that
// pins the underlying index. Empty prefix returns alphabetically first
// values so the dropdown has something to show on focus.
func (s *Server) querySDStringField(sdField, comfyField, prefix string, limit int, substring bool) []string {
	d := s.db()
	if d == nil {
		return nil
	}
	type pair struct{ table, field string }
	tables := []pair{{"sd_metadata", sdField}, {"comfyui_metadata", comfyField}}
	seen := make(map[string]struct{}, limit*2)
	out := make([]string, 0, limit*2)
	for _, t := range tables {
		var values []string
		switch {
		case prefix == "":
			values = s.suggestLabels(d.Read, t.field,
				`SELECT DISTINCT `+t.field+` FROM `+t.table+`
				 WHERE `+t.field+` IS NOT NULL AND `+t.field+` != ''
				 ORDER BY `+t.field+` LIMIT ?`,
				limit)
		case substring:
			values = s.suggestLabels(d.Read, t.field,
				`SELECT DISTINCT `+t.field+` FROM `+t.table+`
				 WHERE `+t.field+` LIKE ? ESCAPE '\'
				 ORDER BY `+t.field+` LIMIT ?`,
				"%"+db.EscapeLike(prefix)+"%", limit)
		default:
			values = s.suggestLabels(d.Read, t.field,
				`SELECT DISTINCT `+t.field+` FROM `+t.table+`
				 WHERE `+t.field+` >= ? AND `+t.field+` < ?
				 ORDER BY `+t.field+` LIMIT ?`,
				prefix, nextPrefix(prefix), limit)
		}
		for _, v := range values {
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
		if len(out) >= limit {
			out = out[:limit]
			break
		}
	}
	return out
}

func (s *Server) tagSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Accept the input's value however it arrives: q=, tag=, canonical_id=,
	// or target= (the batch-imply input submits its value under its name).
	prefix := cmp.Or(q.Get("q"), q.Get("tag"), q.Get("canonical_id"), q.Get("target"))

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

	items := make([]suggestItem, len(suggestions))
	for i, t := range suggestions {
		items[i] = suggestItem{
			Name:       t.Name,
			Color:      t.CategoryColor,
			UsageCount: t.UsageCount,
			ShowCount:  true,
		}
	}
	s.renderSuggestList(w, `data-tag-name`, `onclick="applyTagSuggest(this)"`, items)
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
		// system: hijacks the suggest endpoint to surface the query
		// language itself - the keywords, operators, and closed-vocabulary
		// values - without the user leaving the search bar. "system" is
		// reserved at the category layer, so the categoryExists branch
		// below cannot reach this name.
		if rest, ok := strings.CutPrefix(last, "system:"); ok {
			s.renderSystemSuggest(w, rest)
			return
		}
		if colonIdx := strings.IndexByte(last, ':'); colonIdx >= 0 {
			key := strings.ToLower(last[:colonIdx])
			val := last[colonIdx+1:]
			// Filter keyword: surface the level-2 hint - operators for
			// date/width/height, closed-vocabulary values for
			// fav/source/rating/etc., live category names for cat: - so
			// the dropdown helps the user the same way `system:<key>:`
			// would. Avoids forcing the user to remember the cheat-sheet
			// trigger after they've already committed to the filter.
			if searchkw.IsKeyword(key) {
				// Most filter keys carry closed-vocabulary values that are
				// case-insensitive matches against a static enum (fav:true,
				// ai:comfyui, ...). The free-text keys are the exceptions:
				// labels are operator-entered free text whose case must
				// survive the prefix-range SQL, so pass them through
				// unchanged. They all accept the quoted form
				// (`source:"foo bar"`); strip a leading `"` so the dropdown
				// keeps matching while the user is mid-quote.
				vp := strings.ToLower(val)
				switch key {
				case "collection", "source", "name", "prompt", "model", "sampler":
					vp = strings.TrimPrefix(val, `"`)
				}
				rows := s.systemSuggestLevel2(key, vp)
				if len(rows) == 0 {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				s.renderSearchSuggest(w, rows)
				return
			}
			// Category-qualified only when the prefix actually names a
			// category; otherwise suggest literal tags that match the
			// whole "key:val" string (e.g. "nier:aut..." → "nier:automata").
			if colonIdx > 0 && s.categoryExists(key) {
				catFilter = key
				prefix = val
			} else {
				prefix = last
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

	rows := make([]suggestItem, len(suggestions))
	for i, t := range suggestions {
		rows[i] = suggestItem{
			Name:       t.Name,
			Color:      t.CategoryColor,
			UsageCount: t.UsageCount,
			ShowCount:  true,
		}
	}
	s.renderSearchSuggest(w, rows)
}

// renderSystemSuggest emits cheat-sheet rows for the search-bar's
// `system:` namespace. rest is what follows "system:" in the user's
// last word. Without an inner colon the level-1 list surfaces every
// real prefix (filter keywords plus existing tag categories); with an
// inner colon the per-keyword level-2 list takes over (static operators
// or values for filter keywords, live tags for category prefixes).
func (s *Server) renderSystemSuggest(w http.ResponseWriter, rest string) {
	var rows []suggestItem
	if colonIdx := strings.IndexByte(rest, ':'); colonIdx >= 0 {
		key := strings.ToLower(rest[:colonIdx])
		valPrefix := strings.ToLower(rest[colonIdx+1:])
		rows = s.systemSuggestLevel2(key, valPrefix)
	} else {
		rows = s.systemSuggestLevel1(strings.ToLower(rest))
	}
	if len(rows) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderSearchSuggest(w, rows)
}

// systemSuggestLevel1 lists every prefix the user can type to start a
// `key:value` search token: the search-filter keywords plus every
// existing tag category. A category whose name doubles as a filter
// keyword (rating: is both) is folded into the keyword row to avoid
// duplicate dropdown entries. Category rows carry their own colour so
// the dropdown reads at a glance like the rest of the tag UI.
func (s *Server) systemSuggestLevel1(prefix string) []suggestItem {
	var rows []suggestItem
	for _, kw := range searchkw.Keywords {
		if !strings.HasPrefix(kw, prefix) {
			continue
		}
		rows = append(rows, suggestItem{
			Name:        kw + ":",
			Description: searchkw.Descriptions[kw],
		})
	}
	for _, cat := range s.systemCategoryRows() {
		if searchkw.IsKeyword(cat.Name) {
			continue
		}
		if !strings.HasPrefix(cat.Name, prefix) {
			continue
		}
		rows = append(rows, suggestItem{
			Name:        cat.Name + ":",
			Color:       cat.Color,
			Description: "tag category",
		})
	}
	return rows
}

// expansionRows renders the static level-2 vocabulary a keyword declares
// in searchkw.Expansions, filtered by what the user has typed so far.
func expansionRows(key, valPrefix string) []suggestItem {
	descs := searchkw.ExpansionDescriptions[key]
	var rows []suggestItem
	for _, exp := range searchkw.Expansions[key] {
		if !strings.HasPrefix(exp, valPrefix) {
			continue
		}
		rows = append(rows, suggestItem{
			Name:        key + ":" + exp,
			Description: descs[exp],
		})
	}
	return rows
}

// quotedSDLabelRows wraps each label as `<key>:"<label>"` so multi-
// word model / sampler / prompt values stay one parser token.
func quotedSDLabelRows(key string, labels []string) []suggestItem {
	rows := make([]suggestItem, 0, len(labels))
	for _, lbl := range labels {
		rows = append(rows, suggestItem{
			Name: key + `:"` + search.QuoteValue(lbl) + `"`,
		})
	}
	return rows
}

func (s *Server) systemSuggestLevel2(key, valPrefix string) []suggestItem {
	if key == "cat" {
		var rows []suggestItem
		for _, cat := range s.systemCategoryRows() {
			if !strings.HasPrefix(cat.Name, valPrefix) {
				continue
			}
			rows = append(rows, suggestItem{
				Name:  "cat:" + cat.Name,
				Color: cat.Color,
			})
			if len(rows) >= 10 {
				break
			}
		}
		return rows
	}
	// collection labels may contain spaces; wrap each suggestion in
	// double quotes so the parser still treats it as a single token.
	if key == "collection" {
		return quotedSDLabelRows("collection", s.queryCollectionLabels(valPrefix, 10))
	}
	// Source labels are operator-edited free text that frequently contains
	// spaces; quote each suggestion so the parser still treats it as one
	// token. Mirrors the collection: branch above. The none / any
	// membership shortcuts lead, since no real label can reach them.
	if key == "source" {
		return append(expansionRows(key, valPrefix),
			quotedSDLabelRows("source", s.querySourceLabels(valPrefix, 10))...)
	}
	// name: surfaces distinct file basenames whose substring matches
	// the prefix, mirroring the executor's `canonical_path LIKE '%/<val>%'`
	// shape. Empty prefix returns nothing - on a million-image library a
	// blank scan would walk the whole table and overflow the suggest budget.
	// Filenames frequently contain spaces; wrap each suggestion in quotes
	// so the parser keeps the value as a single token, matching the
	// source: / collection: branches.
	if key == "name" {
		if valPrefix == "" {
			return nil
		}
		return quotedSDLabelRows("name", s.queryNameBasenames(valPrefix, 10))
	}
	// model: / sampler: are typically short low-cardinality identifiers
	// (e.g. `sdxl_v1.0`, `Euler a`). Surface distinct values from both
	// metadata tables that prefix-match the typed text. Quote the value
	// so multi-word sampler names like `Euler a` survive the parser.
	if key == "model" {
		return quotedSDLabelRows("model", s.querySDStringField("model", "model_checkpoint", valPrefix, 10, false))
	}
	if key == "sampler" {
		return quotedSDLabelRows("sampler", s.querySDStringField("sampler", "sampler", valPrefix, 10, false))
	}
	// prompt: stores free-text sentences, so substring-match the prefix
	// against existing prompts. Empty prefix returns nothing - listing
	// the alphabetically first prompts isn't useful, and a full-table
	// distinct on a sentence column is expensive. Prompts always contain
	// spaces; the quoted form is the only one the parser can ingest as
	// a single token.
	if key == "prompt" {
		if valPrefix == "" {
			return nil
		}
		return quotedSDLabelRows("prompt", s.querySDStringField("prompt", "prompt", valPrefix, 10, true))
	}
	if _, ok := searchkw.Expansions[key]; ok {
		return expansionRows(key, valPrefix)
	}
	// Filter keyword without a static expansion (folder, folderonly,
	// generated): no level-2 hint - the user types the value freeform.
	if searchkw.IsKeyword(key) {
		return nil
	}
	// Real category at level 2: list tags in that category, mirroring
	// the existing `<category>:<prefix>` autocomplete path. These rows
	// wear the category color and a usage count, not the dim "system"
	// label, since they're real data, not a static hint.
	if s.categoryExists(key) {
		suggestions, _ := search.SuggestTagsWithFilter(s.db(), nil, valPrefix, key, 10)
		rows := make([]suggestItem, 0, len(suggestions))
		for _, t := range suggestions {
			rows = append(rows, suggestItem{
				Name:       key + ":" + t.Name,
				Color:      t.CategoryColor,
				UsageCount: t.UsageCount,
				ShowCount:  true,
			})
		}
		return rows
	}
	return nil
}

// systemCategoryRow pairs a tag-category name with its colour so the
// system: dropdown can render each row in the category's accent.
type systemCategoryRow struct {
	Name  string
	Color string
}

// systemCategoryRows pulls the live category list once per request.
// tag_categories is small (~9 builtin plus a handful of user rows) so
// it's cheaper to read all and filter in Go than to run a LIKE per
// keystroke and worry about escaping underscored names. Color is the
// hex value from the categories table; unknown values fall back to
// the neutral default via tags.SafeCategoryColor.
func (s *Server) systemCategoryRows() []systemCategoryRow {
	d := s.db()
	if d == nil {
		return nil
	}
	out, err := db.QueryAll(d.Read, func(rows *sql.Rows) (systemCategoryRow, error) {
		var name, color string
		err := rows.Scan(&name, &color)
		return systemCategoryRow{Name: name, Color: tags.SafeCategoryColor(color)}, err
	}, `SELECT name, color FROM tag_categories ORDER BY name`)
	if err != nil {
		logx.Warnf("system category rows: %v", err)
	}
	return out
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
		// Shares searchkw.IsKeyword with searchSuggest's value-only check.
		if colonIdx := strings.IndexByte(t, ':'); colonIdx > 0 {
			if searchkw.IsKeyword(strings.ToLower(t[:colonIdx])) {
				continue
			}
		}
		set[t] = struct{}{}
	}
	return set
}
