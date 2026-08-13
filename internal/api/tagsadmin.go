package api

import (
	"cmp"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// toTagResponse projects a fully-joined models.Tag (the shape GetTag /
// CreateAlias return, with category name + colour populated) into the
// API's TagRow.
func toTagResponse(t *models.Tag) tagResponse {
	resp := tagResponse{
		ID:         t.ID,
		Name:       t.Name,
		Category:   t.CategoryName,
		Color:      t.CategoryColor,
		UsageCount: t.UsageCount,
		IsAlias:    t.IsAlias,
		Origin:     t.Origin,
	}
	if !t.LastUsedAt.IsZero() {
		resp.LastUsedAt = t.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// resolveCategoryID maps a category name to its id; an empty name
// resolves to the built-in general category. The bool reports whether
// the name named a real category.
func resolveCategoryID(g Gallery, name string) (int64, bool, error) {
	name = strings.TrimSpace(name)
	name = cmp.Or(name, "general")
	return categoryIDByName(g, name)
}

// categoryIDByName looks up a tag category id by exact name, with no
// empty-name default; resolveCategoryID layers the "general" fallback.
// A read failure is reported separately from a name that matched nothing:
// answering "unknown category" to a broken read would blame the caller for
// the server's fault.
func categoryIDByName(g Gallery, name string) (int64, bool, error) {
	var id int64
	err := g.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// sentinelStatus maps one service sentinel to its API status/code pair.
type sentinelStatus struct {
	err    error
	status int
	code   string
}

// writeSentinelError walks the table and writes the first errors.Is match,
// reporting whether one hit; the caller keeps its bespoke fallback.
func writeSentinelError(w http.ResponseWriter, err error, table []sentinelStatus) bool {
	for _, e := range table {
		if errors.Is(err, e.err) {
			apiError(w, e.status, e.code, err.Error())
			return true
		}
	}
	return false
}

// writeTagError maps tags-service errors to API status codes. Typed
// sentinels resolve precisely; the remaining plain-text errors the
// rename / alias / merge paths return are matched by phrase (the
// service has no sentinel for them) so a collision reads as 409, a
// missing target as 404, and a self-reference as 400 instead of a bare
// 500.
func writeTagError(w http.ResponseWriter, err error) {
	if writeSentinelError(w, err, []sentinelStatus{
		{tags.ErrTagNotFound, http.StatusNotFound, "not_found"},
		{tags.ErrCategoryNotFound, http.StatusBadRequest, "invalid_request"},
		{tags.ErrAliasNameInUse, http.StatusConflict, "conflict"},
		{tags.ErrImplicationCycle, http.StatusConflict, "conflict"},
		{tags.ErrInvalidTagName, http.StatusBadRequest, "invalid_request"},
		{tags.ErrNonCanonicalRating, http.StatusBadRequest, "invalid_request"},
		{tags.ErrRatingTagImmutable, http.StatusBadRequest, "invalid_request"},
	}) {
		return
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already exists"):
		apiError(w, http.StatusConflict, "conflict", msg)
	case strings.Contains(msg, "not found"):
		apiError(w, http.StatusNotFound, "not_found", msg)
	case strings.Contains(msg, "itself"), strings.Contains(msg, "alias"):
		apiError(w, http.StatusBadRequest, "invalid_request", msg)
	default:
		apiError(w, http.StatusInternalServerError, "internal_error", msg)
	}
}

// createTag handles POST /api/v1/tags. Get-or-create against (name,
// category); category defaults to general. Returns the tag either way -
// a name already present in the category resolves to the existing row.
func (h *Handler) createTag(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	catID, found, err := resolveCategoryID(g, body.Category)
	if serverError(w, err) {
		return
	}
	if !found {
		apiError(w, http.StatusBadRequest, "invalid_request", "unknown category: "+body.Category)
		return
	}
	tag, err := g.TagSvc.GetOrCreateTagFrom(body.Name, catID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	full, err := g.TagSvc.GetTag(tag.ID)
	if serverError(w, err) {
		return
	}
	g.invalidate()
	writeJSON(w, http.StatusCreated, toTagResponse(full))
}

// patchTag handles PATCH /api/v1/tags/{id}: rename and/or move to
// another category. Both edits target the same row; rename is applied
// first. At least one of name / category is required.
func (h *Handler) patchTag(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name     *string `json:"name"`
		Category *string `json:"category"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == nil && body.Category == nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "name or category is required")
		return
	}
	if body.Name != nil {
		if err := g.TagSvc.RenameTag(id, *body.Name); err != nil {
			writeTagError(w, err)
			return
		}
	}
	if body.Category != nil {
		catID, found, cerr := resolveCategoryID(g, *body.Category)
		if serverError(w, cerr) {
			return
		}
		if !found {
			apiError(w, http.StatusBadRequest, "invalid_request", "unknown category: "+*body.Category)
			return
		}
		if err := g.TagSvc.ChangeTagCategory(id, catID); err != nil {
			writeTagError(w, err)
			return
		}
	}
	full, err := g.TagSvc.GetTag(id)
	if err != nil {
		writeTagError(w, err)
		return
	}
	g.invalidate()
	writeJSON(w, http.StatusOK, toTagResponse(full))
}

// deleteTag handles DELETE /api/v1/tags/{id}. Rating-category rows are
// usage-stripped rather than removed (the catalog row stays), matching
// the web behaviour; the response is 204 either way.
func (h *Handler) deleteTag(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if err := g.TagSvc.DeleteTag(id); err != nil {
		writeTagError(w, err)
		return
	}
	g.invalidate()
	w.WriteHeader(http.StatusNoContent)
}

// createAlias handles POST /api/v1/tags/aliases: declare that name (in
// category, default general) resolves to canonical_id. Returns the
// alias row.
func (h *Handler) createAlias(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Category    string `json:"category"`
		CanonicalID int64  `json:"canonical_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	if body.CanonicalID == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "canonical_id is required")
		return
	}
	catID, found, err := resolveCategoryID(g, body.Category)
	if serverError(w, err) {
		return
	}
	if !found {
		apiError(w, http.StatusBadRequest, "invalid_request", "unknown category: "+body.Category)
		return
	}
	alias, err := g.TagSvc.CreateAliasFrom(body.Name, catID, body.CanonicalID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	g.invalidate()
	writeJSON(w, http.StatusCreated, toTagResponse(alias))
}

// mergeTags handles POST /api/v1/tags/merge: make alias_id an alias of
// canonical_id, moving its image_tags onto the canonical. Returns the
// canonical tag.
func (h *Handler) mergeTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		AliasID     int64 `json:"alias_id"`
		CanonicalID int64 `json:"canonical_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.AliasID == 0 || body.CanonicalID == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "alias_id and canonical_id are required")
		return
	}
	if err := g.TagSvc.MergeTags(body.AliasID, body.CanonicalID); err != nil {
		writeTagError(w, err)
		return
	}
	canon, err := g.TagSvc.GetTag(body.CanonicalID)
	if err != nil {
		writeTagError(w, err)
		return
	}
	g.invalidate()
	writeJSON(w, http.StatusOK, toTagResponse(canon))
}

type implicationJSON struct {
	ParentID        int64  `json:"parent_id"`
	ImpliedID       int64  `json:"implied_id"`
	ImpliedName     string `json:"implied_name"`
	ImpliedCategory string `json:"implied_category"`
}

// listImplications handles GET /api/v1/tags/{id}/implications: the
// direct edges declared from this parent.
func (h *Handler) listImplications(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if _, err := g.TagSvc.GetTag(id); err != nil {
		writeTagError(w, err)
		return
	}
	imps, err := g.TagSvc.ListImplications(id)
	if serverError(w, err) {
		return
	}
	out := make([]implicationJSON, 0, len(imps))
	for _, im := range imps {
		out = append(out, implicationJSON{
			ParentID:        im.ParentID,
			ImpliedID:       im.ImpliedID,
			ImpliedName:     im.ImpliedName,
			ImpliedCategory: im.ImpliedCategoryName,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// addImplication handles POST /api/v1/tags/{id}/implications. Body
// carries an existing tag id as implied_id; both sides must be
// canonical (non-alias) tags and the edge must not close a cycle.
// Declaring the edge is synchronous and immediately governs future tag
// adds; the historical fan-out across images already carrying the
// parent (the web's background propagation job, scoped to the active
// gallery) is not run from the API.
func (h *Handler) addImplication(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	parentID, ok := apiPathInt64(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		ImpliedID int64 `json:"implied_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.ImpliedID == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "implied_id is required")
		return
	}
	isNew, err := g.TagSvc.AddImplicationFrom(parentID, body.ImpliedID, "api")
	if err != nil {
		writeTagError(w, err)
		return
	}
	if isNew {
		w.WriteHeader(http.StatusCreated)
		return
	}
	// Edge already declared - idempotent no-op.
	w.WriteHeader(http.StatusOK)
}

// removeImplication handles DELETE /api/v1/tags/{id}/implications/{impliedID}.
// Drops the edge only; the image-side sweep of rows implied solely by
// this edge is the web's background job and is not run from the API.
func (h *Handler) removeImplication(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	parentID, ok := apiPathInt64(w, r, "id")
	if !ok {
		return
	}
	impliedID, ok := apiPathInt64(w, r, "impliedID")
	if !ok {
		return
	}
	if err := g.TagSvc.RemoveImplication(parentID, impliedID); err != nil {
		writeTagError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
