package api

import (
	"net/http"

	"github.com/monbooru/monbooru/internal/relations"
)

// relationsResponse mirrors relations.ImageRelations in JSON shape.
// Pointer fields are nil-friendly so the client can tell the
// difference between "no parent version" and "parent id 0".
type relationsResponse struct {
	DuplicateGroup   *dupGroupJSON `json:"duplicate_group"`
	AlternateGroup   *altGroupJSON `json:"alternate_group"`
	VersionParent    *int64        `json:"version_parent"`
	VersionChild     *int64        `json:"version_child"`
	DerivativeSource *int64        `json:"derivative_source"`
	Derivatives      []int64       `json:"derivatives"`
}

type dupGroupJSON struct {
	ID       int64   `json:"id"`
	Original int64   `json:"original_image_id"`
	Members  []int64 `json:"member_ids"`
}

type altGroupJSON struct {
	ID      int64   `json:"id"`
	Members []int64 `json:"member_ids"`
}

// relationsForImage serves GET /api/v1/images/{id}/relations.
func (h *Handler) relationsForImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	rels, err := relations.LoadImageRelations(g.DB, id)
	if serverError(w, err) {
		return
	}
	resp := relationsResponse{
		VersionParent:    rels.VersionParent,
		VersionChild:     rels.VersionChild,
		DerivativeSource: rels.DerivativeSource,
		Derivatives:      rels.Derivatives,
	}
	if rels.DupGroup != nil {
		resp.DuplicateGroup = &dupGroupJSON{
			ID:       rels.DupGroup.ID,
			Original: rels.DupGroup.Original,
			Members:  rels.DupGroup.Members,
		}
	}
	if rels.AltGroupID != nil {
		resp.AlternateGroup = &altGroupJSON{
			ID:      *rels.AltGroupID,
			Members: rels.AltGroupMembers,
		}
	}
	if resp.Derivatives == nil {
		resp.Derivatives = []int64{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// relationsAddBody is the JSON shape POST /api/v1/relations expects.
// Type, A, and B are required. Direction defaults to "ab" - A is the left
// side (original / source / newer / parent). Promotion to original is a
// DELETE-side action (type "promote_original"), not a field here.
type relationsAddBody struct {
	Type      string `json:"type"`
	A         int64  `json:"a"`
	B         int64  `json:"b"`
	Direction string `json:"direction"`
}

// addRelation serves POST /api/v1/relations.
func (h *Handler) addRelation(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body relationsAddBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.A == 0 || body.B == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "missing a or b")
		return
	}
	left, right := body.A, body.B
	if body.Direction == "ba" {
		left, right = right, left
	}
	if g.RelationsSvc == nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "relations not wired")
		return
	}
	var err error
	switch body.Type {
	case "duplicate":
		err = g.RelationsSvc.AddDuplicate(left, right)
	case "alternate":
		err = g.RelationsSvc.AddAlternate(left, right)
	case "version":
		// left is the parent (older revision), right the child.
		err = g.RelationsSvc.AddVersionEdge(left, right)
	case "derivative":
		// left is the source, right the derivative.
		err = g.RelationsSvc.AddDerivativeEdge(left, right)
	case "not_related":
		err = g.RelationsSvc.AddNotRelated(left, right)
	default:
		apiError(w, http.StatusBadRequest, "invalid_request", "unknown type")
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	g.invalidate()
	w.WriteHeader(http.StatusCreated)
}

// relationsRemoveBody is the JSON for DELETE /api/v1/relations.
type relationsRemoveBody struct {
	Type    string `json:"type"`
	A       int64  `json:"a"`
	B       int64  `json:"b"`
	ImageID int64  `json:"image_id"`
	GroupID int64  `json:"group_id"`
	RootID  int64  `json:"root_id"`
}

// removeRelation serves DELETE /api/v1/relations.
func (h *Handler) removeRelation(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body relationsRemoveBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if g.RelationsSvc == nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "relations not wired")
		return
	}
	var err error
	switch body.Type {
	case "duplicate":
		if !requireID(w, body.ImageID, "image_id") {
			return
		}
		err = g.RelationsSvc.RemoveDupMember(body.ImageID)
	case "alternate":
		if !requireID(w, body.ImageID, "image_id") {
			return
		}
		err = g.RelationsSvc.RemoveAltMember(body.ImageID)
	case "version":
		err = g.RelationsSvc.RemoveVersionEdge(body.A, body.B)
	case "derivative":
		err = g.RelationsSvc.RemoveDerivativeEdge(body.A, body.B)
	case "not_related":
		err = g.RelationsSvc.RemoveNotRelated(body.A, body.B)
	case "dissolve_dup":
		if !requireID(w, body.GroupID, "group_id") {
			return
		}
		err = g.RelationsSvc.DissolveDupGroup(body.GroupID)
	case "dissolve_alt":
		if !requireID(w, body.GroupID, "group_id") {
			return
		}
		err = g.RelationsSvc.DissolveAltGroup(body.GroupID)
	case "dissolve_version":
		if !requireID(w, body.RootID, "root_id") {
			return
		}
		err = g.RelationsSvc.DissolveVersionChain(body.RootID)
	case "dissolve_derivative":
		if !requireID(w, body.RootID, "root_id") {
			return
		}
		err = g.RelationsSvc.DissolveDerivativeTree(body.RootID)
	case "promote_original":
		if !requireID(w, body.GroupID, "group_id") || !requireID(w, body.ImageID, "image_id") {
			return
		}
		err = g.RelationsSvc.PromoteToOriginal(body.GroupID, body.ImageID)
	default:
		apiError(w, http.StatusBadRequest, "invalid_request", "unknown type")
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	g.invalidate()
	w.WriteHeader(http.StatusNoContent)
}

// writeRelationError surfaces relations.Service errors as API errors,
// using the shared FriendlyErrorFor mapping for typed sentinels and
// falling back to a generic 500 for anything else.
func writeRelationError(w http.ResponseWriter, err error) {
	if fe := relations.FriendlyErrorFor(err); fe != nil {
		apiError(w, fe.Status, fe.Code, fe.Message)
		return
	}
	apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
}
