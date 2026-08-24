package web

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// taggerRow is the per-template render shape for one row of the
// Auto-Tagger settings table. It unifies installed taggers and catalog
// ghosts so the template iterates a single list. Supported rows (i.e.
// in the catalog) carry precomputed host + docker install snippets so
// the Instructions cell can open a per-row dialog without the template
// touching shell quoting.
type taggerRow struct {
	Name                string
	Description         string
	Available           bool
	Reason              string
	Enabled             bool
	ConfidenceThreshold float64
	ConfigSummary       string
	Differs             bool
	Installed           bool
	Supported           bool
	Gated               bool
	HostCommand         string
	DockerCommand       string
}

// installedTaggerRow fills the fields every installed tagger shares; the
// caller layers the catalog-only fields on supported rows. Differs
// drives the row's Reset button: true when any operator-tunable state
// (thresholds, caps, disabled categories, gallery scope, dispatch
// overlay) departs from the catalog-seeded stock.
func (s *Server) installedTaggerRow(t tagger.TaggerStatus, totalGalleries int, modelPath string) taggerRow {
	ruleCount := tagger.OverlayRuleCount(modelPath, t.Name)
	return taggerRow{
		Name:                t.Name,
		Available:           t.Available,
		Reason:              t.Reason,
		Enabled:             t.Enabled,
		ConfidenceThreshold: t.ConfidenceThreshold,
		ConfigSummary:       taggerConfigSummary(t.TaggerInstance, totalGalleries, ruleCount),
		Differs:             taggerDiffersFromStock(t.TaggerInstance, modelPath, ruleCount),
		Installed:           true,
	}
}

// taggerConfigSummary is the inline summary next to the row's single
// Configure button: the threshold summary, a gallery restriction when
// one applies, and the custom dispatch-rule count when the overlay
// exists.
func taggerConfigSummary(inst config.TaggerInstance, totalGalleries, ruleCount int) string {
	out := taggerThresholdSummary(inst.ConfidenceThreshold, inst.CategoryThresholds, inst.DisabledCategories)
	if inst.Galleries != nil && len(inst.Galleries) != totalGalleries {
		if len(inst.Galleries) == 0 {
			out += ", no galleries"
		} else {
			out += ", galleries: " + strings.Join(inst.Galleries, ", ")
		}
	}
	switch {
	case ruleCount == 1:
		out += ", 1 rule"
	case ruleCount > 1:
		out += fmt.Sprintf(", %d rules", ruleCount)
	}
	return out
}

// taggerDiffersFromStock reports whether the instance departs from what
// SeedTaggerInstance would produce for a fresh row: any threshold / cap
// / disabled-category override off the catalog seed, a gallery
// restriction, or a dispatch overlay on disk.
func taggerDiffersFromStock(inst config.TaggerInstance, modelPath string, ruleCount int) bool {
	seed := tagger.SeedTaggerInstance(inst.Name, inst.Enabled, catalogEntryByName(modelPath, inst.Name))
	if inst.ConfidenceThreshold != seed.ConfidenceThreshold {
		return true
	}
	if !maps.Equal(inst.CategoryThresholds, seed.CategoryThresholds) {
		return true
	}
	if !maps.Equal(inst.PerCategoryTopK, seed.PerCategoryTopK) {
		return true
	}
	a := append([]string(nil), inst.DisabledCategories...)
	b := append([]string(nil), seed.DisabledCategories...)
	sort.Strings(a)
	sort.Strings(b)
	if !slices.Equal(a, b) {
		return true
	}
	return inst.Galleries != nil || ruleCount > 0
}

// thresholdRow is the per-category render shape for the per-tagger
// Configure dialog. Override is the live category_thresholds value; an
// empty Override falls back to the global threshold and the input
// renders a placeholder instead of a value. MaxTags is the live
// per_category_top_k value formatted as a string ("" = use default,
// "0" = uncapped). Disabled mirrors disabled_categories membership; a
// disabled category emits nothing regardless of its threshold.
// DefaultThreshold / DefaultMaxTags are the catalog-seeded values for
// this category (empty when the catalog seeds none); the per-row Reset
// restores the inputs to these so it lands on the same state the
// dialog-level "Reset to defaults" would, instead of blanking the cell.
type thresholdRow struct {
	Category         string
	Override         string // "" when no override; formatted "%.2f" otherwise
	MaxTags          string // "" when no override; integer string otherwise
	MaxDefault       int    // default cap surfaced as the input placeholder
	Color            string // tag_categories.color, surfaced as a 1px dot
	Disabled         bool   // category is muted (in disabled_categories)
	ViaRules         bool   // reached only through dispatch rules, not the model
	DefaultThreshold string // catalog default threshold; "" = no catalog override
	DefaultMaxTags   string // catalog default top-K; "" = no catalog override
}

// taggerGalleryRow is the per-gallery render shape for the per-tagger
// Galleries dialog: one entry per configured gallery, with Checked =
// true when the tagger's Galleries list contains this name (or is
// empty/missing, meaning "every gallery").
type taggerGalleryRow struct {
	Name    string
	Checked bool
}

func (s *Server) settingsTaggerPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	newProvider := strings.ToLower(strings.TrimSpace(r.FormValue("execution_provider")))
	newProvider = cmp.Or(newProvider, "cpu")
	if !config.IsValidExecutionProvider(newProvider) {
		writeInlineFlash(w, "err", "Invalid execution provider: "+newProvider)
		return
	}
	// Probe the requested execution provider before persisting the change so
	// the user sees any library/device issue immediately instead of waiting
	// for a tagger run to fail. ORT env init is not re-entrant so refuse
	// while a tagger job is holding it.
	if newProvider != s.executionProvider() {
		if s.jobs.IsRunning() {
			writeInlineFlash(w, "err", "A job is running; try again when it finishes.")
			return
		}
		if newProvider != "cpu" {
			// SA4023: the stub build's probe always errors, the tagger build's
			// does not, and staticcheck only ever sees the stub.
			if err := tagger.CheckProviderAvailable(newProvider); err != nil { //nolint:staticcheck
				writeInlineFlash(w, "err", "Cannot enable "+newProvider+": "+err.Error())
				return
			}
		}
	}

	s.cfgMu.Lock()
	providerChanged := s.cfg.Tagger.ExecutionProvider != newProvider
	s.cfg.Tagger.ExecutionProvider = newProvider
	if n, err := strconv.Atoi(r.FormValue("parallel")); err == nil && n >= 1 {
		s.cfg.Tagger.Parallel = n
	}
	if v := r.FormValue("idle_release_after_minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			s.cfg.Tagger.IdleReleaseAfterMinutes = n
		}
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	// Drop the cached ORT session so the freed RAM is visible immediately
	// rather than after idle_release_after_minutes elapses.
	if providerChanged {
		tagger.ReleaseAll()
	}
	logx.Infof("settings: tagger updated (execution_provider=%s)", newProvider)
	writeInlineFlash(w, "ok", "Saved.")
	s.renderTemplate(w, "partials/tagger_mode_badge.html", map[string]any{
		"Provider": newProvider,
		"OOB":      true,
	})
}

// settingsTaggerEnablePost flips one tagger's enabled flag to true without
// going through the full tagger form. Mirrors settingsTaggerDisablePost.
func (s *Server) settingsTaggerEnablePost(w http.ResponseWriter, r *http.Request) {
	s.applyTaggerEnabled(w, strings.TrimSpace(r.PathValue("name")), true)
}

// settingsTaggerDisablePost flips one tagger's enabled flag to false without
// going through the full tagger form. An HX-Refresh header re-renders the
// settings page so the row's enabled state and Actions column reflect the
// new state.
func (s *Server) settingsTaggerDisablePost(w http.ResponseWriter, r *http.Request) {
	s.applyTaggerEnabled(w, strings.TrimSpace(r.PathValue("name")), false)
}

// updateTagger applies mutate to the named TaggerInstance under cfgMu,
// seeding a fresh entry from the on-disk catalog when one doesn't exist
// yet, then persists the config to disk. Returns the saveConfig error
// or nil. Callers that need to surface a save failure to the operator
// pass its error string through their usual flash helper.
func (s *Server) updateTagger(name string, mutate func(*config.TaggerInstance)) error {
	modelPath := s.modelPath()
	s.cfgMu.Lock()
	found := false
	for i := range s.cfg.Tagger.Taggers {
		if s.cfg.Tagger.Taggers[i].Name == name {
			mutate(&s.cfg.Tagger.Taggers[i])
			found = true
			break
		}
	}
	if !found {
		catalog := catalogEntryByName(modelPath, name)
		seeded := tagger.SeedTaggerInstance(name, false, catalog)
		mutate(&seeded)
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers, seeded)
	}
	s.cfgMu.Unlock()
	return s.saveConfig()
}

// applyTaggerEnabled flips a tagger's Enabled flag, seeding a TOML
// entry from the on-disk catalog when one doesn't exist yet so the
// preference persists across disable/enable round trips.
func (s *Server) applyTaggerEnabled(w http.ResponseWriter, name string, enabled bool) {
	if err := tagger.ValidateTaggerName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.Enabled = enabled
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	logx.Infof("settings: tagger %q %s", name, verb)
	// The flash rides monbooru:flash so it survives the refresh; an
	// inline body would be discarded before the swap ever painted.
	setFlashHeader(w, "Tagger "+name+" "+verb+".", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

// settingsTaggerConfigGet renders the tabbed dialog body for one
// tagger: a galleries panel, a thresholds panel, a mappings panel.
// HTMX lazy-loads the body via hx-get on first dialog open; the tab
// strip only toggles panel visibility, so one Save submits every
// panel's fields.
func (s *Server) settingsTaggerConfigGet(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	rows, global, ok := s.thresholdDialogData(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	galRows, allChecked, _ := s.galleryDialogData(name)
	inst, _ := s.resolveTaggerInstance(name)
	modelPath := s.modelPath()
	catIDs, catList := s.categoryChoices()
	labelCount := 0
	if views, err := tagger.BrowseLabels(modelPath, name, taggerTagsFile(inst), catIDs); err == nil {
		labelCount = len(views)
	}

	embedded := tagger.EmbeddedDispatchRules(name)
	overlay := tagger.OverlayDispatchRules(modelPath, name)
	merged := tagger.MergedDispatchRules(modelPath, name)
	exportJSON := ""
	if b, err := tagger.MarshalDispatchDoc(merged); err == nil {
		exportJSON = string(b) + "\n"
	}
	profileJSON, profileLines := "", []exportLine(nil)
	sidecarFields := tagger.SidecarProfileFields(modelPath, name)
	if profile, err := tagger.ResolveProfile(modelPath, name, taggerTagsFile(inst)); err == nil {
		if b, err := tagger.ProfileExportDoc(profile); err == nil {
			profileJSON = string(b) + "\n"
			marked := map[string]bool{}
			for _, f := range sidecarFields {
				marked[f] = true
			}
			for _, line := range strings.Split(string(b), "\n") {
				mark := ""
				trimmed := strings.TrimSpace(line)
				if i := strings.Index(trimmed, `":`); strings.HasPrefix(trimmed, `"`) && i > 0 && marked[trimmed[1:i]] {
					mark = "add"
				}
				profileLines = append(profileLines, exportLine{Text: line, Mark: mark})
			}
		}
	}

	csrf := s.csrfToken(sessionFromContext(r.Context()))
	s.renderTemplate(w, "partials/tagger_config_dialog.html", map[string]any{
		"Name":         name,
		"Global":       global,
		"Rows":         rows,
		"GalRows":      galRows,
		"AllChecked":   allChecked,
		"Categories":   catList,
		"LabelCount":   labelCount,
		"CustomRules":  len(overlay),
		"ExportRules":  exportRuleLines(embedded, overlay),
		"ExportJSON":   exportJSON,
		"RuleCount":    len(merged),
		"ProfileLines": profileLines,
		"ProfileJSON":  profileJSON,
		"ProfileStock": len(sidecarFields) == 0,
		"CSRFToken":    csrf,
	})
}

// exportLine is one rendered line of the export panel's file views.
// Mark selects the color: "" (as shipped), "add" (this install's
// change), "del" (the shipped rule an overlay entry displaces - shown
// struck, absent from the copied file), "gap" (an elided run of
// unchanged rules).
type exportLine struct {
	Text string
	Mark string
}

// exportRuleLines renders the merged dispatch table as display lines:
// overlay-only sources read as added, overridden defaults as a struck
// shipped line above the replacement, and runs of more than five
// untouched defaults collapse into a count so a 1500-rule table stays
// scannable.
func exportRuleLines(embedded, overlay []tagger.DispatchEntry) []exportLine {
	embByte := map[string]tagger.DispatchEntry{}
	for _, e := range embedded {
		embByte[e.Source] = e
	}
	ovr := map[string]tagger.DispatchEntry{}
	for _, e := range overlay {
		ovr[e.Source] = e
	}
	sources := make([]string, 0, len(embByte)+len(ovr))
	seen := map[string]bool{}
	for _, e := range embedded {
		if !seen[e.Source] {
			seen[e.Source] = true
			sources = append(sources, e.Source)
		}
	}
	for _, e := range overlay {
		if !seen[e.Source] {
			seen[e.Source] = true
			sources = append(sources, e.Source)
		}
	}
	sort.Strings(sources)

	ruleText := func(e tagger.DispatchEntry) string {
		b, _ := json.Marshal(e)
		return "    " + string(b) + ","
	}
	lines := []exportLine{{Text: "{"}, {Text: `  "version": 1,`}, {Text: `  "rules": [`}}
	var run []tagger.DispatchEntry
	flushRun := func() {
		if len(run) <= 5 {
			for _, e := range run {
				lines = append(lines, exportLine{Text: ruleText(e)})
			}
		} else {
			lines = append(lines, exportLine{Text: fmt.Sprintf("    ... %d default rules ...", len(run)), Mark: "gap"})
		}
		run = nil
	}
	for _, src := range sources {
		o, hasOverlay := ovr[src]
		e, hasEmbedded := embByte[src]
		if !hasOverlay {
			run = append(run, e)
			continue
		}
		flushRun()
		if hasEmbedded {
			lines = append(lines, exportLine{Text: ruleText(e), Mark: "del"})
		}
		lines = append(lines, exportLine{Text: ruleText(o), Mark: "add"})
	}
	flushRun()
	lines = append(lines, exportLine{Text: "  ]"}, exportLine{Text: "}"})
	return lines
}

// taggerLabelRow is the render shape of one mappings-panel result row.
type taggerLabelRow struct {
	Source      string
	CatName     string
	TagName     string
	Color       string
	Muted       bool   // dropped by a rule
	CategoryOff bool   // effective category is in disabled_categories
	Rule        string // "", "default", "custom"
}

// taggerLabelPageSize is how many rows the mappings panel shows at
// once, and the step the trailing "[+ more]" grows the list by.
// taggerLabelMaxRows is where growing stops: past it the table costs
// more to render and scroll than the search costs to type.
const (
	taggerLabelPageSize = 50
	taggerLabelMaxRows  = 500
)

// settingsTaggerLabelsGet renders the mappings panel's rows: the
// model's label list resolved through the dispatch chain, filtered by
// q (substring on the raw label and the effective tag name) and filter
// (all | customized | muted). "customized" is this install's own rules,
// not the shipped ones. "muted" covers rule-muted labels and labels
// whose effective category the tagger has disabled - the row says
// which is which.
func (s *Server) settingsTaggerLabelsGet(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	s.renderTaggerLabels(w, r, name)
}

// renderTaggerLabels writes the mappings panel's result list for the
// current q / filter / limit. Shared by the search GET and the rule
// POST so an applied edit comes back through the same rendering path.
func (s *Server) renderTaggerLabels(w http.ResponseWriter, r *http.Request, name string) {
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		http.Error(w, "tagger not found", http.StatusNotFound)
		return
	}
	modelPath := s.modelPath()
	catIDs, _ := s.categoryChoices()
	colors := s.categoryColors()
	views, err := tagger.BrowseLabels(modelPath, name, taggerTagsFile(inst), catIDs)
	custom := len(tagger.OverlayDispatchRules(modelPath, name))
	if err != nil {
		s.renderTemplate(w, "partials/tagger_labels_rows.html", map[string]any{
			"Name":        name,
			"CustomRules": custom,
			"Err":         "Cannot read the label file: " + err.Error(),
		})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.FormValue("q")))
	filter := r.FormValue("filter")
	limit := taggerLabelPageSize
	if n, err := strconv.Atoi(r.FormValue("limit")); err == nil && n > limit {
		limit = min(n, taggerLabelMaxRows)
	}
	disabled := map[string]bool{}
	for _, cat := range inst.DisabledCategories {
		disabled[cat] = true
	}
	var rows []taggerLabelRow
	more := 0
	for _, v := range views {
		if q != "" && !strings.Contains(strings.ToLower(v.Source), q) && !strings.Contains(strings.ToLower(v.TagName), q) {
			continue
		}
		catOff := !v.Muted && disabled[v.CatName]
		switch filter {
		case "customized":
			if v.Rule != "custom" {
				continue
			}
		case "muted":
			if !v.Muted && !catOff {
				continue
			}
		}
		if len(rows) >= limit {
			more++
			continue
		}
		rows = append(rows, taggerLabelRow{
			Source:      v.Source,
			CatName:     v.CatName,
			TagName:     v.TagName,
			Color:       colors[v.CatName],
			Muted:       v.Muted,
			CategoryOff: catOff,
			Rule:        v.Rule,
		})
	}
	s.renderTemplate(w, "partials/tagger_labels_rows.html", map[string]any{
		"Name":        name,
		"Rows":        rows,
		"More":        more,
		"NextLimit":   limit + taggerLabelPageSize,
		"CanGrow":     limit < taggerLabelMaxRows,
		"CustomRules": custom,
	})
}

// taggerTagsFile mirrors the discovery fallback for instances whose
// TOML entry doesn't pin a label file.
func taggerTagsFile(inst config.TaggerInstance) string {
	if inst.TagsFile != "" {
		return inst.TagsFile
	}
	return tagger.DefaultTagsFile
}

// categoryChoices returns the gallery's categories as both the name→id
// map the dispatch compiler needs and a name-sorted list for the rule
// editor's category select.
func (s *Server) categoryChoices() (map[string]int64, []string) {
	ids := map[string]int64{}
	var names []string
	cats, err := s.tagSvc().ListCategories()
	if err != nil {
		return ids, names
	}
	for _, c := range cats {
		ids[c.Name] = c.ID
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return ids, names
}

// parseThresholdForm reads the thresholds panel out of the config form.
// A category row with an empty Threshold or Max-tags value clears the
// matching override. Every row submits its category hidden input; the
// per-row Enable checkbox is only present when ticked, so a category
// whose box is absent is muted. errMsg is non-empty on a validation
// failure.
func parseThresholdForm(r *http.Request) (global float64, overrides map[string]float64, topK map[string]int, disabled []string, errMsg string) {
	globalRaw := strings.TrimSpace(r.FormValue("global_threshold"))
	global, err := strconv.ParseFloat(globalRaw, 64)
	if err != nil || global < 0 || global > 1 {
		return 0, nil, nil, nil, "Global threshold must be between 0 and 1."
	}
	overrides = map[string]float64{}
	topK = map[string]int{}
	for _, cat := range r.Form["category"] {
		cat = strings.TrimSpace(cat)
		if cat == "" {
			continue
		}
		raw := strings.TrimSpace(r.FormValue("threshold_" + cat))
		if raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || v < 0 || v > 1 {
				return 0, nil, nil, nil, "Threshold for " + cat + " must be between 0 and 1."
			}
			overrides[cat] = v
		}
		rawK := strings.TrimSpace(r.FormValue("maxtags_" + cat))
		if rawK != "" {
			n, err := strconv.Atoi(rawK)
			if err != nil || n < 0 {
				return 0, nil, nil, nil, "Max tags for " + cat + " must be 0 or higher."
			}
			topK[cat] = n
		}
		if r.FormValue("enable_"+cat) == "" {
			disabled = append(disabled, cat)
		}
	}
	return global, overrides, topK, disabled, ""
}

// parseGalleriesForm reads the galleries panel out of the config form.
// Three submitted shapes:
//   - `all=on`                       → nil (every gallery, legacy)
//   - `all=off` with selected names  → those names
//   - `all=off` with no selection    → []string{} (no gallery, dormant)
//
// The explicit-empty case is preserved by storing a non-nil empty slice
// so the TOML round-trip writes `galleries = []` and AppliesToGallery
// returns false everywhere on the next read. Submitted names are
// filtered against the configured galleries so a stale form value can't
// poison the config.
func (s *Server) parseGalleriesForm(r *http.Request) []string {
	if r.FormValue("all") == "on" {
		return nil
	}
	galleries := []string{}
	valid := map[string]bool{}
	s.cfgMu.Lock()
	for _, g := range s.cfg.Galleries {
		valid[g.Name] = true
	}
	s.cfgMu.Unlock()
	for _, n := range r.Form["gallery_names"] {
		n = strings.TrimSpace(n)
		if n == "" || !valid[n] {
			continue
		}
		galleries = append(galleries, n)
	}
	return galleries
}

// settingsTaggerMappingPost applies one mappings-panel edit to the
// tagger's dispatch overlay and answers with the panel's result list
// re-read from disk, so the edited row comes back carrying the rule it
// now has. A validation failure retargets the swap at the dialog's
// flash slot: the list is left alone and the editor stays open on the
// value the operator has to fix.
func (s *Server) settingsTaggerMappingPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	modelPath := s.modelPath()
	if errMsg := s.applyMappingRule(name, modelPath, r); errMsg != "" {
		w.Header().Set("HX-Retarget", "#flash-tagger-config-"+name)
		writeInlineFlash(w, "err", errMsg)
		return
	}
	// Clear whatever error the previous attempt left in the dialog's
	// flash slot; the main swap owns the result list.
	writeFlashOOB(w, "flash-tagger-config-"+name, "", "")
	s.renderTaggerLabels(w, r, name)
}

// applyMappingRule folds one edit into the tagger's dispatch overlay
// and rewrites it. `rule_reset` drops the custom rule for the label,
// `rule_mute` stores a drop rule, otherwise rule_category / rule_name
// become the rule. errMsg is non-empty on a validation failure;
// nothing is written in that case.
func (s *Server) applyMappingRule(name, modelPath string, r *http.Request) (errMsg string) {
	source := strings.TrimSpace(r.FormValue("rule_source"))
	if source == "" {
		return "Malformed mapping rule."
	}
	// Everything the form can reject is checked before the overlay lock
	// so the held cycle is just read, swap one key, write.
	reset := r.FormValue("rule_reset") != ""
	entry := tagger.DispatchEntry{Source: source}
	if !reset && r.FormValue("rule_mute") == "" {
		category := r.FormValue("rule_category")
		catIDs, _ := s.categoryChoices()
		if _, ok := catIDs[category]; !ok {
			return "Unknown category " + category + " for label " + source + "."
		}
		rename := ""
		if n := strings.TrimSpace(r.FormValue("rule_name")); n != "" && n != source {
			valid, err := tags.ValidateTagName(n)
			if err != nil {
				return "Invalid rename for label " + source + ": " + err.Error()
			}
			rename = valid
		}
		entry = tagger.DispatchEntry{Source: source, Category: category, Name: rename}
	}
	if err := tagger.UpdateDispatchOverlay(modelPath, name, func(overlay map[string]tagger.DispatchEntry) error {
		if reset {
			delete(overlay, source)
		} else {
			overlay[source] = entry
		}
		return nil
	}); err != nil {
		return "Could not save dispatch.json: " + err.Error()
	}
	logx.Infof("settings: tagger %q mapping rule for %q updated", name, source)
	return ""
}

// settingsTaggerConfigPost saves the dialog's gallery scope and
// thresholds in one pass; mapping rules are written as they are
// applied and don't ride this form. On validation error the inline
// flash inside the dialog is updated and the dialog stays open; on
// success the page refreshes so the row summary, Reset button, and any
// state badges all reflect the new configuration.
func (s *Server) settingsTaggerConfigPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	global, overrides, topK, disabled, errMsg := parseThresholdForm(r)
	if errMsg != "" {
		writeInlineFlash(w, "err", errMsg)
		return
	}
	galleries := s.parseGalleriesForm(r)
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.ConfidenceThreshold = global
		if len(overrides) > 0 {
			t.CategoryThresholds = overrides
		} else {
			t.CategoryThresholds = nil
		}
		if len(topK) > 0 {
			t.PerCategoryTopK = topK
		} else {
			t.PerCategoryTopK = nil
		}
		t.DisabledCategories = disabled
		t.Galleries = galleries
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q config updated (global=%.2f, %d threshold overrides, %d top-K overrides, %d disabled, all_galleries=%t)",
		name, global, len(overrides), len(topK), len(disabled), galleries == nil)
	setFlashHeader(w, "Tagger "+name+" configuration saved.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

// settingsTaggerResetPost restores one tagger to stock: catalog-seeded
// thresholds, every gallery, and no dispatch overlay. The row's Reset
// button only renders when something differs, so this is always a
// deliberate act; the page refreshes so the button disappears with the
// state it reported.
func (s *Server) settingsTaggerResetPost(w http.ResponseWriter, r *http.Request) {
	name, ok := taggerNameAndForm(w, r)
	if !ok {
		return
	}
	defaults := tagger.SeedTaggerInstance(name, false, catalogEntryByName(s.modelPath(), name))
	if err := s.updateTagger(name, func(t *config.TaggerInstance) {
		t.ConfidenceThreshold = defaults.ConfidenceThreshold
		t.CategoryThresholds = defaults.CategoryThresholds
		t.PerCategoryTopK = defaults.PerCategoryTopK
		t.DisabledCategories = defaults.DisabledCategories
		t.Galleries = nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	overlay := filepath.Join(s.modelPath(), name, "dispatch.json")
	if err := os.Remove(overlay); err != nil && !os.IsNotExist(err) {
		logx.Warnf("reset tagger %q: remove %q: %v", name, overlay, err)
		writeInlineFlash(w, "err", "Reset saved but could not delete dispatch.json: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q reset to stock", name)
	setFlashHeader(w, "Tagger "+name+" reset to stock.", "ok", nil)
	w.Header().Set("HX-Refresh", "true")
}

// resolveTaggerInstance looks up the named tagger. Discovery covers
// both the configured instances and the subfolders without a TOML
// entry, and it is the only path that fills ModelFile / TagsFile from
// what is actually on disk - a seeded entry carries neither, so a
// tagger whose label file isn't the `tags.csv` default (joytag,
// camie-v2) would otherwise resolve to a filename that doesn't exist.
// ok=false means the tagger isn't in cfg or on disk.
func (s *Server) resolveTaggerInstance(name string) (config.TaggerInstance, bool) {
	for _, t := range tagger.DiscoverTaggers(s.cfgSnapshot()) {
		if t.Name == name {
			return t.TaggerInstance, true
		}
	}
	return config.TaggerInstance{}, false
}

// thresholdDialogData assembles the per-row state the template renders:
// the profile's natively emitted categories first, then the categories
// its dispatch rules route into (flagged ViaRules), then every other
// category on the gallery - so a category no shipped tagger reaches
// (person, species) is still tunable ahead of the dispatch rule that
// would use it. Each group is name-sorted. Stale overrides pointing at
// categories the gallery no longer has trail last so they stay
// clearable. global is the live ConfidenceThreshold. ok=false means the
// tagger isn't in cfg or on disk.
func (s *Server) thresholdDialogData(name string) (rows []thresholdRow, global float64, ok bool) {
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		return nil, 0, false
	}
	modelPath := s.modelPath()
	global = inst.ConfidenceThreshold

	profile, _ := tagger.ResolveProfile(modelPath, name, taggerTagsFile(inst))
	emit := profile.EmittedCategories()

	colors := s.categoryColors()

	// Catalog-seeded defaults drive the per-row Reset so it restores the
	// same per-category values the row-level Reset would
	// (see settingsTaggerResetPost).
	defaults := tagger.SeedTaggerInstance(name, false, catalogEntryByName(modelPath, name))

	seen := map[string]bool{}
	appendRow := func(cat string, viaRules bool) {
		if seen[cat] {
			return
		}
		seen[cat] = true
		rows = append(rows, thresholdRow{
			Category:         cat,
			Override:         formatOverride(inst.CategoryThresholds, cat),
			MaxTags:          formatTopKOverride(inst.PerCategoryTopK, cat),
			MaxDefault:       tagger.ResolveTopK(nil, cat),
			Color:            colors[cat],
			Disabled:         slices.Contains(inst.DisabledCategories, cat),
			ViaRules:         viaRules,
			DefaultThreshold: formatOverride(defaults.CategoryThresholds, cat),
			DefaultMaxTags:   formatTopKOverride(defaults.PerCategoryTopK, cat),
		})
	}
	appendSorted := func(cats []string, viaRules bool) {
		cats = append([]string(nil), cats...)
		sort.Strings(cats)
		for _, cat := range cats {
			appendRow(cat, viaRules)
		}
	}
	appendSorted(emit, false)
	var dispatchTargets []string
	for _, cat := range tagger.DispatchTargetCategories(modelPath, name) {
		if _, ok := colors[cat]; ok {
			dispatchTargets = append(dispatchTargets, cat)
		}
	}
	appendSorted(dispatchTargets, true)
	var rest []string
	for cat := range colors {
		rest = append(rest, cat)
	}
	appendSorted(rest, false)
	// Stale overrides (threshold, top-K, or disabled) pointing at
	// categories the gallery no longer has still render so the operator
	// can clear them.
	var stale []string
	for cat := range inst.CategoryThresholds {
		stale = append(stale, cat)
	}
	for cat := range inst.PerCategoryTopK {
		stale = append(stale, cat)
	}
	stale = append(stale, inst.DisabledCategories...)
	appendSorted(stale, false)
	return rows, global, true
}

func formatOverride(m map[string]float64, key string) string {
	if v, ok := m[key]; ok {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return ""
}

// formatTopKOverride mirrors formatOverride for the per-category cap.
// A missing key returns "" so the input shows the placeholder; an
// explicit zero returns "0" so the operator's opt-out persists.
func formatTopKOverride(m map[string]int, key string) string {
	if v, ok := m[key]; ok {
		return strconv.Itoa(v)
	}
	return ""
}

// taggerThresholdSummary renders the inline summary the table cell
// shows next to the Configure button: "global 0.40" or "global 0.40,
// character 0.85, copyright 0.50". Disabled categories trail in a
// "(disabled: ...)" group so a muted category is visible without
// opening the dialog. Both lists are sorted by category name so two
// equivalent configs render the same string.
func taggerThresholdSummary(global float64, overrides map[string]float64, disabled []string) string {
	out := fmt.Sprintf("global %.2f", global)
	for _, k := range slices.Sorted(maps.Keys(overrides)) {
		out += fmt.Sprintf(", %s %.2f", k, overrides[k])
	}
	if len(disabled) > 0 {
		d := append([]string(nil), disabled...)
		sort.Strings(d)
		out += " (disabled: " + strings.Join(d, ", ") + ")"
	}
	return out
}

// galleryDialogData returns one row per configured gallery, with
// Checked reflecting the tagger's current Galleries list. allChecked
// is true when Galleries is nil (legacy "every gallery") so the
// master toggle renders pre-ticked. A non-nil empty slice means
// "no galleries", which surfaces as the master toggle off and every
// row unchecked.
func (s *Server) galleryDialogData(name string) (rows []taggerGalleryRow, allChecked bool, ok bool) {
	inst, ok := s.resolveTaggerInstance(name)
	if !ok {
		return nil, false, false
	}
	s.cfgMu.Lock()
	galleries := append([]config.Gallery(nil), s.cfg.Galleries...)
	s.cfgMu.Unlock()
	allChecked = inst.Galleries == nil
	picked := map[string]bool{}
	for _, n := range inst.Galleries {
		picked[n] = true
	}
	for _, g := range galleries {
		rows = append(rows, taggerGalleryRow{
			Name:    g.Name,
			Checked: allChecked || picked[g.Name],
		})
	}
	return rows, allChecked, true
}

// catalogEntryByName looks up a catalog row by name, returning nil for
// taggers that aren't in the catalog (homegrown subfolders). Used by
// the per-row Enable / Disable handlers to seed catalog-supplied
// thresholds onto fresh TaggerInstance rows.
func catalogEntryByName(modelPath, name string) *tagger.CatalogEntry {
	for _, e := range tagger.LoadCatalog(modelPath) {
		if e.Name == name {
			entry := e
			return &entry
		}
	}
	return nil
}

// disableUnavailableTaggers flips Enabled to false on any configured tagger
// whose model files have gone missing on disk. Persists the result so a
// re-downloaded model has to be re-enabled deliberately rather than firing
// off a half-broken job.
func (s *Server) disableUnavailableTaggers() {
	available := map[string]bool{}
	for _, t := range tagger.DiscoverTaggers(s.cfgSnapshot()) {
		available[t.Name] = t.Available
	}
	s.cfgMu.Lock()
	changed := false
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Enabled && !available[t.Name] {
			s.cfg.Tagger.Taggers[i].Enabled = false
			changed = true
			logx.Infof("settings: auto-disabled tagger %q (files missing)", t.Name)
		}
	}
	s.cfgMu.Unlock()
	if changed {
		if err := s.saveConfig(); err != nil {
			logx.Warnf("auto-disable taggers: save config: %v", err)
		}
	}
}

// persistNewlyDiscoveredTaggers materialises a TOML entry for any
// available subfolder under model_path that has no entry yet, with
// Enabled=true and the catalog-supplied threshold defaults applied.
// DiscoverTaggers already surfaces these rows as enabled at render
// time, but the state was implicit (derived on the fly each call);
// persisting it makes the intent visible in the config file and
// removes the chance of a future code path treating "no TOML entry"
// as "not enabled".
func (s *Server) persistNewlyDiscoveredTaggers() {
	discovered := tagger.DiscoverTaggers(s.cfgSnapshot())
	modelPath := s.modelPath()
	s.cfgMu.Lock()
	known := make(map[string]bool, len(s.cfg.Tagger.Taggers))
	for _, t := range s.cfg.Tagger.Taggers {
		known[t.Name] = true
	}
	added := false
	for _, d := range discovered {
		if known[d.Name] || !d.Available {
			continue
		}
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers,
			tagger.SeedTaggerInstance(d.Name, true, catalogEntryByName(modelPath, d.Name)))
		known[d.Name] = true
		added = true
		logx.Infof("settings: auto-enabled discovered tagger %q", d.Name)
	}
	s.cfgMu.Unlock()
	if added {
		if err := s.saveConfig(); err != nil {
			logx.Warnf("auto-enable taggers: save config: %v", err)
		}
	}
}

// settingsTaggerDeletePost removes a tagger entry from the config and wipes
// its subfolder under paths.model_path. Refused if the tagger is currently
// enabled (the UI hides the button in that case; this is the server gate).
// The name is validated so it can't escape model_path with `..` segments.
func (s *Server) settingsTaggerDeletePost(w http.ResponseWriter, r *http.Request) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return
	}
	s.cfgMu.Lock()
	for _, t := range s.cfg.Tagger.Taggers {
		if t.Name == name && t.Enabled {
			s.cfgMu.Unlock()
			writeInlineFlash(w, "err", "Disable tagger "+name+" before deleting it.")
			return
		}
	}
	dir := filepath.Join(s.modelPath(), name)
	s.cfgMu.Unlock()
	// The folder goes first: dropping the entry before a removal that
	// then fails leaves memory and the TOML disagreeing, and the next
	// settings write persists the memory view.
	if err := os.RemoveAll(dir); err != nil {
		logx.Warnf("delete tagger %q: remove %q: %v", name, dir, err)
		writeInlineFlash(w, "err", "Could not delete the tagger folder: "+err.Error())
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.Tagger.Taggers = slices.DeleteFunc(c.Tagger.Taggers, func(t config.TaggerInstance) bool { return t.Name == name })
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: tagger %q deleted (folder %s removed)", name, dir)
	w.Header().Set("HX-Refresh", "true")
	writeInlineFlash(w, "ok", "Tagger "+name+" deleted.")
}
