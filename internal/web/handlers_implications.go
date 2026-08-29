package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// implicationsDialogHandler renders the body of the implications dialog
// on the /tags page: one chip per direct implication with a delete
// button, plus a multi-tag input with autocomplete to declare new edges.
func (s *Server) implicationsDialogHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	// Fetched as a fragment by the tag detail page's implications
	// editor; a non-htmx caller (refresh, bookmark, shared link) gets
	// the tag's detail page rather than a chrome-less fragment.
	if !isHTMXRequest(r) {
		http.Redirect(w, r, fmt.Sprintf("/tags/%d", id), http.StatusSeeOther)
		return
	}
	parent, err := s.tagSvc().GetTag(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	imps, err := s.tagSvc().ListImplications(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	labels := make([]string, 0, len(imps))
	for _, im := range imps {
		labels = append(labels, im.Origin)
	}
	data := map[string]any{
		"Parent":       parent,
		"Implications": imps,
		"CSRFToken":    s.csrfToken(sessionFromContext(r.Context())),
		"OriginKinds":  s.originKinds(labels),
	}
	s.renderTemplate(w, "partials/implications_dialog.html", data)
}

// declareImplications reads the tag-input field, resolves each token
// the way the detail-page tag input does (space-separated multi-add,
// "category:name", quoted spans) and declares one edge per token. edge
// turns a resolved tag id into the (parent, implied) pair, which is the
// only thing the two directions disagree on. Returns how many edges were
// new and the per-token failures; a nil return means it already answered
// the request with a parse error.
func (s *Server) declareImplications(w http.ResponseWriter, r *http.Request, field string, edge func(tagID int64) (parent, implied int64)) (added int, failures []string, ok bool) {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		writeInlineFlash(w, "err", "Tag name is required.")
		return 0, nil, false
	}
	catTags, _, parseErrMsg := s.parseTagInput(raw)
	if parseErrMsg != "" {
		writeInlineFlash(w, "err", parseErrMsg)
		return 0, nil, false
	}
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		parent, implied := edge(tag.ID)
		isNew, err := s.tagSvc().AddImplication(parent, implied)
		if err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		if isNew {
			added++
			s.startImplicationPropagation(parent, implied, "add")
		}
	}
	if added > 0 {
		// New targets may have been created via GetOrCreateTag, so the
		// cached tag count is stale until the next render.
		s.Active().InvalidateCaches()
	}
	return added, failures, true
}

// implicationsAddedMsg is the success line both directions report.
func implicationsAddedMsg(added int) string {
	noun := "implication"
	if added != 1 {
		noun = "implications"
	}
	return strconv.Itoa(added) + " " + noun + " added."
}

// writeImplicationFailures reports a run that added nothing or only
// part of what was asked. Returns false when there is nothing to report
// and the caller owns the success response.
func writeImplicationFailures(w http.ResponseWriter, added int, failures []string) bool {
	switch {
	case len(failures) == 0 && added > 0:
		return false
	case len(failures) == 0:
		writeInlineFlash(w, "ok", "Already declared.")
	case added > 0:
		writeInlineFlash(w, "err", "Added "+strconv.Itoa(added)+". Failed: "+strings.Join(failures, "; "))
	default:
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
	}
	return true
}

func (s *Server) addImplicationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	parentID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	added, failures, ok := s.declareImplications(w, r, "implied_id",
		func(tagID int64) (int64, int64) { return parentID, tagID })
	if !ok {
		return
	}
	if added > 0 {
		// implication-added drives the dialog's after-request hook
		// (re-fetch body without closing the modal); monbooru:flash rides
		// the shared helper so the next /tags reload surfaces the green
		// message above the table.
		setFlashHeader(w, implicationsAddedMsg(added), "ok", map[string]any{"implication-added": ""})
	}
	if !writeImplicationFailures(w, added, failures) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// addImpliedByPost is the tag detail page's inline inverse editor: each
// token in `parent_id` becomes a parent implying {id}.
func (s *Server) addImpliedByPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	impliedID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	added, failures, ok := s.declareImplications(w, r, "parent_id",
		func(tagID int64) (int64, int64) { return tagID, impliedID })
	if !ok {
		return
	}
	if !writeImplicationFailures(w, added, failures) {
		hxDone(w, r, implicationsAddedMsg(added), "", fmt.Sprintf("/tags/%d", impliedID))
	}
}

func (s *Server) removeImplicationDelete(w http.ResponseWriter, r *http.Request) {
	parentID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	impliedID, ok := pathInt64(w, r, "impliedID")
	if !ok {
		return
	}
	if err := s.tagSvc().RemoveImplication(parentID, impliedID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.startImplicationPropagation(parentID, impliedID, "remove")
	// Seed the cross-navigation flash slot; the dialog stays open and the
	// /tags reload on close surfaces this above the table.
	setFlashHeader(w, "Implication removed.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

// removeImplicationsDelete and removeImpliedByDelete remove every edge in one
// origin subgroup of the detail page's outbound / inbound implication lists.
func (s *Server) removeImplicationsDelete(w http.ResponseWriter, r *http.Request) {
	s.removeImplicationGroup(w, r, s.tagSvc().ListImplications)
}

func (s *Server) removeImpliedByDelete(w http.ResponseWriter, r *http.Request) {
	s.removeImplicationGroup(w, r, s.tagSvc().ImpliedBy)
}

// removeImplicationGroup drops the edges list reports for the named origin
// subgroup, then sweeps the image side for the whole group in one job: the
// per-edge job removeImplicationDelete starts is refused for every edge after
// the first, which would leave implied rows behind.
func (s *Server) removeImplicationGroup(w http.ResponseWriter, r *http.Request, list func(int64) ([]models.Implication, error)) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	origin, stale := relationGroupFilter(r)
	edges, err := list(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var removed []models.Implication
	for _, im := range edges {
		if im.Origin != origin || im.Stale != stale {
			continue
		}
		if err := s.tagSvc().RemoveImplication(im.ParentID, im.ImpliedID); err != nil {
			logx.Warnf("remove implication %d -> %d: %v", im.ParentID, im.ImpliedID, err)
			continue
		}
		removed = append(removed, im)
	}
	if len(removed) > 0 {
		s.startImplicationGroupSweep(removed)
	}
	noun := "implication"
	if len(removed) != 1 {
		noun = "implications"
	}
	setFlashHeader(w, strconv.Itoa(len(removed))+" "+noun+" removed.", "ok",
		map[string]any{"tag-relations-changed": ""})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startImplicationGroupSweep(edges []models.Implication) {
	if err := s.jobs.Start(models.JobTypeTag); err != nil {
		logx.Warnf("implication group sweep skipped: %v", err)
		return
	}
	go s.runImplicationGroupSweep(edges)
}

// runImplicationGroupSweep drops the implied rows the removed edges no longer
// justify, reusing one closure per distinct implied tag, then reconciles the
// counts of those tags.
func (s *Server) runImplicationGroupSweep(edges []models.Implication) {
	ctx := s.jobs.Context()
	total := len(edges)
	closures := map[int64][]int64{}
	processed := 0
	cancelled := false

	s.jobs.Update(0, total, "removing implications…")
	for i, im := range edges {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if _, ok := closures[im.ImpliedID]; !ok {
			closure, err := s.resolveRemoveClosure(im.ImpliedID)
			if err != nil {
				s.jobs.Fail(err.Error())
				return
			}
			closures[im.ImpliedID] = closure
		}
		if err := s.sweepImplicationRemovalInline(ctx, im.ParentID, closures[im.ImpliedID]); err != nil {
			logx.Warnf("implication sweep %d -> %d: %v", im.ParentID, im.ImpliedID, err)
		}
		processed = i + 1
		s.jobs.Update(processed, total, "removing implications…")
	}

	implied := make([]int64, 0, len(closures))
	for tagID := range closures {
		implied = append(implied, tagID)
	}
	if err := s.tagSvc().RecalcIDs(implied); err != nil {
		logx.Warnf("implication group sweep recalc: %v", err)
	}
	s.Active().InvalidateCaches()
	s.finishJob(nil, cancelled,
		fmt.Sprintf("implication sweep cancelled (%d/%d)", processed, total),
		fmt.Sprintf("swept %d removed implication(s)", processed))
}

// startImplicationPropagation kicks off the background job that fans
// out (op="add") or sweeps (op="remove") the parent → implied edge
// across every image carrying parent. Skipped when a job is already
// running; the user can re-trigger by editing the implication again
// (the in-DB edge is independent of this propagation, so search and
// future adds still see it through the synchronous transitive walk).
func (s *Server) startImplicationPropagation(parentID, impliedID int64, op string) {
	if err := s.jobs.Start(models.JobTypeTag); err != nil {
		logx.Warnf("implication %s skipped: %v", op, err)
		return
	}
	go s.runImplicationPropagation(parentID, impliedID, op)
}

// resolveRemoveClosure walks the removed target's transitive implied
// closure once, in a throwaway read tx, and returns it with the target
// prepended. The closure is invariant across a removal sweep, so the
// removal runners resolve it up front instead of paying an N x graph-walk
// inside the writer-held chunk transactions.
func (s *Server) resolveRemoveClosure(tagID int64) ([]int64, error) {
	tx, err := s.db().Read.Begin()
	if err != nil {
		return nil, err
	}
	closure, err := tags.TransitiveImpliedTx(tx, []int64{tagID})
	_ = tx.Rollback()
	if err != nil {
		return nil, err
	}
	return append([]int64{tagID}, closure...), nil
}

// imageIDsWithTag returns the ids of every image carrying tagID, in id
// order.
func (s *Server) imageIDsWithTag(ctx context.Context, tagID int64) ([]int64, error) {
	return db.QueryIDsContext(ctx, s.db().Read,
		`SELECT image_id FROM image_tags WHERE tag_id = ? ORDER BY image_id`, tagID)
}

// chunkImageTagsByParent runs perImage for every image carrying
// parentID, in id order, committing 500-image write transactions and
// bailing between chunks when ctx is cancelled.
func (s *Server) chunkImageTagsByParent(ctx context.Context, parentID int64, perImage func(*sql.Tx, int64) error) error {
	ids, err := s.imageIDsWithTag(ctx, parentID)
	if err != nil {
		return err
	}
	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		for _, imageID := range ids[start:min(start+chunkSize, len(ids))] {
			if err := perImage(tx, imageID); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) runImplicationPropagation(parentID, impliedID int64, op string) {
	ctx := s.jobs.Context()
	const chunkSize = 500

	ids, err := s.imageIDsWithTag(ctx, parentID)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	verb := "applying implication"
	if op == "remove" {
		verb = "removing implication"
	}

	var removeClosure []int64
	if op == "remove" {
		var err error
		removeClosure, err = s.resolveRemoveClosure(impliedID)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
	}

	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, verb, func(chunk []int64) error {
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		ratingCatID := s.tagSvc().RatingCategoryID()
		for _, imageID := range chunk {
			if op == "add" {
				if err := propagateAddImplication(tx, imageID, parentID, ratingCatID); err != nil {
					_ = tx.Rollback()
					return err
				}
			} else {
				if err := propagateRemoveImplication(tx, imageID, removeClosure); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		return tx.Commit()
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d)", verb, processed, len(ids)))
		return
	}

	if err := s.tagSvc().RecalcIDs([]int64{impliedID}); err != nil {
		logx.Warnf("implication propagation recalc: %v", err)
	}
	s.Active().InvalidateCaches()
	s.jobs.Complete(fmt.Sprintf("%s applied to %d image(s)", verb, processed))
}

// propagateAddImplication backfills implied rows for the parent on the
// given image, mirroring what addTagToImageTxReportingDup would have
// done if the implication had existed at the original add time.
// Existing rows are left alone; only fresh INSERTs get is_implied=1.
func propagateAddImplication(tx *sql.Tx, imageID, parentID, ratingCatID int64) error {
	var isAuto int
	err := tx.QueryRow(
		`SELECT is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, parentID,
	).Scan(&isAuto)
	if err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	return tags.ApplyImpliedFanoutTx(tx, imageID, parentID, ratingCatID, isAuto == 1)
}

// propagateRemoveImplication drops the rows on this image whose only
// justification was the now-deleted edge. The closure (impliedID plus its
// transitive children) is resolved once by the caller and reused across
// every image carrying the parent. The edge is already gone, so nothing
// is excluded from the still-implied check.
func propagateRemoveImplication(tx *sql.Tx, imageID int64, closure []int64) error {
	_, err := tags.SweepImpliedClosureTx(tx, imageID, closure, 0)
	return err
}
