package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

const collectionsPerPage = 60

// Preview tiles fetched per collection. Generous so the strip fills a wide
// row; the template clips the overflow to a single line.
const collectionPreviewSamples = 16

// Tiles per fetch of the click-to-order dialog body.
const collectionOrderWindow = 200

type collectionsPageData struct {
	baseData
	Collections []gallery.CollectionSummary
	Total       int
	Page        int
	TotalPages  int
	Prefix      string
	Sort        string
}

func (s *Server) collectionsHandler(w http.ResponseWriter, r *http.Request) {
	// Rename / dissolve mutate the listing; opt out of caching so a reload
	// after a job never serves a stale render.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	prefix := strings.TrimSpace(q.Get("q"))
	sortStr := q.Get("sort")
	if sortStr != "name" {
		sortStr = "size"
	}
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}
	excludeIDs := resolveCeiling(r, s.Active()).ExcludedTagIDs()

	total, err := gallery.CountCollections(s.db(), prefix, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	totalPages := (total + collectionsPerPage - 1) / collectionsPerPage
	if total > 0 && page > totalPages {
		page = totalPages
	}

	list, err := gallery.ListCollections(s.db(), prefix, sortStr, collectionsPerPage, (page-1)*collectionsPerPage, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := make([]string, len(list))
	for i := range list {
		names[i] = list[i].Name
	}
	samples, err := gallery.CollectionSamples(s.db(), names, collectionPreviewSamples, excludeIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range list {
		list[i].Samples = samples[strings.ToLower(list[i].Name)]
	}

	s.renderTemplate(w, "collections.html", collectionsPageData{
		baseData:    s.base(r, "collections", "Collections - "+s.booruName()),
		Collections: list,
		Total:       total,
		Page:        page,
		TotalPages:  totalPages,
		Prefix:      prefix,
		Sort:        sortStr,
	})
}

// renameCollectionPost relabels a collection across every member as a
// background tag job. Renaming onto an existing label merges the two.
func (s *Server) renameCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	oldName := strings.TrimSpace(r.FormValue("prev"))
	newName := strings.TrimSpace(r.FormValue("name"))
	if oldName == "" || newName == "" {
		flashStatus(w, http.StatusBadRequest, "Both the current and the new collection name are required.")
		return
	}
	if len(newName) > maxExternalSourceLen {
		flashStatus(w, http.StatusBadRequest, "Collection label too long.")
		return
	}
	if oldName == newName {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.startCollectionJob(w, oldName, func(ids []int64) {
		s.runRenameCollection(ids, oldName, newName)
	})
}

// collectionFindRelationsPost flips a collection's find-relations opt-in
// and answers with the refreshed switch so the htmx swap shows the new
// state in place.
func (s *Server) collectionFindRelationsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		flashStatus(w, http.StatusBadRequest, "Collection label required.")
		return
	}
	enabled := r.FormValue("enabled") == "1"
	if err := gallery.SetCollectionFindRelations(s.db(), name, enabled); err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not update the collection.")
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	setFlashHeader(w, "Find relations "+verb+" for "+name+".", "ok", nil)
	s.renderTemplate(w, "partials/collection_find_relations.html", map[string]any{
		"Name":          name,
		"FindRelations": enabled,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
	})
}

// collectionOrderDialog renders the click-to-order dialog body: the
// first visible members of the collection under the caller's ceiling,
// in the current reading order. Windowed by limit so a huge label
// doesn't render tens of thousands of tiles in one dialog body; the
// [show more] button re-fetches with a larger limit.
func (s *Server) collectionOrderDialog(w http.ResponseWriter, r *http.Request) {
	// Fetched as a fragment by the collections page's order dialog; a
	// non-htmx caller (refresh, bookmark, shared link) gets the listing
	// rather than a chrome-less fragment.
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/collections", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "collection required", http.StatusBadRequest)
		return
	}
	limit := collectionOrderWindow
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	excludeIDs := resolveCeiling(r, s.Active()).ExcludedTagIDs()
	members, err := gallery.CollectionMembers(s.db(), name, excludeIDs, limit+1, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hasMore := len(members) > limit
	if hasMore {
		members = members[:limit]
	}
	ceilingHidden, _ := gallery.CollectionCeilingHidden(s.db(), name, excludeIDs)
	s.renderTemplate(w, "partials/collection_order.html", map[string]any{
		"Name":          name,
		"Members":       members,
		"Gallery":       s.activeGallery(),
		"HasMore":       hasMore,
		"NextLimit":     limit + collectionOrderWindow,
		"CeilingHidden": ceilingHidden,
	})
}

// reorderCollectionPost applies the dialog's click order: the listed ids
// get positions 1..N, every other member goes unordered.
func (s *Server) reorderCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		flashStatus(w, http.StatusBadRequest, "Collection label required.")
		return
	}
	// Filename mode ignores the clicked ids and orders the whole collection by
	// filename, ceiling-blind.
	if r.FormValue("mode") == "filename" {
		if err := gallery.SortCollectionByFilename(s.db(), name); err != nil {
			flashStatus(w, http.StatusInternalServerError, "Could not reorder the collection.")
			return
		}
		s.Active().InvalidateCaches()
		writeInlineFlash(w, "ok", "Ordered by filename.")
		return
	}
	raw := strings.TrimSpace(r.FormValue("ids"))
	if raw == "" {
		flashStatus(w, http.StatusBadRequest, "Click at least one image first.")
		return
	}
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			flashStatus(w, http.StatusBadRequest, "Bad image id list.")
			return
		}
		ids = append(ids, id)
	}
	if err := gallery.ReorderCollection(s.db(), name, ids); err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not reorder the collection.")
		return
	}
	s.Active().InvalidateCaches()
	writeInlineFlash(w, "ok", fmt.Sprintf("Ordered %d image(s).", len(ids)))
}

// startCollectionJob materialises the collection's membership and spawns run
// as the jobs-lane background job, answering 202; shared by rename and
// dissolve.
func (s *Server) startCollectionJob(w http.ResponseWriter, name string, run func([]int64)) {
	ids, err := gallery.CollectionMemberIDs(s.db(), name)
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not read the collection.")
		return
	}
	if len(ids) == 0 {
		flashStatus(w, http.StatusBadRequest, "That collection no longer exists.")
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	go run(ids)
	w.WriteHeader(http.StatusAccepted)
}

// dissolveCollectionPost drops a collection label from every member as a
// background tag job. Images and files are left untouched; it reuses the
// batch-collection remove path over the full membership.
func (s *Server) dissolveCollectionPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		flashStatus(w, http.StatusBadRequest, "Collection label required.")
		return
	}
	s.startCollectionJob(w, name, func(ids []int64) {
		s.runBatchCollection(ids, name, "remove")
		// Drop the find-relations opt-in so a later collection reusing the
		// label starts from the disabled default.
		if _, err := s.db().Write.Exec(
			`DELETE FROM collection_find_relations WHERE name = ?`, name); err != nil {
			logx.Debugf("dissolve collection find-relations flag: %v", err)
		}
	})
}

// runRenameCollection relabels old to new across ids in chunks. An image
// already holding new keeps its existing membership (the old one is
// dropped so the relabel can't collide on the (image_id, name) key); the
// home mirror follows for rows homed on the old label. On a merge the
// incoming members are renumbered past the target's last position, so the
// two reading orders append instead of interleaving.
func (s *Server) runRenameCollection(ids []int64, oldName, newName string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	// A case-only rename ("New" -> "new") targets the same NOCASE label, so
	// there is nothing to merge; the collision delete below would otherwise
	// drop the very rows the relabel is meant to recase.
	merging := !strings.EqualFold(oldName, newName)

	// Read once, before any chunk relabels: the target's own members are
	// never renumbered, so the offset stays valid for the whole job.
	var posOffset int
	if merging {
		if err := s.db().Read.QueryRow(
			`SELECT COALESCE(MAX(position), 0) FROM image_collections WHERE name = ?`, newName,
		).Scan(&posOffset); err != nil {
			logx.Debugf("rename collection position offset: %v", err)
		}
	}

	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, "renaming collection", func(chunk []int64) error {
		placeholders, chunkArgs := db.InPlaceholders(chunk)
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if merging {
			if _, err := tx.Exec(
				`DELETE FROM image_collections
				 WHERE name = ? AND image_id IN (`+placeholders+`)
				   AND image_id IN (SELECT image_id FROM image_collections WHERE name = ?)`,
				append(append([]any{oldName}, chunkArgs...), newName)...,
			); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`UPDATE image_collections SET name = ?, position = position + ?
			 WHERE name = ? AND image_id IN (`+placeholders+`)`,
			append([]any{newName, posOffset, oldName}, chunkArgs...)...,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE images SET series = ? WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			append([]any{newName, oldName}, chunkArgs...)...,
		); err != nil {
			return err
		}
		// Sync the home position to whichever membership survived (the
		// pre-existing target's position wins on a merge).
		if _, err := tx.Exec(
			`UPDATE images SET series_order =
			   (SELECT position FROM image_collections WHERE image_id = images.id AND name = ?)
			 WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			append([]any{newName, newName}, chunkArgs...)...,
		); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err == nil {
		// Carry the find-relations opt-in to the new label; on a merge the
		// target's own row wins. The DELETE only runs when merging - on a
		// case-only rename it would match the row the UPDATE just recased.
		if _, e := s.db().Write.Exec(
			`UPDATE OR IGNORE collection_find_relations SET name = ? WHERE name = ?`, newName, oldName); e != nil {
			logx.Debugf("rename collection find-relations flag: %v", e)
		} else if merging {
			if _, e := s.db().Write.Exec(
				`DELETE FROM collection_find_relations WHERE name = ?`, oldName); e != nil {
				logx.Debugf("rename collection find-relations flag: %v", e)
			}
		}
		s.Active().InvalidateCaches()
	}
	s.finishJob(err, cancelled, fmt.Sprintf("rename cancelled (%d/%d processed)", processed, total), fmt.Sprintf("Renamed collection across %d image(s).", processed))
}
