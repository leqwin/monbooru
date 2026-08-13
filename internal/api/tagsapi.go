package api

import (
	"cmp"
	"net/http"

	"github.com/monbooru/monbooru/internal/tags"
)

type tagResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	Color      string `json:"color"`
	UsageCount int    `json:"usage_count"`
	IsAlias    bool   `json:"is_alias"`
	Origin     string `json:"origin"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

// listTags handles GET /api/v1/tags.
func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	prefix := q.Get("q")
	catName := q.Get("category")
	sortStr := q.Get("sort")
	sortStr = cmp.Or(sortStr, "usage")

	offset, limit := parsePage(r, 100, 500)

	filter := tags.TagFilter{
		Prefix:    prefix,
		Sort:      sortStr,
		PageIndex: offset / limit,
		Limit:     limit,
		Origin:    q.Get("origin"),
		Type:      q.Get("type"),
		// Tri-state with the /tags page: empty / anything but "0" → Show
		// (default so freshly-declared tags surface without a flag flip);
		// "0" → Hide. The UI also exposes "only" but the API has no use
		// for that triage view, so any non-"0" string folds into Show.
		ShowZero: q.Get("show_zero") != "0",
	}

	if catName != "" {
		catID, ok, err := categoryIDByName(g, catName)
		if serverError(w, err) {
			return
		}
		if ok {
			filter.CategoryID = &catID
		}
	}

	tagList, total, err := g.TagSvc.ListTags(filter)
	if serverError(w, err) {
		return
	}

	results := make([]tagResponse, 0, len(tagList))
	for _, t := range tagList {
		results = append(results, toTagResponse(&t))
	}

	writePage(w, offset/limit+1, limit, total, results)
}
