package web

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
)

// relatedTile is the small struct the relations partial reads from.
// Each tile carries the image's id, thumbnail URL, and a single-letter
// type marker. Collection siblings render in their own section below the
// panel, not as tiles here.
type relatedTile struct {
	ID     int64
	Marker string // O, D, A, V<-, V->, S, >
	Label  string // hover-title and badge text
}

// relatedEntriesGet renders the lazy-loaded "Related entries" panel
// sitting below "Similar entries" on the detail page. Returns an empty
// 204-equivalent fragment when the image carries no declared relations
// so the caller's HTMX swap drops the empty section entirely.
func (s *Server) relatedEntriesGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	rels, err := relations.LoadImageRelations(cx.DB, id)
	if err != nil {
		logx.Warnf("related entries load %d: %v", id, err)
		http.Error(w, "load relations", http.StatusInternalServerError)
		return
	}
	siblings, sErr := loadCollectionSiblings(cx, id)
	if sErr != nil {
		logx.Warnf("collection siblings load %d: %v", id, sErr)
	}
	if len(siblings) > relatedPanelCap {
		siblings = siblings[:relatedPanelCap]
	}
	tiles := flattenRelationsForPanel(rels, id)
	paths := loadImagePaths(r.Context(), cx.DB, id)
	back := parseBackContext(r)
	s.renderTemplate(w, "partials/related_entries.html", map[string]any{
		"ImageID":       id,
		"Tiles":         tiles,
		"Collection":    siblings,
		"ImagePaths":    paths,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"ActiveGallery": s.activeName,
		"BackQ":         back.Q,
		"BackSort":      back.Sort,
		"BackOrder":     back.Order,
		"BackSeed":      back.Seed,
		"BackPage":      back.Page,
	})
}

// relatedPanelCap bounds both the relation tiles and the collection
// siblings shown in the compact detail panel; the full set lives on the
// /images/{id}/relations page reached via [See all].
const relatedPanelCap = 6

// collectionSibling carries the bits the related-entries panel and the
// see-all grid need to render a card for an image that shares a
// collection with the current one. Series is the shared collection's
// name; Order is the sibling's position within it.
type collectionSibling struct {
	ID     int64
	Series string
	Order  *int64
}

// loadCollectionSiblings returns every non-missing image that shares any
// collection with imageID, grouped by collection name then position. Each
// row names the shared collection so a card can label which one it joins.
// The result excludes the image itself.
func loadCollectionSiblings(cx *galleryCtx, imageID int64) ([]collectionSibling, error) {
	rows, err := cx.DB.Read.Query(
		`SELECT jc.image_id, jc.name, jc.position
		 FROM image_collections self
		 JOIN image_collections jc ON jc.name = self.name
		 JOIN images i ON i.id = jc.image_id
		 WHERE self.image_id = ? AND jc.image_id != ? AND i.is_missing = 0
		 ORDER BY jc.name, jc.position IS NULL, jc.position, jc.image_id
		 LIMIT 200`,
		imageID, imageID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []collectionSibling
	for rows.Next() {
		var sib collectionSibling
		var ord sql.NullInt64
		if scanErr := rows.Scan(&sib.ID, &sib.Series, &ord); scanErr != nil {
			return nil, scanErr
		}
		if ord.Valid {
			v := ord.Int64
			sib.Order = &v
		}
		out = append(out, sib)
	}
	return out, rows.Err()
}

// flattenRelationsForPanel produces an ordered tile list (max
// relatedPanelCap) of declared relations for the compact panel:
// dup-group, alt-group, version neighbours, derivative neighbours, in
// declaration order. Collection siblings render in their own section.
func flattenRelationsForPanel(rels *relations.ImageRelations, self int64) []relatedTile {
	var tiles []relatedTile
	seen := map[int64]bool{self: true}
	add := func(t relatedTile) {
		if len(tiles) >= relatedPanelCap || seen[t.ID] {
			return
		}
		seen[t.ID] = true
		tiles = append(tiles, t)
	}
	if rels.DupGroup != nil {
		for _, m := range rels.DupGroup.Members {
			if m == self {
				continue
			}
			marker := "Duplicate"
			label := "duplicate"
			if m == rels.DupGroup.Original {
				marker = "Original"
				label = "original"
			}
			add(relatedTile{ID: m, Marker: marker, Label: label})
		}
	}
	for _, m := range rels.AltGroupMembers {
		add(relatedTile{ID: m, Marker: "Alternate", Label: "alternate"})
	}
	if rels.VersionParent != nil {
		add(relatedTile{ID: *rels.VersionParent, Marker: "Earlier", Label: "previous version"})
	}
	if rels.VersionChild != nil {
		add(relatedTile{ID: *rels.VersionChild, Marker: "Newer", Label: "newer version"})
	}
	if rels.DerivativeSource != nil {
		add(relatedTile{ID: *rels.DerivativeSource, Marker: "Source", Label: "source"})
	}
	for _, m := range rels.Derivatives {
		add(relatedTile{ID: m, Marker: "Derivative", Label: "derivative"})
	}
	return tiles
}

// imageRelationsPage renders the full-page /images/{id}/relations
// grid analogue of /images/{id}/pages: per-type sections, each a
// thumbnail strip the operator can click through.
func (s *Server) imageRelationsPage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	img, err := loadImage(r.Context(), cx.DB, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rels, err := relations.LoadImageRelations(cx.DB, id)
	if err != nil {
		logx.Warnf("relations page load %d: %v", id, err)
		http.Error(w, "load relations", http.StatusInternalServerError)
		return
	}
	siblings, sErr := loadCollectionSiblings(cx, id)
	if sErr != nil {
		logx.Warnf("relations page collection siblings %d: %v", id, sErr)
	}
	// Same Collection renders every sibling regardless of whether it
	// already appears in a stronger declared-relation section above.
	// Dropping shadowed cards leaves an obvious gap in the operator-
	// set series_order numbering (Test Series #1, #3 with #2 missing);
	// rendering the card twice with its real position is honest about
	// the collection's true membership, and the strong-relation cell
	// above carries the actual decision surface for that pair.
	// Splice the current image into the collection so the operator sees
	// the same "here is this image among its siblings" framing every
	// other section uses. Only when at least one sibling remains; a
	// collection-of-one wouldn't surface here in the first place.
	collection := collectionWithSelf(siblings, *img)
	// When the operator is about to unlink the current original from a
	// group with 3+ members, the post-step promotes a new original.
	// Surface that id so the confirm prompt can name it. Computed in
	// two cases: the operator is looking at another member's page
	// (so the original's cell carries the unlink) and the operator
	// is on the original's own page (so the self cell can offer the
	// same removal symmetrically).
	var nextOriginal int64
	if rels.DupGroup != nil && len(rels.DupGroup.Members) >= 3 {
		target := rels.DupGroup.Original
		if rels.DupGroup.Original == id {
			target = id
		}
		next, nErr := cx.RelationsSvc.NextOriginalIfRemoved(rels.DupGroup.ID, target)
		if nErr != nil {
			logx.Warnf("relations page next-original %d: %v", id, nErr)
		}
		nextOriginal = next
	}
	thumbURL := fmt.Sprintf("/thumbnails/%s/%d.jpg", s.activeName, id)
	// Mirror the detail page's parent/basename title shape so a tab
	// strip with several /relations tabs open stays distinguishable.
	titleName := filepath.Base(img.CanonicalPath)
	if parent := filepath.Base(filepath.Dir(img.CanonicalPath)); parent != "" && parent != "." && parent != "/" {
		titleName = parent + "/" + titleName
	}
	back := parseBackContext(r)
	pageData := relationsImagePageData{
		baseData:                       s.base(r, "gallery", fmt.Sprintf("Relations - %s - %s", titleName, s.booruName())),
		Image:                          *img,
		Relations:                      rels,
		Self:                           id,
		Collection:                     collection,
		ThumbnailURL:                   thumbURL,
		NextOriginalIfOriginalUnlinked: nextOriginal,
		AltGroupMembersOrdered:         reorderSelfFirst(rels.AltGroupMembers, id),
		BackQ:                          back.Q,
		BackSort:                       back.Sort,
		BackOrder:                      back.Order,
		BackSeed:                       back.Seed,
		BackPage:                       back.Page,
	}
	if rels.DupGroup != nil {
		pageData.DupGroupMembersOrdered = reorderSelfFirst(rels.DupGroup.Members, id)
	}
	if rels.VersionParent != nil || rels.VersionChild != nil {
		gens, vErr := versionChainGensForImage(cx, id)
		if vErr != nil {
			logx.Warnf("relations page version chain %d: %v", id, vErr)
		}
		pageData.VersionChainGens = gens
		pageData.VersionActions = versionActionMap(id, rels)
	}
	if rels.DerivativeSource != nil || len(rels.Derivatives) > 0 {
		treeRows, tErr := derivativeTreeRowsForImage(cx, id)
		if tErr != nil {
			logx.Warnf("relations page derivative tree %d: %v", id, tErr)
		}
		pageData.DerivativeTreeRows = treeRows
		pageData.DerivativeActions = derivativeActionMap(id, rels)
	}
	s.renderTemplate(w, "relations_image.html", pageData)
}

// reorderSelfFirst returns members with self at index 0 (if present),
// then the rest in their original order. Used so dup/alt sections on
// /images/{id}/relations render the current image at the leftmost
// cell - the same lead-with-self framing the version chain and
// derivative tree already apply via the relations-tree-current accent.
func reorderSelfFirst(members []int64, self int64) []int64 {
	if len(members) == 0 {
		return members
	}
	out := make([]int64, 0, len(members))
	hasSelf := false
	for _, m := range members {
		if m == self {
			hasSelf = true
		} else {
			out = append(out, m)
		}
	}
	if hasSelf {
		return append([]int64{self}, out...)
	}
	return out
}

// relationRoot is the top of the parentCol -> childCol chain above start,
// read off the gallery's pool.
func relationRoot(cx *galleryCtx, table, parentCol, childCol string, start int64) (int64, error) {
	path, err := relations.ChainPath(cx.DB.Read, table, parentCol, childCol, start)
	if err != nil {
		return 0, err
	}
	if len(path) == 0 {
		return start, nil
	}
	return path[len(path)-1], nil
}

// versionChainGensForImage walks the version chain that contains
// imageID and returns the BFS generations (one image per generation
// because the chain is strictly linear). Walks up via child_image_id
// to find the root, then down via parent_image_id to enumerate every
// descendant. Capped at MaxVersionChainDepth steps in each direction
// so a corrupt cycle can't loop indefinitely.
func versionChainGensForImage(cx *galleryCtx, imageID int64) ([][]int64, error) {
	root, err := relationRoot(cx, "version_edges", "parent_image_id", "child_image_id", imageID)
	if err != nil {
		return nil, err
	}
	members := []int64{root}
	cur := root
	for i := 0; i < relations.MaxVersionChainDepth; i++ {
		var c int64
		err := cx.DB.Read.QueryRow(`SELECT child_image_id FROM version_edges WHERE parent_image_id = ?`, cur).Scan(&c)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		members = append(members, c)
		cur = c
	}
	if len(members) <= 1 {
		return nil, nil
	}
	gens := make([][]int64, len(members))
	for i, m := range members {
		gens[i] = []int64{m}
	}
	return gens, nil
}

// derivativeTreeRowsForImage walks up from imageID via the derivative
// source link to the tree root and DFSes down, returning each tree
// node tagged with its depth and the trunk segments the template
// renders as CSS-drawn branch lines. Same depth budget as the version
// chain walk for safety.
func derivativeTreeRowsForImage(cx *galleryCtx, imageID int64) ([]treeRow, error) {
	root, err := relationRoot(cx, "derivative_edges", "source_image_id", "derivative_image_id", imageID)
	if err != nil {
		return nil, err
	}
	rows := []treeRow{{ID: root, Depth: 0}}
	if err := dfsDerivativeChildren(cx, root, 1, nil, &rows); err != nil {
		return nil, err
	}
	if len(rows) <= 1 {
		return nil, nil
	}
	return rows, nil
}

// dfsDerivativeChildren appends each derivative of `parent` (and the
// subtree below each) to rows in DFS order. ancestorTrunks carries
// the line/empty pattern from the root toward `parent`; the function
// appends a connector (tee or elbow) per child so the template can
// paint each row's branch glyph. Capped at the chain depth constant
// so a malformed graph can't recurse forever.
func dfsDerivativeChildren(cx *galleryCtx, parent int64, depth int, ancestorTrunks []string, rows *[]treeRow) error {
	if depth > relations.MaxVersionChainDepth {
		return nil
	}
	ids, err := db.QueryIDs(cx.DB.Read,
		`SELECT derivative_image_id FROM derivative_edges WHERE source_image_id = ? ORDER BY derivative_image_id`,
		parent,
	)
	if err != nil {
		return err
	}
	for i, id := range ids {
		isLast := i == len(ids)-1
		*rows = append(*rows, treeRow{ID: id, Depth: depth, Trunks: rowTrunks(ancestorTrunks, depth, isLast), Source: parent})
		childAncestors := extendAncestorTrunks(ancestorTrunks, depth, isLast)
		if err := dfsDerivativeChildren(cx, id, depth+1, childAncestors, rows); err != nil {
			return err
		}
	}
	return nil
}

// collectionWithSelf splices the anchor image into the sibling list at
// its sorted position so the rendered strip surfaces "this image among
// its peers" - the same lead-with-self framing the dup, alt, version
// chain, and derivative tree sections already apply via
// .relations-tree-current. Sort key matches loadCollectionSiblings's
// SQL: series_order IS NULL last, then series_order ascending, then id.
// Returns the input unchanged when there are no siblings - a single-
// member collection isn't worth a section.
func collectionWithSelf(siblings []collectionSibling, self models.Image) []collectionSibling {
	if len(siblings) == 0 {
		return siblings
	}
	var order *int64
	if self.SeriesOrder != nil {
		v := int64(*self.SeriesOrder)
		order = &v
	}
	merged := make([]collectionSibling, 0, len(siblings)+1)
	merged = append(merged, siblings...)
	merged = append(merged, collectionSibling{ID: self.ID, Series: self.Series, Order: order})
	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if !strings.EqualFold(a.Series, b.Series) {
			return strings.ToLower(a.Series) < strings.ToLower(b.Series)
		}
		aNull, bNull := a.Order == nil, b.Order == nil
		if aNull != bNull {
			return !aNull
		}
		if !aNull && !bNull && *a.Order != *b.Order {
			return *a.Order < *b.Order
		}
		return a.ID < b.ID
	})
	return merged
}

type relationsImagePageData struct {
	baseData
	Image        models.Image
	Relations    *relations.ImageRelations
	Self         int64
	Collection   []collectionSibling
	ThumbnailURL string
	// NextOriginalIfOriginalUnlinked is the image id that would be
	// promoted to original if the current group's original tile is
	// unlinked. Non-zero only when the group has 3+ members.
	NextOriginalIfOriginalUnlinked int64
	// DupGroupMembersOrdered and AltGroupMembersOrdered carry the
	// group's member ids with Self pushed to position 0 so the per-image
	// page renders the current image at the leftmost cell, matching the
	// way the version chain and derivative tree highlight self.
	DupGroupMembersOrdered []int64
	AltGroupMembersOrdered []int64
	// VersionChainGens groups the chain containing the current image
	// one-image-per-generation, root first. Nil when the image is not
	// in any version chain.
	VersionChainGens [][]int64
	// DerivativeTreeRows flattens the derivative tree containing the
	// current image in DFS order, each row tagged with its depth so
	// the template can indent children under their parent and the
	// branching is visible. Nil when the image has no derivative
	// edges.
	DerivativeTreeRows []treeRow
	// DerivativeActions maps a tree node id to the inline-action label
	// the template should paint next to its thumb: "this" for the
	// current image, "source" for the current image's declared source,
	// "derivative" for each direct derivative of the current image.
	// Tree nodes that are neither (ancestors past the source, siblings,
	// or descendants past the direct derivatives) are absent so the
	// template's `index` lookup yields the empty string and no inline
	// action renders.
	DerivativeActions map[int64]string
	// VersionActions maps a chain node id to "this" / "earlier" / "newer"
	// so the template can drop the right per-thumb button under the
	// current image and its two neighbours; chain nodes further away
	// (non-adjacent) carry no inline button.
	VersionActions map[int64]string
	BackQ          string
	BackSort       string
	BackOrder      string
	BackSeed       string
	BackPage       string
}

// derivativeActionMap returns the per-row inline-action label for the
// derivative-section of /images/{id}/relations: "this" for the current
// image, "source" for its declared source, "derivative" for each direct
// derivative. Tree nodes the operator can't act on from the current
// image's vantage are absent from the map.
func derivativeActionMap(self int64, rels *relations.ImageRelations) map[int64]string {
	m := map[int64]string{self: "this"}
	if rels.DerivativeSource != nil {
		m[*rels.DerivativeSource] = "source"
	}
	for _, d := range rels.Derivatives {
		m[d] = "derivative"
	}
	return m
}

// versionActionMap returns the per-thumb inline-action label for the
// version-chain section: "this" for the current image, "earlier" for
// its immediate parent (the edge whose [unlink earlier revision] now
// sits under that thumb), "newer" for its immediate child. Chain
// neighbours further away carry no inline button.
func versionActionMap(self int64, rels *relations.ImageRelations) map[int64]string {
	m := map[int64]string{self: "this"}
	if rels.VersionParent != nil {
		m[*rels.VersionParent] = "earlier"
	}
	if rels.VersionChild != nil {
		m[*rels.VersionChild] = "newer"
	}
	return m
}

// recomputePhashPost recomputes phash for the named image. Hooked from
// the detail-page [backfill] chip when images.phash is NULL.
func (s *Server) recomputePhashPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	if err := gallery.RecomputeAndStorePhash(r.Context(), cx.DB, id, cx.ThumbnailsPath); err != nil {
		logx.Warnf("recompute phash %d: %v", id, err)
		// Flash at 200: htmx ignores HX-Trigger on a non-2xx response and
		// the form is hx-swap="none", so a 500 here gives no feedback.
		// Only the success arm refreshes, or the reload would wipe this.
		setFlashHeader(w, "phash recompute failed (is the thumbnail present?)", "err", nil)
		return
	}
	cx.InvalidatePhashMissing()
	hxDone(w, r, "phash recomputed.", "", "/images/"+strconv.FormatInt(id, 10))
}

// addRelationPost installs a relation between two images. Form fields:
//   - type: duplicate | alternate | version | derivative | not_related
//   - a, b: image ids (integers); `a` defaults to "this image"
//   - direction: "ab" (default) or "ba" - "ba" treats `b` as the
//     operator's left, so every relation reads with the same
//     left-to-right convention regardless of which input slot the
//     swap arrow ended on
func (s *Server) addRelationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	a, b, ok := parseRelationPair(w, r)
	if !ok {
		return
	}
	if r.FormValue("direction") == "ba" {
		a, b = b, a
	}
	relType := r.FormValue("type")
	force := r.FormValue("force") == "true"
	if force {
		if err := cx.RelationsSvc.ClearBetween(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	}
	var err error
	switch relType {
	case "duplicate":
		err = cx.RelationsSvc.AddDuplicate(a, b)
	case "alternate":
		err = cx.RelationsSvc.AddAlternate(a, b)
	case "version":
		if force {
			// ClearBetween only touched the a-b pair; a version conflict
			// with a third image still blocks the insert. After the
			// direction swap above, a is the new parent and b the new
			// child; drop only the rows that would violate the per-row
			// uniqueness for this (parent, child) so existing chain
			// entries on either endpoint that don't conflict survive.
			if cErr := cx.RelationsSvc.ClearVersionEdgeConflictsFor(a, b); cErr != nil {
				writeRelationError(w, cErr)
				return
			}
		}
		err = cx.RelationsSvc.AddVersionEdge(a, b)
	case "derivative":
		if force {
			// Same reasoning as version: the schema allows only one
			// source per derivative; drop the existing row so the new
			// source can attach.
			if cErr := cx.RelationsSvc.ClearDerivativeSourceOf(b); cErr != nil {
				writeRelationError(w, cErr)
				return
			}
		}
		err = cx.RelationsSvc.AddDerivativeEdge(a, b)
	case "not_related":
		err = cx.RelationsSvc.AddNotRelated(a, b)
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	cx.InvalidateCaches()
	setFlashHeader(w, "Relation added.", "ok", nil)
	writeInlineFlash(w, "ok", "Relation added.")
}

// removeRelationPost / removeRelationDelete unlinks a relation. Form
// fields:
//   - type: duplicate | alternate | version | derivative | not_related |
//     promote-original | dissolve-dup | dissolve-alt |
//     dissolve-version | dissolve-derivative
//   - a, b: image ids (most types); group_id for dup / alt dissolve and
//     promote; root_id for version / derivative dissolve
//   - image_id for promote-original (the new original)
//
// relationRemoveOp is one arm of the remove/dissolve vocabulary: where
// its target comes from, what to call with it, and what the flash says.
// An empty field means the arm reads the a/b pair instead of one id -
// parseRelationPair carries its own self-relation guard, so the two
// readers are not interchangeable.
type relationRemoveOp struct {
	field string
	msg   string
	run   func(svc *relations.Service, a, b int64) error
}

var relationRemoveOps = map[string]relationRemoveOp{
	// "duplicate" / "alternate" unlink one image from its group;
	// "dissolve-*" wipes the whole group instead.
	"duplicate": {field: "image_id", msg: "Relation removed.",
		run: func(svc *relations.Service, id, _ int64) error { return svc.RemoveDupMember(id) }},
	"alternate": {field: "image_id", msg: "Relation removed.",
		run: func(svc *relations.Service, id, _ int64) error { return svc.RemoveAltMember(id) }},
	// The form posts a, b in chain order (parent, child).
	"version":     {msg: "Relation removed.", run: (*relations.Service).RemoveVersionEdge},
	"derivative":  {msg: "Relation removed.", run: (*relations.Service).RemoveDerivativeEdge},
	"not_related": {msg: "Relation removed.", run: (*relations.Service).RemoveNotRelated},
	"dissolve-dup": {field: "group_id", msg: "Group dissolved.",
		run: func(svc *relations.Service, gid, _ int64) error { return svc.DissolveDupGroup(gid) }},
	"dissolve-alt": {field: "group_id", msg: "Group dissolved.",
		run: func(svc *relations.Service, gid, _ int64) error { return svc.DissolveAltGroup(gid) }},
	"dissolve-version": {field: "root_id", msg: "Version chain dissolved.",
		run: func(svc *relations.Service, rid, _ int64) error { return svc.DissolveVersionChain(rid) }},
	"dissolve-derivative": {field: "root_id", msg: "Derivative tree dissolved.",
		run: func(svc *relations.Service, rid, _ int64) error { return svc.DissolveDerivativeTree(rid) }},
}

func (s *Server) removeRelationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	relType := r.FormValue("type")
	var msg string
	switch op, tabled := relationRemoveOps[relType]; {
	case tabled:
		var a, b int64
		var ok bool
		if op.field != "" {
			a, ok = formInt64(w, r, op.field)
		} else {
			a, b, ok = parseRelationPair(w, r)
		}
		if !ok {
			return
		}
		if err := op.run(cx.RelationsSvc, a, b); err != nil {
			writeRelationError(w, err)
			return
		}
		msg = op.msg
	case relType == "promote-original":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		id, ok := formInt64(w, r, "image_id")
		if !ok {
			return
		}
		if err := cx.RelationsSvc.PromoteToOriginal(gid, id); err != nil {
			writeRelationError(w, err)
			return
		}
		msg = "Original updated."
	case relType == "review-again":
		// "review-again" undoes a 2-member relation and reopens the
		// session on that exact pair. The `kind` field disambiguates
		// which relation the dissolve targets (dup / alt / version /
		// derivative / not_related); for dup / alt we accept a group_id
		// and dissolve the whole 2-member group (the template only shows
		// review-again on such groups), for version / derivative /
		// not_related we accept a, b. reviewAgainPost writes its own
		// response (a redirect to the pinned session, or an error flash).
		reviewAgainPost(w, r, cx)
		return
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	cx.InvalidateCaches()
	setFlashHeader(w, msg, "ok", nil)
	writeInlineFlash(w, "ok", msg)
}

// reviewAgainRemovers drops the edge behind each pair-shaped review-again
// kind. The group kinds are absent: they dissolve a group by id instead.
var reviewAgainRemovers = map[string]func(*relations.Service, int64, int64) error{
	"version":     (*relations.Service).RemoveVersionEdge,
	"derivative":  (*relations.Service).RemoveDerivativeEdge,
	"not_related": (*relations.Service).RemoveNotRelated,
}

// reviewAgainPost dissolves a 2-member relation, clears any matching
// not_related_pairs row so the find-pairs job stops skipping it, queues
// the pair onto potential_relation_pairs, and redirects to the session
// pinned to that exact pair so the operator reclassifies the pair they
// clicked rather than whatever sorts first. Writes its own response: a
// redirect on success, an error flash otherwise.
func reviewAgainPost(w http.ResponseWriter, r *http.Request, cx *galleryCtx) {
	subkind := r.FormValue("kind")
	var a, b int64
	switch subkind {
	case "duplicate", "alternate":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		// A group dissolved since the page rendered still yields one row
		// of NULLs, and MIN / MAX alone can't tell a pair from a wider
		// group.
		var n int
		var ar, br int64
		if err := cx.DB.Read.QueryRow(
			`SELECT COUNT(*), COALESCE(MIN(image_id), 0), COALESCE(MAX(image_id), 0)
			 FROM `+groupMembersTable(subkind)+` WHERE group_id = ?`, gid,
		).Scan(&n, &ar, &br); err != nil {
			writeRelationError(w, err)
			return
		}
		if n != 2 {
			flashStatus(w, http.StatusBadRequest, "Group must have exactly two members.")
			return
		}
		a, b = ar, br
		if subkind == "duplicate" {
			if err := cx.RelationsSvc.DissolveDupGroup(gid); err != nil {
				writeRelationError(w, err)
				return
			}
		} else {
			if err := cx.RelationsSvc.DissolveAltGroup(gid); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	case "version", "derivative", "not_related":
		ar, br, ok := parseRelationPair(w, r)
		if !ok {
			return
		}
		if err := reviewAgainRemovers[subkind](cx.RelationsSvc, ar, br); err != nil {
			writeRelationError(w, err)
			return
		}
		a, b = ar, br
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown review-again kind.")
		return
	}
	// not_related_pairs is keyed (a,b) without canonical ordering;
	// the existing AddNotRelated normalises before insert but the
	// row could carry either orientation. Sweep both directions to
	// avoid leaving a skipper row alive that would keep the pair
	// out of the queue after find-pairs runs.
	if _, err := cx.DB.Write.Exec(
		`DELETE FROM not_related_pairs WHERE (a_image_id = ? AND b_image_id = ?) OR (a_image_id = ? AND b_image_id = ?)`,
		a, b, b, a,
	); err != nil {
		writeRelationError(w, err)
		return
	}
	// potential_relation_pairs canonicalises (min, max). INSERT OR
	// IGNORE keeps any pre-existing queue row alive at its real
	// distance; the session is pinned to this pair via the redirect
	// below regardless of where it would otherwise sort.
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	// source='review' rather than a detector: the operator asking to see
	// the pair again is its whole provenance, and claiming a phash match
	// that never happened would misread on the session card.
	if _, err := cx.DB.Write.Exec(
		`INSERT OR IGNORE INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at, source)
		 VALUES (?, ?, 0, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), ?)`,
		lo, hi, relations.SourceReview,
	); err != nil {
		writeRelationError(w, err)
		return
	}
	cx.InvalidateCaches()
	dest := "/relations/session?a=" + strconv.FormatInt(lo, 10) + "&b=" + strconv.FormatInt(hi, 10)
	hxRedirect(w, r, dest)
}

// groupMembersTable returns the per-kind members-table name for
// dup / alt 2-member groups. Hardcoded list - the only callers are
// reviewAgainPost and any future symmetric mutation.
func groupMembersTable(subkind string) string {
	if subkind == "alternate" {
		return "alt_group_members"
	}
	return "dup_group_members"
}

// copyTagsPreviewGroup is one category bucket of new tag names the
// preview dialog renders. Tags inside the bucket are ordered by name;
// buckets are ordered the way `groupByCategory` orders the rest of the
// app's tag lists (general first, custom last).
type copyTagsPreviewGroup struct {
	Category string
	Color    string
	Tags     []string
}

// copyTagsToOriginalPreview renders a small partial listing the tag
// names CopyTagsFromDuplicatesToOriginal would insert onto the
// original. Used by the marked-duplicates walker's preview dialog so
// the operator sees what they are about to merge before confirming.
func (s *Server) copyTagsToOriginalPreview(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	gid, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var original int64
	if err := cx.DB.Read.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, gid).Scan(&original); err != nil {
		http.NotFound(w, r)
		return
	}
	rows, err := cx.DB.Read.Query(`
		SELECT DISTINCT t.name, COALESCE(c.name, ''), COALESCE(c.color, '')
		FROM image_tags it
		JOIN dup_group_members m ON m.image_id = it.image_id
		JOIN tags t ON t.id = it.tag_id
		LEFT JOIN tag_categories c ON c.id = t.category_id
		WHERE m.group_id = ?
		  AND m.image_id != ?
		  AND (c.name IS NULL OR c.name != 'rating')
		  AND NOT EXISTS (
		    SELECT 1 FROM image_tags it2 WHERE it2.image_id = ? AND it2.tag_id = it.tag_id
		  )
		ORDER BY c.name, t.name`,
		gid, original, original,
	)
	if err != nil {
		http.Error(w, "load preview", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		name     string
		category string
		color    string
	}
	var entries []row
	for rows.Next() {
		var rec row
		if scanErr := rows.Scan(&rec.name, &rec.category, &rec.color); scanErr != nil {
			http.Error(w, "scan preview", http.StatusInternalServerError)
			return
		}
		entries = append(entries, rec)
	}
	category := func(e row) string { return cmp.Or(e.category, "(uncategorised)") }
	groups := groupOrdered(entries, nil, category,
		func(e row) *copyTagsPreviewGroup {
			return &copyTagsPreviewGroup{Category: category(e), Color: e.color}
		},
		func(g *copyTagsPreviewGroup, e row) { g.Tags = append(g.Tags, e.name) })
	total := 0
	for _, g := range groups {
		total += len(g.Tags)
	}
	s.renderTemplate(w, "partials/copy_tags_preview.html", map[string]any{
		"GroupID":    gid,
		"OriginalID": original,
		"Groups":     groups,
		"Total":      total,
		"CSRFToken":  s.csrfToken(sessionFromContext(r.Context())),
	})
}

// reverseRelationPost flips the direction of a version or derivative
// edge. Form fields:
//   - type: "version" | "derivative"
//   - a, b: image ids in their current direction (parent/child or
//     source/derivative). After commit, b -> a.
func (s *Server) reverseRelationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	a, b, ok := parseRelationPair(w, r)
	if !ok {
		return
	}
	switch r.FormValue("type") {
	case "version":
		if err := cx.RelationsSvc.ReverseVersionEdge(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	case "derivative":
		if err := cx.RelationsSvc.ReverseDerivativeEdge(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	cx.InvalidateCaches()
	writeInlineFlash(w, "ok", "Edge reversed.")
}

// mergeGroupsPost merges N alt or dup groups into one. Form fields:
//   - kind: "alt" or "dup"
//   - group_id: repeated (the operator's selection). At least two
//     distinct ids are required.
//   - keep_original_from (dup only): the group whose original_image_id
//     becomes the survivor's original. Empty preserves the survivor's
//     existing original.
//
// On success the response carries HX-Redirect back to the unified
// browse page so the freshly merged group renders without a manual
// reload.
func (s *Server) mergeGroupsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	kind := r.URL.Query().Get("kind")
	kind = cmp.Or(kind, r.FormValue("kind"))
	if kind != "alt" && kind != "dup" {
		flashStatus(w, http.StatusBadRequest, "Unknown merge kind.")
		return
	}
	ids := parseIDList(r.Form["group_id"])
	if len(ids) < 2 {
		flashStatus(w, http.StatusBadRequest, "Pick at least two groups to merge.")
		return
	}
	switch kind {
	case "alt":
		if err := cx.RelationsSvc.MergeAltGroups(ids); err != nil {
			writeRelationError(w, err)
			return
		}
	case "dup":
		var keep int64
		if raw := strings.TrimSpace(r.FormValue("keep_original_from")); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				flashStatus(w, http.StatusBadRequest, "Invalid keep_original_from.")
				return
			}
			if !slices.Contains(ids, v) {
				flashStatus(w, http.StatusBadRequest, "keep_original_from must be one of the merging groups.")
				return
			}
			keep = v
		}
		if err := cx.RelationsSvc.MergeDupGroups(ids, keep); err != nil {
			writeRelationError(w, err)
			return
		}
	}
	cx.InvalidateCaches()
	redirectKind := "duplicate"
	if kind == "alt" {
		redirectKind = "alternate"
	}
	target := "/relations/browse?kind=" + redirectKind
	hxRedirect(w, r, target)
}

// dissolveGroupsPost is the batch counterpart to removeRelationPost.
// One request dissolves every group / chain / tree / pair the operator
// ticked in the browse-page toolbar. Form fields per kind mirror the
// per-row dissolve forms:
//   - duplicate / alternate: group_id (repeated)
//   - version / derivative:  root_id  (repeated)
//   - not_related:           pair     (repeated, "a:b")
//
// Each dissolve runs in its own transaction (matching the service API);
// a mid-batch error leaves prior rows committed and surfaces the error
// flash so the operator can retry. On success the response carries
// HX-Redirect back to the same tab so the toolbar repaints.
func (s *Server) dissolveGroupsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	kind := r.URL.Query().Get("kind")
	kind = cmp.Or(kind, r.FormValue("kind"))
	switch kind {
	case "duplicate", "alternate", "version", "derivative", "not_related":
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown dissolve kind.")
		return
	}
	switch kind {
	case "duplicate":
		for _, gid := range parseIDList(r.Form["group_id"]) {
			if err := cx.RelationsSvc.DissolveDupGroup(gid); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	case "alternate":
		for _, gid := range parseIDList(r.Form["group_id"]) {
			if err := cx.RelationsSvc.DissolveAltGroup(gid); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	case "version":
		for _, rid := range parseIDList(r.Form["root_id"]) {
			if err := cx.RelationsSvc.DissolveVersionChain(rid); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	case "derivative":
		for _, rid := range parseIDList(r.Form["root_id"]) {
			if err := cx.RelationsSvc.DissolveDerivativeTree(rid); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	case "not_related":
		for _, raw := range r.Form["pair"] {
			a, b, ok := parsePairValue(raw)
			if !ok {
				continue
			}
			if err := cx.RelationsSvc.RemoveNotRelated(a, b); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	}
	cx.InvalidateCaches()
	target := "/relations/browse?kind=" + kind
	hxRedirect(w, r, target)
}

// parseIDList trims, parses, and de-duplicates a repeated int64 form
// field. Values that fail to parse are silently dropped (the operator
// just ticks rows; there is no hand-edited input here).
func parseIDList(raw []string) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// parsePairValue decodes the "a:b" shape the not_related checkbox
// carries (the only kind that needs two ids per row, since the others
// reduce to a group or chain root).
func parsePairValue(raw string) (int64, int64, bool) {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 || colon == len(raw)-1 {
		return 0, 0, false
	}
	a, err := strconv.ParseInt(strings.TrimSpace(raw[:colon]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.ParseInt(strings.TrimSpace(raw[colon+1:]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return a, b, true
}

// copyTagsToOriginalPost runs CopyTagsFromDuplicatesToOriginal for a
// duplicate group. Wired to the per-card button on the Relations page.
func (s *Server) copyTagsToOriginalPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	gid, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	added, err := cx.RelationsSvc.CopyTagsFromDuplicatesToOriginal(gid)
	if err != nil {
		writeRelationError(w, err)
		return
	}
	writeInlineFlash(w, "ok", fmt.Sprintf("Copied %d tag(s) to the original.", added))
}

// parseRelationPair reads form fields a and b, returning the validated
// ids or false (already writing the error flash).
func parseRelationPair(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	a, ok := formInt64(w, r, "a")
	if !ok {
		return 0, 0, false
	}
	b, ok := formInt64(w, r, "b")
	if !ok {
		return 0, 0, false
	}
	if a == b {
		// Status 200 mirrors writeRelationError so HTMX swaps the body
		// into the dialog's target; a 4xx response would be dropped on
		// the floor by the default htmx config and the operator would
		// see no feedback at all.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		flashStatus(w, http.StatusOK, "Cannot relate an image to itself.")
		return 0, 0, false
	}
	return a, b, true
}

// writeRelationError maps a relations.Service error to a friendly
// flash through the shared FriendlyErrorFor mapping. The status stays
// 200 for HTMX callers so the in-dialog target swap actually paints
// the message - 4xx responses don't swap by default.
func writeRelationError(w http.ResponseWriter, err error) {
	msg := err.Error()
	if fe := relations.FriendlyErrorFor(err); fe != nil {
		msg = fe.Message
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	flashStatus(w, http.StatusOK, msg)
}
