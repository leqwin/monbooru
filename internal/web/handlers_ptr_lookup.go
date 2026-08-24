package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// ptrLookupBatch caps names per monloader graph query (its request limit).
const ptrLookupBatch = 500

// ptrLookupSearchPost sweeps every tag matching the current /tags filter
// through monloader's PTR graph and adds the aliases / implications it
// knows. Mirrors deleteTagsSearchPost: resolve the id set up front, run a
// background "tag" job, return 202 so the client watches the job status bar.
func (s *Server) ptrLookupSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.startTagScopeRun(w, r, s.runPTRTagLookup)
}

// ptrLookupTagPost runs the same sweep for one tag row. An `as` field
// names another spelling to ask the PTR under - the one the look-up dialog
// previewed - instead of the tag's own name.
func (s *Server) ptrLookupTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	as := strings.TrimSpace(r.FormValue("as"))
	if as != "" {
		form, ok := s.ptrSpellingForm(as)
		if !ok {
			http.Error(w, "not a tag name", http.StatusBadRequest)
			return
		}
		as = form
	}
	// merge takes the other direction: the looked-up spelling becomes the
	// canonical here and this tag an alias of it, so there is no `as` left to
	// ask under once the sweep runs.
	merge := r.FormValue("merge") != ""
	if merge && as == "" {
		http.Error(w, "not a tag name", http.StatusBadRequest)
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	if merge {
		go s.runPTRMergeSweep(id, as)
	} else {
		go s.runPTRTagSweep([]int64{id}, as)
	}
	w.WriteHeader(http.StatusAccepted)
}

// runPTRMergeSweep moves the tag onto the looked-up spelling and then sweeps
// what survives: the repository's name ends up the canonical here and the
// operator's an alias of it, so the cluster lands on the name the repository
// uses and the image pages contribute under it. Both halves share the one job
// slot the sweep already holds.
func (s *Server) runPTRMergeSweep(id int64, spelling string) {
	target, msg := s.resolveCanonicalTagInput(spelling, true)
	if msg != "" {
		s.jobs.Fail(msg)
		return
	}
	s.jobs.Update(0, 1, "aliasing tag…")
	if err := s.tagSvc().MergeTags(id, target); err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	s.Active().InvalidateCaches()
	s.runPTRTagSweep([]int64{target}, "")
}

// ptrSpellingForm validates an operator-typed spelling and returns it in
// monbooru form: the normalized bare name for general, category:name
// otherwise.
func (s *Server) ptrSpellingForm(input string) (string, bool) {
	input = strings.TrimSpace(input)
	catID, bare, ok := s.splitCategoryTag(input)
	if !ok {
		return "", false
	}
	norm, err := tags.ValidateTagName(bare)
	if err != nil {
		return "", false
	}
	if catID == s.Active().GeneralCategoryID {
		return norm, true
	}
	return input[:len(input)-len(bare)] + norm, true
}

// ptrLookupCand is one tag headed for monloader's PTR graph, with its name
// in monbooru form: bare for general, category:name otherwise.
type ptrLookupCand struct {
	id   int64
	name string
}

// ptrLookupCands resolves tag ids into sweep candidates, dropping alias rows
// and rating tags (aliases have a canonical that is itself swept; rating rows
// are immutable).
func (s *Server) ptrLookupCands(ids []int64) ([]ptrLookupCand, error) {
	var cands []ptrLookupCand
	for start := 0; start < len(ids); start += ptrLookupBatch {
		placeholders, args := db.InPlaceholders(ids[start:min(start+ptrLookupBatch, len(ids))])
		batch, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (ptrLookupCand, error) {
			var c ptrLookupCand
			var cat string
			err := rows.Scan(&c.id, &c.name, &cat)
			if cat != "general" {
				c.name = cat + ":" + c.name
			}
			return c, err
		},
			`SELECT t.id, t.name, c.name FROM tags t JOIN tag_categories c ON c.id = t.category_id
			 WHERE t.id IN (`+placeholders+`) AND t.is_alias = 0 AND c.name != 'rating'`, args...)
		if err != nil {
			return nil, err
		}
		cands = append(cands, batch...)
	}
	return cands, nil
}

// resolvePTRSpelling picks the spelling the PTR answers for a tag out of one
// graph answer: the tag's own form when known, else the first known alias
// pointing at it - an alias the PTR holds as an ideal first, then in the
// order given (name order). The operator's spelling need not be the
// repository's; the aliases say what else the tag is called.
//
// A spelling the index holds as an orphan answers known with nothing behind
// it, so a candidate carrying relations wins over one that does not wherever
// it turns up - the same preference monloader applies across its own
// candidate spellings. Without it the tag's own orphan name shadows an alias
// holding the whole cluster.
func resolvePTRSpelling(graph map[string]ptrTagInfo, own string, aliases []string) (string, ptrTagInfo, bool) {
	bareName, bareInfo, haveBare := "", ptrTagInfo{}, false
	connected := func(name string, info ptrTagInfo) bool {
		if len(info.Aliases)+len(info.Implications)+len(info.ImpliedBy) > 0 || (info.Ideal != "" && info.Ideal != name) {
			return true
		}
		if !haveBare {
			bareName, bareInfo, haveBare = name, info, true
		}
		return false
	}
	if info, ok := graph[own]; ok && info.Known && connected(own, info) {
		return own, info, true
	}
	var first string
	for _, a := range aliases {
		info, ok := graph[a]
		if !ok || !info.Known || !connected(a, info) {
			continue
		}
		if info.Ideal == a {
			return a, info, true
		}
		if first == "" {
			first = a
		}
	}
	if first != "" {
		return first, graph[first], true
	}
	if haveBare {
		return bareName, bareInfo, true
	}
	return "", ptrTagInfo{}, false
}

// ptrAliasForms lists the monbooru-form names of the aliases pointing at each
// tag, in name order.
func (s *Server) ptrAliasForms(ids []int64) (map[int64][]string, error) {
	aliases, err := s.tagSvc().AliasesForTagIDs(ids)
	if err != nil {
		return nil, err
	}
	forms := make(map[int64][]string, len(aliases))
	for id, rows := range aliases {
		for _, a := range rows {
			forms[id] = append(forms[id], tagFormName(a.CategoryName, a.Name))
		}
	}
	return forms, nil
}

// ptrResolveChunk answers the graph for a chunk of candidates, retrying the
// ones the PTR does not know under the aliases pointing at them. A tag
// answered through another spelling gets it appended to its alias list, so
// the pull adopts it and the staleness sync never retires it. Candidates the
// PTR knows under no spelling are absent from the result. A non-empty `as`
// (the look-up dialog's spelling, one candidate only) is asked instead of
// the candidate's own name, with no alias fallback: the operator named it.
func (s *Server) ptrResolveChunk(ctx context.Context, chunk []ptrLookupCand, as string) (map[int64]ptrTagInfo, error) {
	if as != "" && len(chunk) == 1 {
		graph, err := s.ptrTagLookup(ctx, []string{as})
		if err != nil {
			return nil, err
		}
		info := graph[as]
		if !info.Known {
			return map[int64]ptrTagInfo{}, nil
		}
		if as != chunk[0].name {
			info.Aliases = append(info.Aliases, as)
		}
		return map[int64]ptrTagInfo{chunk[0].id: info}, nil
	}
	names := make([]string, len(chunk))
	for i, c := range chunk {
		names[i] = c.name
	}
	graph, err := s.ptrTagLookup(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]ptrTagInfo, len(chunk))
	var unknown []int64
	for _, c := range chunk {
		if info := graph[c.name]; info.Known {
			out[c.id] = info
		} else {
			unknown = append(unknown, c.id)
		}
	}
	if len(unknown) == 0 {
		return out, nil
	}
	forms, err := s.ptrAliasForms(unknown)
	if err != nil {
		return nil, err
	}
	var all []string
	for _, id := range unknown {
		all = append(all, forms[id]...)
	}
	for start := 0; start < len(all); start += ptrLookupBatch {
		part, err := s.ptrTagLookup(ctx, all[start:min(start+ptrLookupBatch, len(all))])
		if err != nil {
			return nil, err
		}
		maps.Copy(graph, part)
	}
	for _, c := range chunk {
		if _, done := out[c.id]; done {
			continue
		}
		if spelling, info, ok := resolvePTRSpelling(graph, c.name, forms[c.id]); ok {
			info.Aliases = append(info.Aliases, spelling)
			out[c.id] = info
		}
	}
	return out, nil
}

// runPTRTagLookup queries monloader's PTR graph for each candidate tag in
// ptrLookupBatch-sized calls. Tags created by the implication fan-in are
// swept in follow-up rounds so their aliases and implications land in the
// same run; each id is swept at most once, so the rounds terminate.
func (s *Server) runPTRTagLookup(ids []int64) {
	s.runPTRTagSweep(ids, "")
}

// runPTRTagSweep is the sweep body; `as` is the look-up dialog's spelling
// for a single-tag pull, asked in place of that tag's own name on the seed
// round only.
func (s *Server) runPTRTagSweep(ids []int64, as string) {
	ctx := s.jobs.Context()
	lookedUpAs := as

	cands, err := s.ptrLookupCands(ids)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	// Implied-by parents are pulled only for the requested tags, never for tags
	// minted mid-sweep: a series' implied-by is every character that carries it,
	// so recursing would cascade the whole graph in.
	seed := make(map[int64]bool, len(cands))
	for _, c := range cands {
		seed[c.id] = true
	}

	total := len(cands)
	aliases, implications, unknown, processed, dropped, aliased, retired := 0, 0, 0, 0, 0, 0, 0
	cancelled := false
	unavailable := false
	impliedTouched := map[int64]struct{}{}
	fanOutParents := map[int64]struct{}{}
	swept := make(map[int64]struct{}, total)
	s.jobs.Update(0, total, "PTR lookup…")
	for len(cands) > 0 && !cancelled && !unavailable {
		createdTags := map[int64]struct{}{}
		for start := 0; start < len(cands) && !cancelled; start += ptrLookupBatch {
			if ctx.Err() != nil {
				cancelled = true
				break
			}
			chunk := cands[start:min(start+ptrLookupBatch, len(cands))]
			results, err := s.ptrResolveChunk(ctx, chunk, as)
			if errors.Is(err, errPTRUnavailable) {
				// The PTR going away mid-sweep is a degraded stop, not a
				// failure: keep what already applied and report how far it got,
				// like the batch lookup's per-image skip.
				unavailable = true
				break
			}
			if err != nil {
				s.jobs.Fail("PTR lookup failed: " + err.Error())
				return
			}
			for _, c := range chunk {
				swept[c.id] = struct{}{}
				info := results[c.id]
				if !info.Known {
					// A tag the PTR no longer knows has no fresh state left, so
					// every relation it pulled earlier is flagged.
					unknown++
					retired += s.syncPTRStaleness(c.id, nil, nil)
				} else {
					a, i, d, al, r := s.applyPTRTagInfo(c.id, info, impliedTouched, fanOutParents, createdTags, seed[c.id])
					aliases += a
					implications += i
					dropped += d
					aliased += al
					retired += r
				}
				processed++
			}
			s.jobs.Update(processed, total, "PTR lookup…")
		}
		if cancelled || unavailable {
			break
		}
		as = ""
		var next []int64
		for id := range createdTags {
			if _, ok := swept[id]; !ok {
				next = append(next, id)
			}
		}
		if cands, err = s.ptrLookupCands(next); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		total += len(cands)
	}

	// Backfill the new edges onto images already carrying the parents. This
	// runs inside the sweep's own job because startImplicationPropagation
	// can't start one while the runner is held; one pass per parent covers
	// all its new edges (the fan-out applies the full implied closure).
	for parentID := range fanOutParents {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if err := s.fanOutImplicationsInline(ctx, parentID); err != nil {
			logx.Warnf("ptr lookup fan-out for tag %d: %v", parentID, err)
		}
	}
	if len(impliedTouched) > 0 {
		touched := make([]int64, 0, len(impliedTouched))
		for id := range impliedTouched {
			touched = append(touched, id)
		}
		if err := s.tagSvc().RecalcIDs(touched); err != nil {
			logx.Warnf("ptr lookup recalc: %v", err)
		}
	}
	s.Active().InvalidateCaches()

	msg := fmt.Sprintf("PTR: added %d alias(es) and %d implication(s) across %d tag(s)", aliases, implications, processed)
	if lookedUpAs != "" {
		msg += ", looked up as " + lookedUpAs
	}
	if unknown > 0 {
		msg += fmt.Sprintf("; %d unknown to the PTR", unknown)
	}
	if dropped > 0 {
		msg += fmt.Sprintf("; %d spelling(s) not representable", dropped)
	}
	if aliased > 0 {
		msg += fmt.Sprintf("; %d relation(s) skipped, an alias here points elsewhere", aliased)
	}
	if retired > 0 {
		msg += fmt.Sprintf("; %d relation(s) no longer on the PTR", retired)
	}
	msg += "."
	if unavailable && !cancelled {
		s.jobs.Complete(fmt.Sprintf("PTR lookup stopped at %d/%d: the PTR became unavailable on monloader. %s", processed, total, msg))
		return
	}
	s.finishJob(nil, cancelled, fmt.Sprintf("PTR lookup cancelled (%d/%d processed)", processed, total), msg)
}

// applyPTRTagInfo adds the PTR's aliases and implications for one canonical
// tag. The PTR ideal counts as an alias too: locally this tag stays the
// canonical, so the repository's preferred spelling lands here inverted. An
// alias is created only when its name is unused in its category, so the
// operator's catalog always wins over the PTR's preferred spelling.
// Implication edges rely on AddImplication's cycle and alias guards and are
// logged and skipped on failure so one bad edge can't abort the sweep; any
// tag GetOrCreateTag had to create lands in createdTags for the caller to
// sweep too. The names the answer carried form the fresh state the tag's
// earlier PTR pulls are reconciled against: relations no longer listed are
// flagged stale, not removed. When pullImpliedBy is set the reverse edges
// (parents that imply this tag) are declared too, but those parents are only
// created and linked, never swept - their own relations stay for a pull from
// their page, so one tag's pull cannot cascade the whole cluster in.
func (s *Server) applyPTRTagInfo(tagID int64, info ptrTagInfo, impliedTouched, fanOutParents, createdTags map[int64]struct{}, pullImpliedBy bool) (aliases, implications, dropped, aliased, retired int) {
	freshAliases := map[tags.AliasKey]bool{}
	freshImplied := map[int64]bool{}
	aliasNames := info.Aliases
	if info.Ideal != "" {
		aliasNames = append([]string{info.Ideal}, aliasNames...)
	}
	for _, name := range aliasNames {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			dropped++
			continue
		}
		normalized, err := tags.ValidateTagName(bare)
		if err != nil {
			dropped++
			continue
		}
		freshAliases[tags.AliasKey{CategoryID: catID, Name: normalized}] = true
		exists, err := s.tagNameExists(catID, normalized)
		if err != nil {
			logx.Warnf("ptr alias %q: %v", name, err)
			continue
		}
		if exists {
			continue
		}
		if _, err := s.tagSvc().CreateAliasFrom(normalized, catID, tagID, "ptr"); err != nil {
			logx.Warnf("ptr alias %q: %v", name, err)
			continue
		}
		aliases++
	}
	for _, name := range info.Implications {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			continue
		}
		normalized, err := tags.ValidateTagName(bare)
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		exists, err := s.tagNameExists(catID, normalized)
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		implied, err := s.tagSvc().GetOrCreateTagFrom(normalized, catID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if redirected(implied, normalized, catID) {
			aliased++
			continue
		}
		freshImplied[implied.ID] = true
		if !exists {
			createdTags[implied.ID] = struct{}{}
		}
		isNew, err := s.tagSvc().AddImplicationFrom(tagID, implied.ID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if isNew {
			implications++
			impliedTouched[implied.ID] = struct{}{}
			fanOutParents[tagID] = struct{}{}
		}
	}
	if pullImpliedBy {
		for _, name := range info.ImpliedBy {
			catID, bare, ok := s.splitCategoryTag(name)
			if !ok {
				continue
			}
			normalized, err := tags.ValidateTagName(bare)
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			// A reverse-edge parent is linked but never swept: its own
			// implications point at siblings of this tag (solo_futanari implies
			// futanari), and following them would drag the whole surrounding
			// cluster into the catalog. Pull the parent's graph from its page.
			parent, err := s.tagSvc().GetOrCreateTagFrom(normalized, catID, "ptr")
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			if redirected(parent, normalized, catID) {
				aliased++
				continue
			}
			isNew, err := s.tagSvc().AddImplicationFrom(parent.ID, tagID, "ptr")
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			if isNew {
				implications++
				impliedTouched[tagID] = struct{}{}
				fanOutParents[parent.ID] = struct{}{}
			}
		}
	}
	retired = s.syncPTRStaleness(tagID, freshAliases, freshImplied)
	return aliases, implications, dropped, aliased, retired
}

// redirected reports that GetOrCreateTag handed back a different tag than the
// name asked for, because that name is an alias here. The catalog says the two
// are the same tag; the PTR only ever spoke about the aliased spelling. Storing
// the relation against the canonical would assert an edge neither source
// declares - and the contribution dialog would then keep offering that edge
// back to the PTR as new.
func redirected(got *models.Tag, name string, categoryID int64) bool {
	return got.Name != name || got.CategoryID != categoryID
}

// syncPTRStaleness reconciles a tag's pulled relations against the fresh
// answer (nil maps mean the PTR listed nothing), returning how many were
// newly flagged. Errors are logged and skipped like the apply loops - a
// failed flag pass must not abort the sweep.
func (s *Server) syncPTRStaleness(tagID int64, freshAliases map[tags.AliasKey]bool, freshImplied map[int64]bool) int {
	retired := 0
	n, err := s.tagSvc().SyncAliasStaleness(tagID, "ptr", freshAliases)
	if err != nil {
		logx.Warnf("ptr alias staleness for tag %d: %v", tagID, err)
	}
	retired += n
	n, err = s.tagSvc().SyncImplicationStaleness(tagID, "ptr", freshImplied)
	if err != nil {
		logx.Warnf("ptr implication staleness for tag %d: %v", tagID, err)
	}
	return retired + n
}

// tagNameExists reports whether a (category, name) tag row exists,
// alias rows included.
func (s *Server) tagNameExists(catID int64, name string) (bool, error) {
	var n int
	if err := s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE name = ? AND category_id = ?`, name, catID,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// splitCategoryTag resolves a monbooru-form name ("bare" or "category:name")
// to its category id, treating an unknown prefix as part of a general-
// category name, matching the tag-input parser.
func (s *Server) splitCategoryTag(input string) (catID int64, bare string, ok bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, "", false
	}
	if idx := strings.Index(input, ":"); idx > 0 {
		if name := input[idx+1:]; name != "" {
			var id int64
			if err := s.db().Read.QueryRow(
				`SELECT id FROM tag_categories WHERE name = ?`, input[:idx],
			).Scan(&id); err == nil {
				return id, name, true
			}
		}
	}
	cx := s.Active()
	if cx == nil || cx.GeneralCategoryID == 0 {
		return 0, "", false
	}
	return cx.GeneralCategoryID, input, true
}

// fanOutImplicationsInline backfills a parent's implied closure onto every
// image already carrying it, chunked like the propagation job.
func (s *Server) fanOutImplicationsInline(ctx context.Context, parentID int64) error {
	ratingCatID := s.tagSvc().RatingCategoryID()
	return s.chunkImageTagsByParent(ctx, parentID, func(tx *sql.Tx, imageID int64) error {
		return propagateAddImplication(tx, imageID, parentID, ratingCatID)
	})
}
