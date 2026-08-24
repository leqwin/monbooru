package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

// skipReasons collects the distinct reasons a tag-scope batch refused
// rows. The runners answer 202 before they start, so the job summary is
// the only place the operator ever sees one; a bare "skipped N" there
// says nothing, and a run where every row was refused still lands in
// the terminal success state. Capped at three: the summary is one line
// and wants the shape of the failure, not every instance of it.
type skipReasons struct {
	seen []string
	// dropped counts the distinct reasons past the cap, so a truncated
	// list says it was cut rather than reading as the whole story.
	dropped int
}

func (s *skipReasons) add(err error) {
	msg := err.Error()
	if slices.Contains(s.seen, msg) {
		return
	}
	if len(s.seen) >= 3 {
		s.dropped++
		return
	}
	s.seen = append(s.seen, msg)
}

func (s *skipReasons) any() bool { return len(s.seen) > 0 }

func (s *skipReasons) String() string {
	out := strings.Join(s.seen, "; ")
	if s.dropped > 0 {
		out += fmt.Sprintf("; and %d more", s.dropped)
	}
	return out
}

// finishTagScopeJob writes a tag-scope batch's terminal state. A run
// that changed nothing and was refused for a reason fails rather than
// completes: the status widget renders any completion as a green check,
// so "0 of 1 rejected outright" and "1 of 1 succeeded" would otherwise
// look like the same event. A partial run stays a completion but still
// names why the rest was skipped.
func (s *Server) finishTagScopeJob(changed int, reasons skipReasons, cancelled bool, noun, summary string) {
	if reasons.any() {
		summary += ": " + reasons.String()
	}
	var failed error
	if !cancelled && changed == 0 && reasons.any() {
		failed = errors.New(summary)
	}
	s.finishJob(failed, cancelled, fmt.Sprintf("%s cancelled (%s)", noun, summary), summary)
}

// resolveTagScope resolves a tags-page batch POST's target set: an
// explicit ids list (the checkbox selection) when present, else every
// tag matching the posted filter fields.
func (s *Server) resolveTagScope(r *http.Request) ([]int64, error) {
	q := r.Form
	// A present-but-empty `ids` is an empty selection; the whole-search
	// escalation posts the filter fields with no `ids` field at all.
	if q.Has("ids") {
		idsStr := strings.TrimSpace(q.Get("ids"))
		if idsStr == "" {
			return nil, nil
		}
		var ids []int64
		for _, part := range strings.Split(idsStr, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad tag id %q", part)
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	// Resolve the posted filter fields exactly as the listing does, so a
	// whole-search escalation acts on what the page shows. ListTagIDs
	// orders by id and reads neither the page nor the limit.
	filter := s.tagListingFilter(tagListingParamsFrom(q))
	filter.PageIndex, filter.Limit = 0, 0
	return s.tagSvc().ListTagIDs(filter)
}

// startTagScopeJob wraps the shared scope-resolve / empty-scope / job-slot
// preamble of the batch POST handlers. Returns the ids and true when the
// caller should launch its runner; the response is already written
// otherwise.
func (s *Server) startTagScopeJob(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	ids, err := s.resolveTagScope(r)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return nil, false
	}
	if !s.startJob(w, models.JobTypeTag) {
		return nil, false
	}
	return ids, true
}

// startTagScopeRun wraps the tail every tag-scope batch POST shares:
// resolve the scope, launch run on it in the background, answer 202.
// The response is already written when the resolve or job slot fails.
func (s *Server) startTagScopeRun(w http.ResponseWriter, r *http.Request, run func(ids []int64)) {
	ids, ok := s.startTagScopeJob(w, r)
	if !ok {
		return
	}
	go run(ids)
	w.WriteHeader(http.StatusAccepted)
}

// runTagScopeLoop walks a tag-scope batch with cancellation and
// progress, counting each row as changed or skipped. op reports which;
// an error it returns is recorded as a refusal reason. Callers own the
// summary - they differ in nouns and in what else they count.
func (s *Server) runTagScopeLoop(ids []int64, gerund string, stride int, op func(id int64) (bool, error)) (changed, skipped int, reasons skipReasons, cancelled bool) {
	ctx := s.jobs.Context()
	total := len(ids)
	s.jobs.Update(0, total, gerund)
	for i, id := range ids {
		if ctx.Err() != nil {
			return changed, skipped, reasons, true
		}
		switch ok, err := op(id); {
		case err != nil:
			reasons.add(err)
			skipped++
		case ok:
			changed++
		default:
			skipped++
		}
		if (i+1)%stride == 0 || i+1 == total {
			s.jobs.Update(i+1, total, gerund)
		}
	}
	return changed, skipped, reasons, false
}

// skippedSuffix appends the shared ", skipped N" tail every tag-scope
// summary carries.
func skippedSuffix(summary string, skipped int) string {
	if skipped > 0 {
		summary += fmt.Sprintf(", skipped %d", skipped)
	}
	return summary
}

// batchTagCategoryPost moves every tag in scope to the posted category
// as a background job. merge=1 resolves (name, target) collisions by
// merging into the existing row; otherwise collisions are skipped and
// counted.
func (s *Server) batchTagCategoryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	catID, err := strconv.ParseInt(r.FormValue("category_id"), 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid category.")
		return
	}
	merge := r.FormValue("merge") == "1"
	s.startTagScopeRun(w, r, func(ids []int64) { s.runBatchTagCategory(ids, catID, merge) })
}

func (s *Server) runBatchTagCategory(ids []int64, catID int64, merge bool) {
	mergedCount := 0
	changed, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "moving tags…", 50, func(id int64) (bool, error) {
		// A tag already in the target moved nowhere; counting it as one
		// leaves a summary that reports work the catalog does not show.
		var current int64
		if err := s.db().Read.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&current); err == nil && current == catID {
			return false, nil
		}
		if !merge {
			if err := s.tagSvc().ChangeTagCategory(id, catID); err != nil {
				logx.Warnf("batch category tag %d: %v", id, err)
				return false, err
			}
			return true, nil
		}
		didMerge, err := s.tagSvc().ChangeTagCategoryMerge(id, catID)
		if err != nil {
			logx.Warnf("batch category tag %d: %v", id, err)
			return false, err
		}
		if didMerge {
			mergedCount++
		}
		return true, nil
	})

	s.Active().InvalidateCaches()
	summary := fmt.Sprintf("moved %d tag(s)", changed-mergedCount)
	if mergedCount > 0 {
		summary += fmt.Sprintf(", merged %d", mergedCount)
	}
	s.finishTagScopeJob(changed, reasons, cancelled, "category move", skippedSuffix(summary, skipped))
}

// batchTagAliasPost merges every tag in scope into one canonical (an
// alias row in scope repoints). The canonical input goes through the
// create-or-resolve path so a pending name works, like the alias dialog.
func (s *Server) batchTagAliasPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	canonID, msg := s.resolveCanonicalTagInput(r.FormValue("canonical_id"), true)
	if msg != "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return
	}
	s.startTagScopeRun(w, r, func(ids []int64) { s.runBatchTagAlias(ids, canonID) })
}

func (s *Server) runBatchTagAlias(ids []int64, canonID int64) {
	aliased, skipped, reasons, cancelled := s.runTagScopeLoop(ids, "aliasing tags…", 50, func(id int64) (bool, error) {
		if id == canonID {
			// The Merge dialog puts the chosen canonical in the scope
			// too; self-skipping it is not a refusal.
			return false, nil
		}
		if err := s.tagSvc().MergeTags(id, canonID); err != nil {
			logx.Warnf("batch alias tag %d: %v", id, err)
			return false, err
		}
		return true, nil
	})

	s.Active().InvalidateCaches()
	summary := skippedSuffix(fmt.Sprintf("aliased %d tag(s)", aliased), skipped)
	s.finishTagScopeJob(aliased, reasons, cancelled, "alias", summary)
}

// batchMergeFoldedPost merges each folded original in scope into its corrected
// spelling, resolving the target from folded_tag_pairs. Ambiguous originals and
// any whose pair no longer holds are skipped.
func (s *Server) batchMergeFoldedPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.startTagScopeRun(w, r, s.runMergeFolded)
}

func (s *Server) runMergeFolded(ids []int64) {
	s.jobs.Update(0, len(ids), "merging folded tags…")
	res, err := s.tagSvc().MergeFolded(s.jobs.Context(), ids)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	s.Active().InvalidateCaches()
	var reasons skipReasons
	for _, e := range res.Refused {
		reasons.add(e)
	}
	summary := skippedSuffix(fmt.Sprintf("merged %d folded tag(s)", res.Merged), res.Skipped)
	s.finishTagScopeJob(res.Merged, reasons, res.Cancelled, "folded merge", summary)
}

// batchTagImplyPost declares (mode=add) or removes (mode=remove) the
// "each tag in scope implies X" edge, with the image-side fan-out /
// sweep run inline inside the held job slot, mirroring the PTR sweep.
func (s *Server) batchTagImplyPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	remove := r.FormValue("mode") == "remove"
	targetID, msg := s.resolveCanonicalTagInput(r.FormValue("target"), !remove)
	if msg != "" {
		flashStatus(w, http.StatusBadRequest, msg)
		return
	}
	s.startTagScopeRun(w, r, func(ids []int64) { s.runBatchTagImply(ids, targetID, remove) })
}

func (s *Server) runBatchTagImply(ids []int64, targetID int64, remove bool) {
	ctx := s.jobs.Context()
	verb := "declaring implications…"
	if remove {
		verb = "removing implications…"
	}

	var removeClosure []int64
	if remove {
		var err error
		removeClosure, err = s.resolveRemoveClosure(targetID)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
	}

	changed, skipped, reasons, cancelled := s.runTagScopeLoop(ids, verb, 10, func(parentID int64) (bool, error) {
		if parentID == targetID {
			return false, nil
		}
		if remove {
			if err := s.tagSvc().RemoveImplication(parentID, targetID); err != nil {
				return false, err
			}
			if err := s.sweepImplicationRemovalInline(ctx, parentID, removeClosure); err != nil {
				logx.Warnf("batch imply sweep parent %d: %v", parentID, err)
			}
			return true, nil
		}
		created, err := s.tagSvc().AddImplicationFrom(parentID, targetID, "user")
		if err != nil {
			logx.Warnf("batch imply parent %d: %v", parentID, err)
			return false, err
		}
		if !created {
			return false, nil
		}
		if err := s.fanOutImplicationsInline(ctx, parentID); err != nil {
			logx.Warnf("batch imply fan-out parent %d: %v", parentID, err)
		}
		return true, nil
	})

	if err := s.tagSvc().RecalcIDs([]int64{targetID}); err != nil {
		logx.Warnf("batch imply recalc: %v", err)
	}
	s.Active().InvalidateCaches()
	noun := "declared"
	if remove {
		noun = "removed"
	}
	summary := skippedSuffix(fmt.Sprintf("%s %d implication(s)", noun, changed), skipped)
	s.finishTagScopeJob(changed, reasons, cancelled, "implication batch", summary)
}

// sweepImplicationRemovalInline drops the implied rows a removed edge no
// longer justifies on every image carrying parentID, chunked like the
// propagation job. closure is the removed target's transitive closure,
// target included, resolved once by the caller.
func (s *Server) sweepImplicationRemovalInline(ctx context.Context, parentID int64, closure []int64) error {
	return s.chunkImageTagsByParent(ctx, parentID, func(tx *sql.Tx, imageID int64) error {
		return propagateRemoveImplication(tx, imageID, closure)
	})
}
