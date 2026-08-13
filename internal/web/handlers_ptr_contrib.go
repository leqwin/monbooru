package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// imageShaType reads one image's sha256 and file type; ok=false when
// the row is missing.
func (s *Server) imageShaType(id int64) (sha, fileType string, ok bool) {
	err := s.db().Read.QueryRow(`SELECT sha256, file_type FROM images WHERE id = ?`, id).Scan(&sha, &fileType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	return sha, fileType, err == nil
}

// The image-page PTR contribution panel: act (tick items, write reasons)
// then send (the confirm), both inside monbooru. Every surface here
// gates on the cached contrib flag; a stale flag degrades in place on a
// 409 from monloader, like the lookup controls.

// contribPreviewToAdd mirrors monloader's preview to_add entry. Color is
// monbooru's own category color for the row, filled at render time.
type contribPreviewToAdd struct {
	Tag    string `json:"tag"`
	PTR    string `json:"ptr"`
	Status string `json:"status"`
	Note   string `json:"note"`
	Color  string `json:"-"`
}

// PTRDiffers reports whether the PTR spelling differs beyond the
// mechanical underscore/space mapping, so the dialog only calls out
// real renames.
func (t contribPreviewToAdd) PTRDiffers() bool {
	return strings.ReplaceAll(t.Tag, "_", " ") != t.PTR
}

// contribPreviewPTROnly mirrors monloader's ptr_only entry. Color is
// monbooru's own category color for the row, filled at render time.
type contribPreviewPTROnly struct {
	Tag          string `json:"tag"`
	PTR          string `json:"ptr"`
	Petitionable bool   `json:"petitionable"`
	Color        string `json:"-"`
}

// contribPreview is monloader's preview response.
type contribPreview struct {
	Provisional bool                    `json:"provisional"`
	ToAdd       []contribPreviewToAdd   `json:"to_add"`
	PTROnly     []contribPreviewPTROnly `json:"ptr_only"`
}

// monloaderPostJSON marshals payload, POSTs it through
// monloaderContribJSON, and decodes the reply into a T. A free function
// because methods can't be generic.
func monloaderPostJSON[T any](s *Server, ctx context.Context, path string, payload map[string]any) (*T, error) {
	body, _ := json.Marshal(payload)
	var out T
	if err := s.monloaderContribJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// monloaderContribPreview proxies POST /api/v1/ptr/contrib/preview for
// one image's sha256, its storage tags, and its implied tags (context
// only: never offered as adds, but they keep tags the image already
// shows out of the removal-petition candidates).
func (s *Server) monloaderContribPreview(ctx context.Context, sha256 string, tags, implied []string) (*contribPreview, error) {
	return monloaderPostJSON[contribPreview](s, ctx, "/api/v1/ptr/contrib/preview",
		map[string]any{"sha256": sha256, "tags": tags, "implied": implied})
}

// contribSendResult is one item's verdict from the send.
type contribSendResult struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Note   string `json:"note"`
}

// contribSendResponse is monloader's stage-and-commit answer.
type contribSendResponse struct {
	Results []contribSendResult `json:"results"`
	JobID   int64               `json:"job_id"`
}

// monloaderContribSend proxies POST /api/v1/ptr/contrib with commit: true.
func (s *Server) monloaderContribSend(ctx context.Context, origin string, items []map[string]any) (*contribSendResponse, error) {
	return monloaderPostJSON[contribSendResponse](s, ctx, "/api/v1/ptr/contrib",
		map[string]any{"commit": true, "origin": origin, "items": items})
}

// monloaderContribJSON issues one authed request to monloader and
// decodes a JSON reply, mapping a 409 to errPTRUnavailable so callers
// collapse the surface in place.
func (s *Server) monloaderContribJSON(ctx context.Context, method, path string, body []byte, out any) error {
	resp, err := s.monloaderDo(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return errPTRUnavailable
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("monloader returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// tagFormName renders a tag in monbooru form: bare for general,
// category-qualified otherwise.
func tagFormName(cat, name string) string {
	if cat != "general" {
		return cat + ":" + name
	}
	return name
}

// contribTagColor returns a category-color resolver for monbooru-form tag
// names: the prefix's category when it names one, general otherwise.
func (s *Server) contribTagColor() func(string) string {
	colors := map[string]string{}
	if cats, err := s.tagSvc().ListCategories(); err == nil {
		for _, c := range cats {
			colors[c.Name] = c.Color
		}
	}
	return func(tag string) string {
		if i := strings.Index(tag, ":"); i > 0 {
			if color, ok := colors[tag[:i]]; ok {
				return color
			}
		}
		return colors["general"]
	}
}

// contribStorageTags splits an image's tags for the preview: storage
// tags are eligible adds (non-implied, non-alias, in monbooru form);
// implied and rating tags ride along as context so the diff knows what
// the image already shows without offering to add it. Rating tags are
// never adds (monloader refuses the namespace) but as context they keep
// the PTR's copy of the rating out of the removal-petition candidates.
func (s *Server) contribStorageTags(imageTags []models.ImageTag) (storage, implied []string) {
	for _, t := range imageTags {
		if t.IsImplied || t.Category == "rating" {
			implied = append(implied, tagFormName(t.Category, t.TagName))
			continue
		}
		storage = append(storage, tagFormName(t.Category, t.TagName))
	}
	return storage, implied
}

// imageContribPreview fetches the contribution diff for one image's
// current tags under the surfaces' shared 8 s budget. Runs after the
// caller's gate and existence checks; the panel and dialog keep their
// own divergent failure responses.
func (s *Server) imageContribPreview(rctx context.Context, id int64, sha string) (*contribPreview, error) {
	_, imageTags, _ := s.tagSvc().GetImageTags(id)
	storage, implied := s.contribStorageTags(imageTags)
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	return s.monloaderContribPreview(ctx, sha, storage, implied)
}

// ptrUnattributed lists the image's tags the repository also holds but
// that the local ledger does not record it as having applied. Carrying
// a tag is not the same as having it from the repository: one a booru
// supplied leaves the repository absent from the by-source view (§5.8)
// even though it vouches for the tag too, and a pull is what records
// that. So these are work to offer even though no tag would be added.
func (s *Server) ptrUnattributed(id int64, preview *contribPreview) []string {
	known := map[string]bool{}
	for _, t := range preview.ToAdd {
		if t.Status == "known" {
			known[t.Tag] = true
		}
	}
	if len(known) == 0 {
		return nil
	}
	ledger, err := s.tagSvc().TagSourcesForImage(id)
	if err != nil {
		return nil
	}
	_, imageTags, _ := s.tagSvc().GetImageTags(id)
	var out []string
	for _, t := range imageTags {
		name := tagFormName(t.Category, t.TagName)
		if !known[name] || slices.ContainsFunc(ledger[t.TagID], isPTRSource) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func isPTRSource(src tags.TagSource) bool {
	return strings.EqualFold(src.Source, "ptr")
}

// ptrContribPanel renders the image-page panel body: a one-line summary
// from one preview, or the zero-work / provisional state. Absent when
// the gate is closed or the row is a cbz bundle, so the caller only
// mounts it when eligible.
func (s *Server) ptrContribPanel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.contribReadOpen() {
		w.WriteHeader(http.StatusOK)
		return
	}
	sha, fileType, ok := s.imageShaType(id)
	if !ok || fileType == models.FileTypeCBZ {
		w.WriteHeader(http.StatusOK)
		return
	}
	preview, err := s.imageContribPreview(r.Context(), id, sha)
	if err != nil {
		// A 409 or a transport hiccup collapses the panel in place.
		w.WriteHeader(http.StatusOK)
		return
	}
	var newTags, petitionTags []string
	hasKnown := false
	for _, t := range preview.ToAdd {
		switch t.Status {
		case "new":
			newTags = append(newTags, t.Tag)
		case "known":
			hasKnown = true
		}
	}
	for _, p := range preview.PTROnly {
		if p.Petitionable {
			petitionTags = append(petitionTags, p.Tag)
		}
	}
	canContribute, contribHint := s.contribHint()
	unattributed := s.ptrUnattributed(id, preview)
	s.renderTemplate(w, "partials/ptr_contrib_panel.html", map[string]any{
		"ImageID":           id,
		"NewCount":          len(newTags),
		"PetitionCount":     len(petitionTags),
		"AllKnown":          hasKnown,
		"NewTip":            strings.Join(newTags, "\n"),
		"PetitionTip":       strings.Join(petitionTags, "\n"),
		"UnattributedCount": len(unattributed),
		"UnattributedTip":   strings.Join(unattributed, "\n"),
		"CanContribute":     canContribute,
		// A pull is worth offering when the repository holds tags this
		// image lacks, and also when it merely holds tags the ledger has
		// not credited it for - that is the attribution the pull writes.
		"CanPull":       s.ptrPullOpen() && (len(petitionTags) > 0 || len(unattributed) > 0),
		"ContribHint":   contribHint,
		"Provisional":   preview.Provisional,
		"FailedUploads": s.monloaderContribFailedSeed(),
		"Monloader":     s.monloaderWebBase(),
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
	})
}

// ptrContribDialog renders the contribute dialog, populated from one
// preview.
func (s *Server) ptrContribDialog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.contribGateOpen() {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	sha, fileType, ok := s.imageShaType(id)
	if !ok || fileType == models.FileTypeCBZ {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	preview, err := s.imageContribPreview(r.Context(), id, sha)
	if err != nil {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	color := s.contribTagColor()
	var actionable, known, queued, ineligible []contribPreviewToAdd
	for _, t := range preview.ToAdd {
		t.Color = color(t.Tag)
		switch t.Status {
		case "new":
			actionable = append(actionable, t)
		case "known":
			known = append(known, t)
		case "unsent":
			queued = append(queued, t)
		default:
			ineligible = append(ineligible, t)
		}
	}
	var petitionable, petitioned []contribPreviewPTROnly
	for _, p := range preview.PTROnly {
		p.Color = color(p.Tag)
		if p.Petitionable {
			petitionable = append(petitionable, p)
		} else {
			petitioned = append(petitioned, p)
		}
	}
	s.renderTemplate(w, "partials/ptr_contrib_dialog.html", map[string]any{
		"ImageID":      id,
		"SHA256":       sha,
		"Actionable":   actionable,
		"Known":        known,
		"Queued":       queued,
		"Ineligible":   ineligible,
		"Petitionable": petitionable,
		"Petitioned":   petitioned,
		"Provisional":  preview.Provisional,
		"CSRFToken":    s.csrfToken(sessionFromContext(r.Context())),
	})
}

// ptrContribSend handles the dialog confirm: one stage-and-commit call
// carrying the ticked adds and any petitions with their reason.
func (s *Server) ptrContribSend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.contribGateOpen() {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	sha, fileType, ok := s.imageShaType(id)
	if !ok || fileType == models.FileTypeCBZ {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// The dialog bakes in the sha it previewed; if the row now resolves to a
	// different file (a gallery switch under the lock-free route) the ticked
	// tags belong to another image, so refuse rather than contribute them.
	if want := r.FormValue("sha256"); want != "" && want != sha {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "this image changed; reopen the dialog"})
		return
	}
	petitionReason := strings.TrimSpace(r.FormValue("petition_reason"))
	if len(r.Form["petition"]) > 0 && petitionReason == "" {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "a reason is required to petition a removal"})
		return
	}
	var items []map[string]any
	for _, tag := range r.Form["add"] {
		items = append(items, map[string]any{"kind": "mapping_add", "sha256": sha, "tag": tag})
	}
	for _, tag := range r.Form["petition"] {
		items = append(items, map[string]any{
			"kind": "mapping_petition", "sha256": sha, "tag": tag, "reason": petitionReason,
		})
	}
	if len(items) == 0 {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "nothing selected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.monloaderContribSend(ctx, "image "+strconv.FormatInt(id, 10), items)
	if err != nil {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "contributions unavailable"})
		return
	}
	s.renderContribReceipt(w, items, resp)
}

// renderContribReceipt swaps the dialog body for the send receipt: the
// HX-Retarget replaces the whole form so the pick-and-send controls give
// way to the per-item verdicts and a close.
func (s *Server) renderContribReceipt(w http.ResponseWriter, items []map[string]any, resp *contribSendResponse) {
	// The receipt zip relies on monloader answering every item in order;
	// a mismatched count would pair tags with the wrong verdicts, so it
	// surfaces as a failure instead of a silently truncated receipt.
	if len(resp.Results) != len(items) {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{
			"Err": fmt.Sprintf("monloader answered %d of %d items; check its contribution history", len(resp.Results), len(items)),
		})
		return
	}
	sent, refused := 0, 0
	for _, res := range resp.Results {
		if res.Result == "staged" {
			sent++
		} else {
			refused++
		}
	}
	adds, petitions, refusedRows := contribReceipt(items, resp.Results)
	w.Header().Set("HX-Retarget", "#ptr-contrib-form")
	w.Header().Set("HX-Reswap", "innerHTML")
	s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{
		"Sent":            sent,
		"Refused":         refused,
		"StagedAdds":      adds,
		"StagedPetitions": petitions,
		"RefusedRows":     refusedRows,
		"JobID":           resp.JobID,
		"Monloader":       s.monloaderWebBase(),
	})
}

// contribReceiptRow is one named verdict on the send receipt.
type contribReceiptRow struct {
	Label string
	Note  string
}

// contribReceipt zips the sent items with monloader's per-item results
// (answered in item order) into named receipt rows.
func contribReceipt(items []map[string]any, results []contribSendResult) (adds, petitions, refused []contribReceiptRow) {
	for i := 0; i < min(len(items), len(results)); i++ {
		kind, _ := items[i]["kind"].(string)
		var label string
		switch {
		case strings.HasPrefix(kind, "mapping"):
			label, _ = items[i]["tag"].(string)
		case strings.HasPrefix(kind, "sibling"):
			label = fmt.Sprintf("%v -> %v", items[i]["bad"], items[i]["good"])
		default:
			label = fmt.Sprintf("%v => %v", items[i]["child"], items[i]["parent"])
		}
		row := contribReceiptRow{Label: label}
		switch {
		case results[i].Result != "staged":
			row.Note = results[i].Result
			if results[i].Note != "" {
				row.Note += " - " + results[i].Note
			}
			refused = append(refused, row)
		case strings.Contains(kind, "petition"):
			petitions = append(petitions, row)
		default:
			adds = append(adds, row)
		}
	}
	return adds, petitions, refused
}

// contribGateOpen reports whether the contribution surfaces may render:
// paired, PTR enabled, and a usable personal account.
func (s *Server) contribGateOpen() bool {
	if !s.pairedWith("monloader") {
		return false
	}
	_, _, ptrReady, _, contrib := s.monloaderStatusSeed()
	return ptrReady && contrib
}

// contribReadOpen reports whether the read-only diff may render: paired and
// the PTR index enabled. A still-building index answers diffs (marked
// provisional), so syncing counts; contributing additionally needs a synced
// index and a personal account (contribGateOpen).
func (s *Server) contribReadOpen() bool {
	if !s.pairedWith("monloader") {
		return false
	}
	_, _, ptrReady, ptrSyncing, _ := s.monloaderStatusSeed()
	return ptrReady || ptrSyncing
}

// ptrPullOpen reports whether the PTR pull actions may render: they ride the
// lookup path, which monloader refuses until the index is caught up.
func (s *Server) ptrPullOpen() bool {
	_, _, ptrReady, _, _ := s.monloaderStatusSeed()
	return ptrReady
}

// contribHint reports whether the panel's Contribute button may act and,
// when it cannot, why. The diff and Pull stay live either way.
func (s *Server) contribHint() (bool, string) {
	_, _, _, ptrSyncing, ptrContrib := s.monloaderStatusSeed()
	switch {
	case ptrContrib:
		return true, ""
	case ptrSyncing:
		return false, "the Public Tag Repository is still syncing"
	case s.monloaderContribBannedSeed():
		return false, "the contribution account is banned"
	default:
		return false, "a contribution account in monloader is required to contribute"
	}
}

// pairPreview mirrors monloader's pair-preview response.
type pairPreview struct {
	APtr        string `json:"a_ptr"`
	BPtr        string `json:"b_ptr"`
	Direction   string `json:"direction"`
	Note        string `json:"note"`
	Provisional bool   `json:"provisional"`
}

// monloaderPairPreview proxies the relation pair-preview.
func (s *Server) monloaderPairPreview(ctx context.Context, kind, a, b string) (*pairPreview, error) {
	return monloaderPostJSON[pairPreview](s, ctx, "/api/v1/ptr/contrib/pair-preview",
		map[string]any{"kind": kind, "a": a, "b": b})
}

// tagPairRow is one relation row in the tag-page contribution surfaces.
// Sibling pairs carry a=bad (alias), b=good (canonical); parent pairs
// carry a=child (the carrying tag), b=parent (the implied tag) - the
// hydrus child/parent flip is owned here.
type tagPairRow struct {
	Kind           string // sibling | parent
	A, B           string // monbooru form
	AColor, BColor string // category colors, filled at render time
	Rel            string // alias of this | this aliased to | implies | implied by
	Direction      string // suggest | petition | pending | conflict | ineligible
	Note           string
}

// Value encodes a row for the dialog form. Space is the delimiter
// because it is reserved out of the tag charset, unlike '|'.
func (p tagPairRow) Value() string { return p.Kind + " " + p.A + " " + p.B }

// Label renders the pair for a row: -> reads "resolves to", => "implies".
func (p tagPairRow) Label() string {
	if p.Kind == "sibling" {
		return p.A + " -> " + p.B
	}
	return p.A + " => " + p.B
}

// tagContribRows assembles the tag's contribution diff: its declared
// relations with their pair-preview direction, and the PTR-side
// relations monbooru does not declare (removal-petition candidates).
// pullable reports whether a pull would still add a relation, so the pull
// control can hide once nothing is left to adopt.
func (s *Server) tagContribRows(ctx context.Context, id int64, tag *models.Tag) (local, ptrOnly []tagPairRow, pullable, provisional bool, err error) {
	tagForm := tagFormName(tag.CategoryName, tag.Name)
	aliases, err := s.tagSvc().AliasesForTagIDs([]int64{id})
	if err != nil {
		return nil, nil, false, false, err
	}
	implications, err := s.tagSvc().ListImplications(id)
	if err != nil {
		return nil, nil, false, false, err
	}
	impliedBy, err := s.tagSvc().ImpliedBy(id)
	if err != nil {
		return nil, nil, false, false, err
	}
	localAlias, localImplied, localImpliedBy := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, a := range aliases[id] {
		local = append(local, tagPairRow{Kind: "sibling", A: tagFormName(a.CategoryName, a.Name), B: tagForm, Rel: "alias of this"})
		localAlias[tagFormName(a.CategoryName, a.Name)] = true
	}
	for _, im := range impliedBy {
		local = append(local, tagPairRow{Kind: "parent", A: tagFormName(im.ParentCategoryName, im.ParentName), B: tagForm, Rel: "implied by"})
		localImpliedBy[tagFormName(im.ParentCategoryName, im.ParentName)] = true
	}
	for _, im := range implications {
		local = append(local, tagPairRow{Kind: "parent", A: tagForm, B: tagFormName(im.ImpliedCategoryName, im.ImpliedName), Rel: "implies"})
		localImplied[tagFormName(im.ImpliedCategoryName, im.ImpliedName)] = true
	}
	for i := range local {
		preview, err := s.monloaderPairPreview(ctx, local[i].Kind, local[i].A, local[i].B)
		if err != nil {
			return nil, nil, false, false, err
		}
		local[i].Direction, local[i].Note = preview.Direction, preview.Note
		provisional = provisional || preview.Provisional
	}
	// The PTR side: relations of this tag that monbooru does not declare,
	// limited to the directions a pull can adopt - fan-in aliases, the
	// ideal this tag resolves to, and implications out. Implied-by edges
	// are left out: a pull never writes the reverse implication onto the
	// parent, so offering them is a dead action. Only pairs the index
	// actually holds qualify (the preview answers petition or pending);
	// anything else is graph noise from a sibling chain and is dropped.
	graph, err := s.ptrTagLookup(ctx, []string{tagForm})
	if err != nil {
		return nil, nil, false, false, err
	}
	if info, ok := graph[tagForm]; ok && info.Known {
		pullable = s.pullWouldAdd(info, tagForm, localImplied, localImpliedBy)
		var candidates []tagPairRow
		for _, a := range info.Aliases {
			if a == "" || a == tagForm || localAlias[a] {
				continue
			}
			candidates = append(candidates, tagPairRow{Kind: "sibling", A: a, B: tagForm, Rel: "alias of this"})
		}
		// A pull inverts the PTR's orientation - the ideal lands here as an
		// alias of this tag - so an ideal already declared locally is not a
		// missing relation.
		if info.Ideal != "" && info.Ideal != tagForm && !localAlias[info.Ideal] {
			candidates = append(candidates, tagPairRow{Kind: "sibling", A: tagForm, B: info.Ideal, Rel: "this aliased to"})
		}
		for _, im := range info.Implications {
			if im == "" || im == tagForm || localImplied[im] {
				continue
			}
			candidates = append(candidates, tagPairRow{Kind: "parent", A: tagForm, B: im, Rel: "implies"})
		}
		for _, row := range candidates {
			// A petition needs a local judgement about the pair; a relation
			// whose other endpoint the catalog has never seen is not one the
			// operator can vouch against, so it is not offered at all.
			other := row.A
			if other == tagForm {
				other = row.B
			}
			if !s.tagFormExists(other) {
				continue
			}
			preview, err := s.monloaderPairPreview(ctx, row.Kind, row.A, row.B)
			if err != nil {
				return nil, nil, false, false, err
			}
			if preview.Direction != "petition" && preview.Direction != "pending" {
				continue
			}
			row.Direction, row.Note = preview.Direction, preview.Note
			ptrOnly = append(ptrOnly, row)
		}
	}
	return local, ptrOnly, pullable, provisional, nil
}

// pullWouldAdd reports whether a PTR pull would still create a relation for the
// tag: an alias whose name is free (a pull never overwrites an existing tag),
// the ideal, or an implies / implied-by edge not yet declared against a tag the
// catalog does not alias elsewhere. Mirrors the create conditions in
// applyPTRTagInfo so the pull control tracks real work - a condition missing
// here leaves the control offering a pull that would adopt nothing, for good.
func (s *Server) pullWouldAdd(info ptrTagInfo, tagForm string, localImplied, localImpliedBy map[string]bool) bool {
	aliasFree := func(name string) bool {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			return false
		}
		norm, err := tags.ValidateTagName(bare)
		if err != nil {
			return false
		}
		exists, err := s.tagNameExists(catID, norm)
		return err == nil && !exists
	}
	for _, a := range info.Aliases {
		if a != "" && a != tagForm && aliasFree(a) {
			return true
		}
	}
	if info.Ideal != "" && info.Ideal != tagForm && aliasFree(info.Ideal) {
		return true
	}
	for _, im := range info.Implications {
		if im != "" && im != tagForm && !localImplied[im] && !s.aliasedElsewhere(im) {
			return true
		}
	}
	for _, im := range info.ImpliedBy {
		if im != "" && im != tagForm && !localImpliedBy[im] && !s.aliasedElsewhere(im) {
			return true
		}
	}
	return false
}

// aliasedElsewhere reports that the name is an alias row here, which is what
// makes applyPTRTagInfo skip a relation naming it: GetOrCreateTag redirects an
// alias to its canonical, and the edge stored there is one neither the PTR nor
// the alias declares.
func (s *Server) aliasedElsewhere(name string) bool {
	catID, bare, ok := s.splitCategoryTag(name)
	if !ok {
		return false
	}
	norm, err := tags.ValidateTagName(bare)
	if err != nil {
		return false
	}
	var isAlias int
	if err := s.db().Read.QueryRow(
		`SELECT is_alias FROM tags WHERE name = ? AND category_id = ?`, norm, catID,
	).Scan(&isAlias); err != nil {
		return false
	}
	return isAlias == 1
}

// tagFormExists reports whether a monbooru-form tag names a row that
// exists in the catalog (alias rows included).
func (s *Server) tagFormExists(form string) bool {
	catID, bare, ok := s.splitCategoryTag(form)
	if !ok {
		return false
	}
	exists, err := s.tagNameExists(catID, bare)
	return err == nil && exists
}

// contribTag answers the tag row for the contribution surfaces; ok=false
// when the row is missing or never eligible (alias, rating).
func (s *Server) contribTag(id int64) (*models.Tag, bool) {
	tag, err := s.tagSvc().GetTag(id)
	if err != nil || tag.IsAlias || tag.CategoryName == "rating" {
		return nil, false
	}
	return tag, true
}

// tagContribPreview fetches the tag's relation diff under the surfaces'
// shared 8 s budget. Runs after the caller's gate and eligibility
// checks; the panel and dialog keep their own divergent failure
// responses.
func (s *Server) tagContribPreview(rctx context.Context, id int64, tag *models.Tag) (local, ptrOnly []tagPairRow, pullable, provisional bool, err error) {
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	return s.tagContribRows(ctx, id, tag)
}

// tagPtrContribPanel renders the tag-page contribution card body: a
// one-line summary of what the relation diff offers. Empty when the
// gate is closed or the tag is not eligible, so the caller only mounts
// it when eligible.
func (s *Server) tagPtrContribPanel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.contribReadOpen() {
		w.WriteHeader(http.StatusOK)
		return
	}
	tag, ok := s.contribTag(id)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	local, ptrOnly, pullable, provisional, err := s.tagContribPreview(r.Context(), id, tag)
	if err != nil {
		// A 409 or a transport hiccup collapses the panel in place.
		w.WriteHeader(http.StatusOK)
		return
	}
	var newTags, petitionTags []string
	for _, p := range local {
		if p.Direction == "suggest" {
			newTags = append(newTags, p.Label())
		}
	}
	for _, p := range ptrOnly {
		if p.Direction == "petition" {
			petitionTags = append(petitionTags, p.Label())
		}
	}
	canContribute, contribHint := s.contribHint()
	s.renderTemplate(w, "partials/tag_ptr_contrib_panel.html", map[string]any{
		"TagID":         id,
		"NewCount":      len(newTags),
		"PetitionCount": len(petitionTags),
		"Pullable":      pullable && s.ptrPullOpen(),
		"NewTip":        strings.Join(newTags, "\n"),
		"PetitionTip":   strings.Join(petitionTags, "\n"),
		"CanContribute": canContribute,
		"ContribHint":   contribHint,
		"Provisional":   provisional,
		"FailedUploads": s.monloaderContribFailedSeed(),
		"Monloader":     s.monloaderWebBase(),
	})
}

// tagPtrContribDialog renders the tag contribute dialog, populated from
// the relation diff.
func (s *Server) tagPtrContribDialog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.contribGateOpen() {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	tag, ok := s.contribTag(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	local, ptrOnly, _, provisional, err := s.tagContribPreview(r.Context(), id, tag)
	if err != nil {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	color := s.contribTagColor()
	paint := func(p tagPairRow) tagPairRow {
		p.AColor, p.BColor = color(p.A), color(p.B)
		return p
	}
	var actionable, pending, inSync, ineligible []tagPairRow
	for _, p := range local {
		p = paint(p)
		switch p.Direction {
		case "suggest":
			actionable = append(actionable, p)
		case "pending":
			pending = append(pending, p)
		case "petition", "covered":
			// Declared here and on the PTR (directly, or derived through the
			// child's parent closure): in sync.
			inSync = append(inSync, p)
		default: // conflict / ineligible
			ineligible = append(ineligible, p)
		}
	}
	var petitionable, petitioned []tagPairRow
	for _, p := range ptrOnly {
		p = paint(p)
		if p.Direction == "petition" {
			petitionable = append(petitionable, p)
		} else {
			petitioned = append(petitioned, p)
		}
	}
	s.renderTemplate(w, "partials/tag_ptr_contrib_dialog.html", map[string]any{
		"TagID":        id,
		"TagName":      tag.Name,
		"Actionable":   actionable,
		"Pending":      pending,
		"InSync":       inSync,
		"Ineligible":   ineligible,
		"Petitionable": petitionable,
		"Petitioned":   petitioned,
		"Provisional":  provisional,
		"CSRFToken":    s.csrfToken(sessionFromContext(r.Context())),
	})
}

// tagPtrContribSend handles the tag dialog confirm: one stage-and-commit
// carrying the ticked relation suggestions and removal petitions with
// their reasons.
func (s *Server) tagPtrContribSend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.contribGateOpen() {
		http.Error(w, "contributions unavailable", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var items []map[string]any
	appendPairs := func(values []string, direction, reason string) {
		for _, v := range values {
			parts := strings.SplitN(v, " ", 3)
			if len(parts) != 3 {
				continue
			}
			if item := pairSendItem(parts[0], direction, parts[1], parts[2], reason); item != nil {
				items = append(items, item)
			}
		}
	}
	suggestReason := strings.TrimSpace(r.FormValue("suggest_reason"))
	petitionReason := strings.TrimSpace(r.FormValue("petition_reason"))
	if (len(r.Form["pair"]) > 0 && suggestReason == "") || (len(r.Form["pair_petition"]) > 0 && petitionReason == "") {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "a reason is required"})
		return
	}
	appendPairs(r.Form["pair"], "suggest", suggestReason)
	appendPairs(r.Form["pair_petition"], "petition", petitionReason)
	if len(items) == 0 {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "nothing selected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.monloaderContribSend(ctx, "tag "+strconv.FormatInt(id, 10), items)
	if err != nil {
		s.renderTemplate(w, "partials/ptr_contrib_flash.html", map[string]any{"Err": "contributions unavailable"})
		return
	}
	s.renderContribReceipt(w, items, resp)
}

// pairSendItem builds the stage item for a pair confirm: the suggest or
// petition kind matching the resolved direction. a/b are monbooru form;
// for sibling a=bad, b=good; for parent a=child, b=parent.
func pairSendItem(kind, direction, a, b, reason string) map[string]any {
	suggest, petition := "sibling", "sibling_petition"
	aKey, bKey := "bad", "good"
	if kind == "parent" {
		suggest, petition = "parent", "parent_petition"
		aKey, bKey = "child", "parent"
	}
	var itemKind string
	switch direction {
	case "suggest":
		itemKind = suggest
	case "petition":
		itemKind = petition
	default:
		return nil // pending / conflict / ineligible never send
	}
	return map[string]any{"kind": itemKind, aKey: a, bKey: b, "reason": reason}
}
