package api

import (
	"net/http"

	"github.com/leqwin/monbooru/internal/tags"
)

type tagResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	Color      string `json:"color"`
	UsageCount int    `json:"usage_count"`
	IsAlias    bool   `json:"is_alias"`
}

// listTags handles GET /api/v1/tags.
func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("q")
	catName := q.Get("category")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "usage"
	}

	offset, limit := parsePage(r, 100, 500)

	filter := tags.TagFilter{
		Prefix:    prefix,
		Sort:      sortStr,
		PageIndex: offset / limit,
		Limit:     limit,
	}

	if catName != "" {
		var catID int64
		if err := h.db.Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, catName,
		).Scan(&catID); err == nil {
			filter.CategoryID = &catID
		}
	}

	tagList, total, err := h.tagSvc.ListTags(filter)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	results := make([]tagResponse, 0, len(tagList))
	for _, t := range tagList {
		results = append(results, tagResponse{
			ID:         t.ID,
			Name:       t.Name,
			Category:   t.CategoryName,
			Color:      t.CategoryColor,
			UsageCount: t.UsageCount,
			IsAlias:    t.IsAlias,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"page":    offset/limit + 1,
		"limit":   limit,
		"total":   total,
		"results": results,
	})
}
