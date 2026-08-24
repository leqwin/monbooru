package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

type tagDetailData struct {
	baseData
	Tag        *models.Tag
	Categories []models.TagCategory
	IsRating   bool
	// Canonical is the resolve target on alias rows; nil otherwise.
	Canonical *models.Tag
	// Aliases are the rows pointing at this tag (fan-in).
	Aliases          []models.Tag
	Implications     []models.Implication
	ImpliedBy        []models.Implication
	RecentImageIDs   []int64
	OriginKinds      map[string]string
	MonloaderContrib bool
	// BackURL points the crumb at the listing view the visitor came
	// from; PrevURL/NextURL step through that view's tag order. All
	// empty on a direct visit with no `back` context.
	BackURL string
	PrevURL string
	NextURL string
}

func (s *Server) tagDetailHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tag, err := s.tagSvc().GetTag(id)
	if errors.Is(err, tags.ErrTagNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cats, _ := s.tagSvc().ListCategories()

	data := tagDetailData{
		baseData:         s.base(r, "tags", tag.Name+" - "+s.booruName()),
		Tag:              tag,
		Categories:       cats,
		IsRating:         tag.CategoryName == "rating",
		MonloaderContrib: s.contribGateOpen(),
	}
	if backRaw := r.URL.Query().Get("back"); backRaw != "" {
		if bq, err := url.ParseQuery(backRaw); err == nil {
			p := tagListingParamsFrom(bq)
			enc := p.backQS()
			data.BackURL = "/tags?" + enc
			prevID, nextID, err := s.tagSvc().AdjacentTags(s.tagListingFilter(p), id)
			if err != nil {
				logx.Warnf("adjacent tags: %v", err)
			}
			if prevID != nil {
				data.PrevURL = fmt.Sprintf("/tags/%d?back=%s", *prevID, url.QueryEscape(enc))
			}
			if nextID != nil {
				data.NextURL = fmt.Sprintf("/tags/%d?back=%s", *nextID, url.QueryEscape(enc))
			}
		}
	}
	labels := []string{tag.Origin}

	if tag.IsAlias {
		if tag.CanonicalTagID != nil {
			if canon, err := s.tagSvc().GetTag(*tag.CanonicalTagID); err == nil {
				data.Canonical = canon
				labels = append(labels, canon.Origin)
			}
		}
	} else {
		aliases, err := s.tagSvc().AliasesForTagIDs([]int64{id})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Aliases = aliases[id]
		if data.Implications, err = s.tagSvc().ListImplications(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if data.ImpliedBy, err = s.tagSvc().ImpliedBy(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Newest carriers by image id: ids are allocation-ordered, so the
		// id ordering streams straight off idx_image_tags_tag_image where a
		// created_at order would temp-sort a popular tag's whole row set.
		// CROSS JOIN pins image_tags as the outer table; otherwise the
		// planner drives from images on is_missing and temp-sorts anyway.
		recentQ := `SELECT it.image_id FROM image_tags it INDEXED BY idx_image_tags_tag_image
			 CROSS JOIN images i ON i.id = it.image_id
			 WHERE it.tag_id = ? AND i.is_missing = 0`
		recentArgs := []any{id}
		if where, wargs := resolveCeiling(r, s.Active()).WhereOne("i.id"); where != "" {
			recentQ += ` AND ` + where
			recentArgs = append(recentArgs, wargs...)
		}
		data.RecentImageIDs, err = db.QueryIDs(s.db().Read, recentQ+` ORDER BY it.image_id DESC LIMIT 7`, recentArgs...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range data.Aliases {
			labels = append(labels, a.Origin)
		}
		for _, im := range data.Implications {
			labels = append(labels, im.Origin)
		}
		for _, im := range data.ImpliedBy {
			labels = append(labels, im.Origin)
		}
	}
	data.OriginKinds = s.originKinds(labels)
	s.renderTemplate(w, "tags_detail.html", data)
}

// relationGroupFilter reads the origin subgroup a detail-page [×] button
// names: the exact provenance label (empty selects the unrecorded-source
// group) and whether it is that label's stale half.
func relationGroupFilter(r *http.Request) (origin string, stale bool) {
	q := r.URL.Query()
	return q.Get("origin"), q.Get("stale") == "1"
}

// usageBar is one row of the detail page's usage histogram, with the
// bar pre-rendered server-side as block characters (no chart library).
type usageBar struct {
	Month string
	Count int
	Bar   string
}

const usageBarMaxWidth = 24

// tagUsagePanelHandler renders the detail page's applied-by table and
// usage histogram as one lazy fragment. Both views aggregate every
// image_tags row the tag carries - seconds on a monster tag - so they
// load after first paint and share a single pass (UsageBreakdown)
// instead of blocking the page render.
func (s *Server) tagUsagePanelHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	// Fetched as a fragment by the tag detail page; a non-htmx caller
	// (refresh, bookmark, shared link) gets the detail page rather than
	// a chrome-less fragment.
	if !isHTMXRequest(r) {
		http.Redirect(w, r, fmt.Sprintf("/tags/%d", id), http.StatusSeeOther)
		return
	}
	applied, months, err := s.tagSvc().UsageBreakdown(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Keep the panel bounded on very old tags: the last 24 months tell
	// the "is this vocabulary alive" story the graph exists for.
	if len(months) > 24 {
		months = months[len(months)-24:]
	}
	maxCount := 0
	for _, m := range months {
		if m.Count > maxCount {
			maxCount = m.Count
		}
	}
	bars := make([]usageBar, 0, len(months))
	for _, m := range months {
		width := 0
		if maxCount > 0 {
			width = m.Count * usageBarMaxWidth / maxCount
		}
		if width == 0 && m.Count > 0 {
			width = 1
		}
		bars = append(bars, usageBar{Month: m.Month, Count: m.Count, Bar: strings.Repeat("█", width)})
	}
	labels := make([]string, 0, len(applied))
	for _, a := range applied {
		labels = append(labels, a.Label)
	}
	s.renderTemplate(w, "partials/tag_usage_panel.html", map[string]any{
		"AppliedBy":   applied,
		"Bars":        bars,
		"OriginKinds": s.originKinds(labels),
	})
}
