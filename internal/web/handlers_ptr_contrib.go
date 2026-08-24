package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
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

// ptrRefuse answers a prologue refusal: the dialogs get the status and the
// reason, the panels a bare 200 - a panel that cannot render collapses in
// place rather than reading as breakage.
func ptrRefuse(w http.ResponseWriter, msg string, code int) {
	if msg == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, msg, code)
}

// ptrTagTarget is the prologue every tag-side PTR handler opens with: the
// id off the path, the gate, then the tag. refusal is the gate's 409 text;
// an empty one takes the panels' silent 200 for every refusal. The gate
// runs ahead of the resolve so a closed one still costs no query.
func (s *Server) ptrTagTarget(w http.ResponseWriter, r *http.Request, open func() bool, refusal string) (int64, *models.Tag, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, nil, false
	}
	if !open() {
		ptrRefuse(w, refusal, http.StatusConflict)
		return 0, nil, false
	}
	tag, ok := s.contribTag(id)
	if !ok {
		ptrRefuse(w, ptrMissText(refusal), http.StatusNotFound)
		return 0, nil, false
	}
	return id, tag, true
}

// ptrImageTarget is the same prologue for the image-side handlers, ending
// in the row's sha. A cbz bundle has no single file to speak about, so it
// refuses like a missing row.
func (s *Server) ptrImageTarget(w http.ResponseWriter, r *http.Request, open func() bool, refusal string) (int64, string, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, "", false
	}
	if !open() {
		ptrRefuse(w, refusal, http.StatusConflict)
		return 0, "", false
	}
	sha, fileType, ok := s.imageShaType(id)
	if !ok || fileType == models.FileTypeCBZ {
		ptrRefuse(w, ptrMissText(refusal), http.StatusNotFound)
		return 0, "", false
	}
	return id, sha, true
}

// ptrMissText names the target-resolve refusal for a surface whose gate
// refusal is refusal: a panel stays silent for both.
func ptrMissText(refusal string) string {
	if refusal == "" {
		return ""
	}
	return "not found"
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
	// UnknownTag: a new row whose spelling is no tag the repository holds
	// at all, so a send would create the tag. Older monloaders omit it.
	UnknownTag bool   `json:"unknown_tag"`
	Color      string `json:"-"`
	// Sent is the spelling a contribution goes up under, which is the tag's
	// own name unless an alias here answered for it. Filled at render time.
	Sent string `json:"-"`
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
	resp, err := s.monloader().Do(ctx, method, path, body)
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
	byTag, byAlias := s.imageTagAliases(imageTags)
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	sent, local := s.ptrSubmitSpellings(ctx, storage, byTag)
	preview, err := s.monloaderContribPreview(ctx, sha, sent, implied)
	if err != nil {
		return nil, err
	}
	// Rows come back keyed on the submitted spelling; the surfaces label,
	// count and attribute them by the tag the operator sees.
	for i, t := range preview.ToAdd {
		preview.ToAdd[i].Sent = t.Tag
		if own, ok := local[t.Tag]; ok {
			preview.ToAdd[i].Tag = own
		}
	}
	foldAliasedPTRTags(preview, byAlias)
	return preview, nil
}

// ptrSubmitSpellings picks the spelling each storage tag is contributed
// under: the repository's own when a pull has left an alias here that answers
// for the tag, so an add lands in the cluster the catalog already agreed with
// instead of minting the operator's private spelling. Returns the list to
// submit and, per substituted spelling, the tag it stands for. Only tags that
// have an alias are asked about, and the graph endpoint refuses outright while
// the index is still syncing, where the panel keeps rendering: no answer means
// no substitution, not a failure.
func (s *Server) ptrSubmitSpellings(ctx context.Context, storage []string, byTag map[string][]string) ([]string, map[string]string) {
	var names []string
	for _, form := range storage {
		if len(byTag[form]) > 0 {
			names = append(names, form)
			names = append(names, byTag[form]...)
		}
	}
	if len(names) == 0 {
		return storage, nil
	}
	graph := map[string]ptrTagInfo{}
	for start := 0; start < len(names); start += ptrLookupBatch {
		part, err := s.ptrTagLookup(ctx, names[start:min(start+ptrLookupBatch, len(names))])
		if err != nil {
			return storage, nil
		}
		maps.Copy(graph, part)
	}
	sent := make([]string, len(storage))
	taken := make(map[string]bool, len(storage))
	for i, form := range storage {
		sent[i], taken[form] = form, true
	}
	local := map[string]string{}
	for i, form := range storage {
		if len(byTag[form]) == 0 {
			continue
		}
		// A spelling another tag on the image already occupies would submit
		// one name twice and pair the wrong row with it.
		if spelling, _, ok := resolvePTRSpelling(graph, form, byTag[form]); ok && !taken[spelling] {
			sent[i], taken[spelling], local[spelling] = spelling, true, form
		}
	}
	return sent, local
}

// foldAliasedPTRTags reconciles the two sides of the diff through the alias
// graph. A PTR pull adopts the repository's spellings as aliases of the
// operator's tag, and the preview only ever sees the tag's own name, so
// without this the repository's copy reads as a tag the image lacks and the
// operator's spelling as one the repository lacks - a petition and an upload
// for the same tag under two names the catalog calls equal.
func foldAliasedPTRTags(preview *contribPreview, byAlias map[string]string) {
	if len(byAlias) == 0 {
		return
	}
	carried := map[string]bool{}
	kept := preview.PTROnly[:0]
	for _, p := range preview.PTROnly {
		if local, ok := byAlias[p.Tag]; ok {
			carried[local] = true
			continue
		}
		kept = append(kept, p)
	}
	preview.PTROnly = kept
	for i, t := range preview.ToAdd {
		if t.Status == "new" && carried[t.Tag] {
			preview.ToAdd[i].Status = "known"
		}
	}
}

// imageTagAliases reads the alias rows pointing at the image's tags once for
// the two questions the diff asks of them: which spellings a tag can be
// contributed under (byTag), and which tag a repository spelling names
// (byAlias). The pull stores an alias under the same projection monloader
// answers the diff in, so the two sides compare as strings; a name a
// pre-widening catalog folded is keyed under both shapes.
func (s *Server) imageTagAliases(imageTags []models.ImageTag) (byTag map[string][]string, byAlias map[string]string) {
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		ids = append(ids, t.TagID)
	}
	byCanonical, err := s.tagSvc().AliasesForTagIDs(ids)
	if err != nil {
		logx.Warnf("AliasesForTagIDs: %v", err)
		return nil, nil
	}
	byTag, out := map[string][]string{}, map[string]string{}
	for _, list := range byCanonical {
		for _, a := range list {
			form := tagFormName(a.CategoryName, a.Name)
			canonical := tagFormName(a.CanonicalCategoryName, a.CanonicalName)
			byTag[canonical] = append(byTag[canonical], form)
			out[form] = canonical
			out[tags.LegacyFold(form)] = canonical
		}
	}
	return byTag, out
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
	id, sha, ok := s.ptrImageTarget(w, r, s.contribReadOpen, "")
	if !ok {
		return
	}
	preview, err := s.imageContribPreview(r.Context(), id, sha)
	if err != nil {
		// A 409 or a transport hiccup collapses the panel in place.
		w.WriteHeader(http.StatusOK)
		return
	}
	var newTags, petitionTags, unknownTags []string
	hasKnown := false
	for _, t := range preview.ToAdd {
		switch t.Status {
		case "new":
			newTags = append(newTags, t.Tag)
			if t.UnknownTag {
				unknownTags = append(unknownTags, t.Tag)
			}
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
		"UnknownCount":      len(unknownTags),
		"UnknownTip":        strings.Join(unknownTags, "\n"),
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
	id, sha, ok := s.ptrImageTarget(w, r, s.contribGateOpen, "contributions unavailable")
	if !ok {
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
	id, sha, ok := s.ptrImageTarget(w, r, s.contribGateOpen, "contributions unavailable")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
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
	Rel            string // alias of this | implies | implied by
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

// tagContribDiff is the tag page's contribution diff: the declared
// relations with their pair-preview direction, the PTR-side relations
// monbooru does not declare (removal-petition candidates), and how the
// PTR knows the tag at all.
type tagContribDiff struct {
	Local, PTROnly []tagPairRow
	// Pullable reports whether a pull would still add a relation, so the
	// pull control can hide once nothing is left to adopt.
	Pullable    bool
	Provisional bool
	// KnownAs is the alias spelling the PTR answered for when it does not
	// know the tag's own name; empty when it does, or knows nothing.
	KnownAs string
	Unknown bool
	// Empty is a spelling the PTR holds with no cluster behind it. Distinct
	// from Unknown, and from a cluster the catalog already declares in full:
	// here another spelling may still carry the relations.
	Empty bool
	// IdealElsewhere is the PTR ideal when it names a separate tag here. The
	// pull leaves that name alone, so the local answer is a merge.
	IdealElsewhere string
}

// tagContribRows assembles the tag's contribution diff. The PTR is asked
// about the tag's own name and every alias pointing at it in one call, so
// a tag the operator spells differently from the repository still finds its
// graph through the alias.
func (s *Server) tagContribRows(ctx context.Context, id int64, tag *models.Tag) (*tagContribDiff, error) {
	tagForm := tagFormName(tag.CategoryName, tag.Name)
	aliases, err := s.tagSvc().AliasesForTagIDs([]int64{id})
	if err != nil {
		return nil, err
	}
	implications, err := s.tagSvc().ListImplications(id)
	if err != nil {
		return nil, err
	}
	impliedBy, err := s.tagSvc().ImpliedBy(id)
	if err != nil {
		return nil, err
	}
	d := &tagContribDiff{}
	localAlias, localImplied := map[string]bool{}, map[string]bool{}
	var aliasForms []string
	for _, a := range aliases[id] {
		form := tagFormName(a.CategoryName, a.Name)
		d.Local = append(d.Local, tagPairRow{Kind: "sibling", A: form, B: tagForm, Rel: "alias of this"})
		localAlias[form] = true
		aliasForms = append(aliasForms, form)
	}
	for _, im := range impliedBy {
		d.Local = append(d.Local, tagPairRow{Kind: "parent", A: tagFormName(im.ParentCategoryName, im.ParentName), B: tagForm, Rel: "implied by"})
	}
	for _, im := range implications {
		d.Local = append(d.Local, tagPairRow{Kind: "parent", A: tagForm, B: tagFormName(im.ImpliedCategoryName, im.ImpliedName), Rel: "implies"})
		localImplied[tagFormName(im.ImpliedCategoryName, im.ImpliedName)] = true
	}
	names := append([]string{tagForm}, aliasForms...)
	if len(names) > ptrLookupBatch {
		names = names[:ptrLookupBatch]
	}
	graph, err := s.ptrTagLookup(ctx, names)
	if err != nil {
		return nil, err
	}
	known, info, ok := resolvePTRSpelling(graph, tagForm, aliasForms)
	if ok && known != tagForm {
		d.KnownAs = known
	}
	for i := range d.Local {
		// Known only through another spelling: the PTR cannot see the tag's
		// own name, so pair-preview reads every local relation as new. The
		// ones the cluster already carries under that spelling are on the
		// PTR in all but orientation - suggesting them would propose a flip
		// of the ideal - so they fold as covered without asking.
		if d.KnownAs != "" && s.heldByCluster(d.Local[i], tagForm, known, info) {
			d.Local[i].Direction = "covered"
			continue
		}
		preview, err := s.monloaderPairPreview(ctx, d.Local[i].Kind, d.Local[i].A, d.Local[i].B)
		if err != nil {
			return nil, err
		}
		d.Local[i].Direction, d.Local[i].Note = preview.Direction, preview.Note
		d.Provisional = d.Provisional || preview.Provisional
	}
	if !ok {
		d.Unknown = true
		return d, nil
	}
	// The PTR side: relations of this tag that monbooru does not declare,
	// limited to the directions a pull can adopt - fan-in aliases and
	// implications out. Implied-by edges are left out: a pull never writes
	// the reverse implication onto the parent, so offering them is a dead
	// action. Only pairs the index actually holds qualify (the preview
	// answers petition or pending); anything else is graph noise from a
	// sibling chain and is dropped. The pairs are keyed on the spelling the
	// PTR answered for, since the index holds them under that name.
	d.Pullable = s.pullWouldAdd(id, info, known)
	d.Empty = len(info.Aliases) == 0 && len(info.Implications) == 0 && len(info.ImpliedBy) == 0 &&
		(info.Ideal == "" || info.Ideal == known)
	var candidates []tagPairRow
	for _, a := range info.Aliases {
		if a == "" || a == known || localAlias[s.localForm(a)] {
			continue
		}
		candidates = append(candidates, tagPairRow{Kind: "sibling", A: a, B: known, Rel: "alias of this"})
	}
	// A pull inverts the PTR's orientation - the ideal lands here as an
	// alias of this tag - so an ideal already declared locally is not a
	// missing relation. One the catalog holds under another tag is not one
	// either: the pull leaves that name alone and no petition undoes it.
	if info.Ideal != "" && info.Ideal != known && !localAlias[s.localForm(info.Ideal)] && s.tagFormExists(info.Ideal) {
		d.IdealElsewhere = info.Ideal
	}
	for _, im := range info.Implications {
		if im == "" || im == known || localImplied[s.localForm(im)] {
			continue
		}
		// The pull skips an endpoint aliased elsewhere here, so the edge has
		// no local form to disagree with.
		if isAlias, _, _, _, ok := s.tagRowByForm(im); ok && isAlias {
			continue
		}
		candidates = append(candidates, tagPairRow{Kind: "parent", A: known, B: im, Rel: "implies"})
	}
	for _, row := range candidates {
		// A petition needs a local judgement about the pair; a relation
		// whose other endpoint the catalog has never seen is not one the
		// operator can vouch against, so it is not offered at all.
		other := row.A
		if other == known {
			other = row.B
		}
		if !s.tagFormExists(other) {
			continue
		}
		preview, err := s.monloaderPairPreview(ctx, row.Kind, row.A, row.B)
		if err != nil {
			return nil, err
		}
		if preview.Direction != "petition" && preview.Direction != "pending" {
			continue
		}
		row.Direction, row.Note = preview.Direction, preview.Note
		d.PTROnly = append(d.PTROnly, row)
	}
	return d, nil
}

// localForm normalizes a PTR-side name the way the catalog stores it, so a
// membership check against local names never misses on a spelling the tag
// input would fold. A name the charset cannot hold passes through unchanged.
func (s *Server) localForm(name string) string {
	if form, ok := s.ptrSpellingForm(name); ok {
		return form
	}
	return name
}

// heldByCluster reports whether a local relation of the tag is one the PTR
// cluster answered for under `known` already carries: the alias that
// answered, the ideal or a fan-in alias for a sibling row; a declared edge
// for an implication row.
func (s *Server) heldByCluster(row tagPairRow, tagForm, known string, info ptrTagInfo) bool {
	names := func(list []string) bool {
		return slices.ContainsFunc(list, func(n string) bool { return s.localForm(n) == row.A })
	}
	switch {
	case row.Kind == "sibling":
		return row.A == known || row.A == s.localForm(info.Ideal) || names(info.Aliases)
	case row.A == tagForm:
		return slices.ContainsFunc(info.Implications, func(n string) bool { return s.localForm(n) == row.B })
	default:
		return names(info.ImpliedBy)
	}
}

// pullWouldAdd reports whether a PTR pull would still create a relation for the
// tag: an alias whose name is free (a pull never overwrites an existing tag),
// the ideal, or an implies / implied-by edge not yet declared against a tag the
// catalog does not alias elsewhere. Every check goes through the same
// normalization and row lookups applyPTRTagInfo uses, so the pull control
// tracks real work - a prediction that compared spellings by string could
// keep offering a pull that adopts nothing.
func (s *Server) pullWouldAdd(tagID int64, info ptrTagInfo, tagForm string) bool {
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
		if im != "" && im != tagForm && s.edgeWouldAdd(tagID, im, true) {
			return true
		}
	}
	for _, im := range info.ImpliedBy {
		if im != "" && im != tagForm && s.edgeWouldAdd(tagID, im, false) {
			return true
		}
	}
	return false
}

// edgeWouldAdd mirrors applyPTRTagInfo's implication branch for one PTR
// name: representable, and either no row yet (the pull creates it) or a
// canonical row the edge is not declared against. An alias row is skipped
// there, so it is not an add here.
func (s *Server) edgeWouldAdd(tagID int64, name string, implied bool) bool {
	catID, bare, ok := s.splitCategoryTag(name)
	if !ok {
		return false
	}
	norm, err := tags.ValidateTagName(bare)
	if err != nil {
		return false
	}
	var id int64
	var isAlias int
	err = s.db().Read.QueryRow(
		`SELECT id, is_alias FROM tags WHERE name = ? AND category_id = ?`, norm, catID,
	).Scan(&id, &isAlias)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil || isAlias == 1 {
		return false
	}
	parent, child := tagID, id
	if !implied {
		parent, child = id, tagID
	}
	var n int
	err = s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?`, parent, child,
	).Scan(&n)
	return err == nil && n == 0
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
func (s *Server) tagContribPreview(rctx context.Context, id int64, tag *models.Tag) (*tagContribDiff, error) {
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	return s.tagContribRows(ctx, id, tag)
}

// tagPtrContribPanel renders the tag-page contribution card body: a
// one-line summary of what the relation diff offers. Empty when the
// gate is closed or the tag is not eligible, so the caller only mounts
// it when eligible.
func (s *Server) tagPtrContribPanel(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.contribReadOpen, "")
	if !ok {
		return
	}
	diff, err := s.tagContribPreview(r.Context(), id, tag)
	if err != nil {
		// A 409 or a transport hiccup collapses the panel in place.
		w.WriteHeader(http.StatusOK)
		return
	}
	var newTags, petitionTags []string
	for _, p := range diff.Local {
		if p.Direction == "suggest" {
			newTags = append(newTags, p.Label())
		}
	}
	for _, p := range diff.PTROnly {
		if p.Direction == "petition" {
			petitionTags = append(petitionTags, p.Label())
		}
	}
	canContribute, contribHint := s.contribHint()
	s.renderTemplate(w, "partials/tag_ptr_contrib_panel.html", map[string]any{
		"TagID":          id,
		"NewCount":       len(newTags),
		"PetitionCount":  len(petitionTags),
		"Pullable":       diff.Pullable && s.ptrPullOpen(),
		"NewTip":         strings.Join(newTags, "\n"),
		"PetitionTip":    strings.Join(petitionTags, "\n"),
		"KnownAs":        diff.KnownAs,
		"Unknown":        diff.Unknown,
		"Empty":          diff.Empty,
		"IdealElsewhere": diff.IdealElsewhere,
		"CanContribute":  canContribute,
		"ContribHint":    contribHint,
		"Provisional":    diff.Provisional,
		"FailedUploads":  s.monloaderContribFailedSeed(),
		"Monloader":      s.monloaderWebBase(),
	})
}

// tagPtrContribDialog renders the tag contribute dialog, populated from
// the relation diff.
func (s *Server) tagPtrContribDialog(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.contribGateOpen, "contributions unavailable")
	if !ok {
		return
	}
	diff, err := s.tagContribPreview(r.Context(), id, tag)
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
	for _, p := range diff.Local {
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
	for _, p := range diff.PTROnly {
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
		"Provisional":  diff.Provisional,
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

// ptrLookupRow is one name of a looked-up PTR cluster in the pull preview,
// with what the pull would do about it.
type ptrLookupRow struct {
	Name, Color string
	Class       string // ptr-row-staged (would add) | ptr-row-ctx (declared) | ptr-row-refused (left alone)
	Note        string
}

// ptrLookupGroup is one hunk of the preview: aliases, implies, implied by.
type ptrLookupGroup struct {
	Title, Note string
	Rows        []ptrLookupRow
	Adds        int
}

// ptrNameValid reports whether a PTR-side name can be a tag row here.
func (s *Server) ptrNameValid(name string) bool {
	_, bare, ok := s.splitCategoryTag(name)
	if !ok {
		return false
	}
	_, err := tags.ValidateTagName(bare)
	return err == nil
}

// tagRowByForm answers the catalog row a valid monbooru-form name maps to.
func (s *Server) tagRowByForm(form string) (isAlias bool, canonicalID int64, canonicalName string, usage int, ok bool) {
	catID, bare, _ := s.splitCategoryTag(form)
	norm, _ := tags.ValidateTagName(bare)
	var alias int
	var canon sql.NullInt64
	var canonName sql.NullString
	err := s.db().Read.QueryRow(
		`SELECT t.is_alias, t.canonical_tag_id, c.name, t.usage_count
		 FROM tags t LEFT JOIN tags c ON c.id = t.canonical_tag_id
		 WHERE t.name = ? AND t.category_id = ?`, norm, catID,
	).Scan(&alias, &canon, &canonName, &usage)
	if err != nil {
		return false, 0, "", 0, false
	}
	return alias == 1, canon.Int64, canonName.String, usage, true
}

// ptrLookupAliasRow classifies one alias-side name of the cluster the way
// applyPTRTagInfo will treat it: created when the name is free, already
// declared when it points here, left alone otherwise.
func (s *Server) ptrLookupAliasRow(tagID int64, name string, color func(string) string) ptrLookupRow {
	row := ptrLookupRow{Name: name, Color: color(name)}
	if !s.ptrNameValid(name) {
		row.Class, row.Note = "ptr-row-refused", "not representable"
		return row
	}
	isAlias, canonicalID, canonicalName, usage, exists := s.tagRowByForm(name)
	switch {
	case !exists:
		row.Class, row.Note = "ptr-row-staged", "new alias"
	case isAlias && canonicalID == tagID:
		row.Class, row.Note = "ptr-row-ctx", "already an alias here"
	case isAlias:
		row.Class, row.Note = "ptr-row-refused", "an alias here of "+canonicalName
	default:
		row.Class, row.Note = "ptr-row-refused", fmt.Sprintf("a tag here (usage %d)", usage)
	}
	return row
}

// ptrLookupImplRow classifies one implication endpoint: a fresh edge when
// the tag exists or will be created, declared when the edge is already
// here, left alone when the name is an alias pointing at another tag (the
// pull skips those, since the edge would land on a tag the PTR never named).
func (s *Server) ptrLookupImplRow(name string, declared map[string]bool, color func(string) string) ptrLookupRow {
	row := ptrLookupRow{Name: name, Color: color(name)}
	if declared[s.localForm(name)] {
		row.Class, row.Note = "ptr-row-ctx", "declared"
		return row
	}
	if !s.ptrNameValid(name) {
		row.Class, row.Note = "ptr-row-refused", "not representable"
		return row
	}
	isAlias, _, canonicalName, _, exists := s.tagRowByForm(name)
	switch {
	case !exists:
		row.Class, row.Note = "ptr-row-staged", "new implication, new tag"
	case isAlias:
		row.Class, row.Note = "ptr-row-refused", "an alias here of "+canonicalName
	default:
		row.Class, row.Note = "ptr-row-staged", "new implication"
	}
	return row
}

// ptrLookupGroups builds the pull preview for a looked-up spelling: the
// cluster's spellings (the looked-up one, the ideal, the aliases), then
// both implication directions, each row classified as the pull would treat
// it.
func (s *Server) ptrLookupGroups(id int64, tagForm, spelling string, info ptrTagInfo) ([]ptrLookupGroup, error) {
	implications, err := s.tagSvc().ListImplications(id)
	if err != nil {
		return nil, err
	}
	impliedBy, err := s.tagSvc().ImpliedBy(id)
	if err != nil {
		return nil, err
	}
	localImplied, localImpliedBy := map[string]bool{}, map[string]bool{}
	for _, im := range implications {
		localImplied[tagFormName(im.ImpliedCategoryName, im.ImpliedName)] = true
	}
	for _, im := range impliedBy {
		localImpliedBy[tagFormName(im.ParentCategoryName, im.ParentName)] = true
	}
	color := s.contribTagColor()
	seen := map[string]bool{tagForm: true}
	aliases := ptrLookupGroup{Title: "aliases", Note: "would become aliases of " + tagForm}
	for _, name := range append([]string{spelling, info.Ideal}, info.Aliases...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		row := s.ptrLookupAliasRow(id, name, color)
		if row.Class == "ptr-row-staged" {
			aliases.Adds++
		}
		aliases.Rows = append(aliases.Rows, row)
	}
	implies := s.ptrLookupImplGroup(ptrLookupGroup{Title: "implies"},
		info.Implications, localImplied, color, tagForm, spelling)
	implied := s.ptrLookupImplGroup(ptrLookupGroup{Title: "implied by", Note: "direct children only, at most 200"},
		info.ImpliedBy, localImpliedBy, color, tagForm, spelling)
	var groups []ptrLookupGroup
	for _, g := range []ptrLookupGroup{aliases, implies, implied} {
		if len(g.Rows) > 0 {
			groups = append(groups, g)
		}
	}
	return groups, nil
}

// ptrLookupImplGroup fills one implication direction's rows into g. The two
// directions differ only in which edge set counts as already declared; the
// looked-up tag and the spelling itself are skipped either way, since an
// edge to oneself is not one the pull would make.
func (s *Server) ptrLookupImplGroup(g ptrLookupGroup, names []string, declared map[string]bool, color func(string) string, tagForm, spelling string) ptrLookupGroup {
	for _, name := range names {
		if name == "" || name == tagForm || name == spelling {
			continue
		}
		row := s.ptrLookupImplRow(name, declared, color)
		if row.Class == "ptr-row-staged" {
			g.Adds++
		}
		g.Rows = append(g.Rows, row)
	}
	return g
}

// ptrLookupLocalStatus words where the looked-up spelling stands in this
// catalog, for the preview's header line. aliasable is false for the two
// standings a merge onto that spelling resolves back to this tag, which is
// no merge at all.
func (s *Server) ptrLookupLocalStatus(id int64, tagForm, spelling string) (status string, aliasable bool) {
	if spelling == tagForm {
		return "this tag", false
	}
	isAlias, canonicalID, canonicalName, usage, exists := s.tagRowByForm(spelling)
	switch {
	case !exists:
		return "not in this catalog", true
	case isAlias && canonicalID == id:
		return "alias of this tag", false
	case isAlias:
		return "alias of " + canonicalName, true
	default:
		return fmt.Sprintf("a tag here (usage %d)", usage), true
	}
}

// ptrSearchLimit is how many clusters the look-up dialog lists at once.
const ptrSearchLimit = 20

// ptrSpellingStems lists the prefixes to try for a tag, longest first: the
// whole name, then with each trailing "(qualifier)" dropped, then with
// trailing underscore-separated words dropped. An operator's spelling is
// usually the repository's with something added, so the longest stem that
// answers is the closest guess. The category rides along on every stem.
func ptrSpellingStems(form string) []string {
	cat, name, qualified := strings.Cut(form, ":")
	if !qualified {
		cat, name = "", form
	}
	var out []string
	seen := map[string]bool{}
	push := func(n string) {
		if len(n) < 4 || seen[n] {
			return
		}
		seen[n] = true
		if cat != "" {
			n = cat + ":" + n
		}
		out = append(out, n)
	}
	push(name)
	stem := name
	for strings.HasSuffix(stem, ")") && strings.Contains(stem, "_(") {
		stem = stem[:strings.LastIndex(stem, "_(")]
		push(stem)
	}
	for parts := strings.Split(stem, "_"); len(parts) > 1; {
		parts = parts[:len(parts)-1]
		push(strings.Join(parts, "_"))
	}
	return out
}

// ptrSeedSearch runs the stem backoff and returns the first answer worth
// showing: a stem whose clusters carry relations, since the tag's own name
// often sits in the index as an orphan and stopping there would hide the
// cluster one stem up. Falls back to the longest stem that answered at all.
func (s *Server) ptrSeedSearch(ctx context.Context, tagForm string) (clusters []ptrCluster, stem string, truncated bool, err error) {
	for _, candidate := range ptrSpellingStems(tagForm) {
		got, cut, err := s.ptrSpellingSearch(ctx, candidate, "", ptrSearchLimit)
		if err != nil {
			return nil, "", false, err
		}
		if len(got) == 0 {
			continue
		}
		if clusters == nil {
			clusters, stem, truncated = got, candidate, cut
		}
		for _, c := range got {
			if c.Aliases+c.Implications+c.ImpliedBy > 0 {
				return got, candidate, cut, nil
			}
		}
	}
	return clusters, stem, truncated, nil
}

// ptrSearchRow is one cluster as the dialog lists it.
type ptrSearchRow struct {
	Spelling string
	Color    string
	Note     string
}

// ptrSearchRows renders the clusters into pickable rows, noting each one's
// graph size and, when the match came in under another spelling, which.
func (s *Server) ptrSearchRows(clusters []ptrCluster) []ptrSearchRow {
	color := s.contribTagColor()
	rows := make([]ptrSearchRow, 0, len(clusters))
	for _, c := range clusters {
		var parts []string
		if len(c.Matched) > 0 && c.Matched[0] != c.Ideal {
			parts = append(parts, "via "+c.Matched[0])
		}
		if c.Aliases == 1 {
			parts = append(parts, "+1 alias")
		} else if c.Aliases > 1 {
			parts = append(parts, fmt.Sprintf("+%d aliases", c.Aliases))
		}
		if c.Implications > 0 {
			parts = append(parts, fmt.Sprintf("+%d implies", c.Implications))
		}
		if c.ImpliedBy > 0 {
			parts = append(parts, fmt.Sprintf("%d implied by", c.ImpliedBy))
		}
		if len(parts) == 0 {
			parts = append(parts, "no relations")
		}
		rows = append(rows, ptrSearchRow{Spelling: c.Ideal, Color: color(c.Ideal), Note: strings.Join(parts, " · ")})
	}
	return rows
}

// tagPtrLookupSearch answers the dialog's two search shapes from one place: a
// request carrying no `q` at all is the seed the dialog opens on, rendered
// into the body with its own header and cancel; one carrying `q` is the
// typeahead, rendered bare into the dropdown.
func (s *Server) tagPtrLookupSearch(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ptrPullOpen, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	query := r.URL.Query()
	typed, isTypeahead := query["q"]
	data := map[string]any{"TagID": id, "Seed": !isTypeahead}
	render := func() { s.renderTemplate(w, "partials/tag_ptr_lookup_search.html", data) }

	tagForm := tagFormName(tag.CategoryName, tag.Name)
	q := strings.TrimSpace(strings.Join(typed, ""))
	if isTypeahead && q == "" {
		render()
		return
	}
	// The seed walks several stems and a substring pass walks a whole
	// namespace, so this budget is wider than the panel's single graph call.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var clusters []ptrCluster
	var truncated bool
	var err error
	stem := q
	if isTypeahead {
		mode := ""
		if query.Get("anywhere") != "" {
			mode = "contains"
		}
		clusters, truncated, err = s.ptrSpellingSearch(ctx, q, mode, ptrSearchLimit)
	} else {
		clusters, stem, truncated, err = s.ptrSeedSearch(ctx, tagForm)
	}
	switch {
	case errors.Is(err, errPTRNoSearch):
		// An older monloader: the dialog keeps its plain input and says
		// nothing rather than looking broken.
		render()
		return
	case errors.Is(err, errPTRSearchUnbounded):
		data["Note"] = "matching anywhere in the name needs a category: prefix."
		render()
		return
	case err != nil:
		data["Note"] = "the Public Tag Repository is unavailable."
		render()
		return
	}

	data["Rows"] = s.ptrSearchRows(clusters)
	switch {
	case isTypeahead:
		if truncated {
			data["Note"] = fmt.Sprintf("showing the first %d.", len(clusters))
		}
	case len(clusters) == 0:
		data["Note"] = "the Public Tag Repository holds no spelling near " + tagForm + "."
	case stem != tagForm:
		data["Note"] = "no PTR spelling starts with " + tagForm + "; showing " + stem + "."
	}
	render()
}

// tagPtrLookupDialog renders the look-up dialog shell: one spelling input,
// the result slot the preview fills, and a cancel.
func (s *Server) tagPtrLookupDialog(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ptrPullOpen, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	s.renderTemplate(w, "partials/tag_ptr_lookup_dialog.html", map[string]any{
		"TagID":   id,
		"TagName": tag.Name,
	})
}

// tagPtrLookupPreview answers the graph for one operator-typed spelling and
// renders what a pull under it would do to this tag.
func (s *Server) tagPtrLookupPreview(w http.ResponseWriter, r *http.Request) {
	id, tag, ok := s.ptrTagTarget(w, r, s.ptrPullOpen, "the Public Tag Repository is unavailable")
	if !ok {
		return
	}
	data := map[string]any{"TagID": id, "TagName": tag.Name}
	spelling, ok := s.ptrSpellingForm(r.URL.Query().Get("as"))
	if !ok {
		data["Err"] = "not a tag name"
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	graph, err := s.ptrTagLookup(ctx, []string{spelling})
	if err != nil {
		data["Err"] = "the Public Tag Repository is unavailable"
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	data["Spelling"] = spelling
	info := graph[spelling]
	if !info.Known {
		s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
		return
	}
	tagForm := tagFormName(tag.CategoryName, tag.Name)
	data["TagForm"] = tagForm
	groups, err := s.ptrLookupGroups(id, tagForm, spelling, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	color := s.contribTagColor()
	aliasAdds, implAdds, declared, skipped := 0, 0, 0, 0
	for _, g := range groups {
		for _, row := range g.Rows {
			switch row.Class {
			case "ptr-row-staged":
				if g.Title == "aliases" {
					aliasAdds++
				} else {
					implAdds++
				}
			case "ptr-row-ctx":
				declared++
			default:
				skipped++
			}
		}
	}
	data["Known"] = true
	data["Ideal"] = info.Ideal
	data["SpellingColor"] = color(spelling)
	data["IdealColor"] = color(info.Ideal)
	data["LocalStatus"], data["CanAlias"] = s.ptrLookupLocalStatus(id, tagForm, spelling)
	data["Groups"] = groups
	data["AliasAdds"] = aliasAdds
	data["ImplAdds"] = implAdds
	data["Declared"] = declared
	data["Skipped"] = skipped
	s.renderTemplate(w, "partials/tag_ptr_lookup_result.html", data)
}
