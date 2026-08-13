package web

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// maxTagDepth caps how deep an implication chain indents. The sidebar
// column is narrow and tag names are long, so a deeper tail renders at
// the last level rather than walking off the edge.
const maxTagDepth = 3

// categoryRank orders the seeded categories for reading: the small
// metadata block first, then the image's content, most identifying
// first. Categories the operator created sort alphabetically between
// the content set and general, which stays last as the biggest group.
var categoryRank = map[string]int{
	"rating": 0, "year": 1, "meta": 2, "medium": 3,
	"person": 4, "artist": 5, "copyright": 6, "character": 7,
	"species": 8, "general": 10,
}

const customCategoryRank = 9

// tagSidebarRow is one line of the detail sidebar's tag list.
type tagSidebarRow struct {
	models.ImageTag
	// Depth is 0 for a tag on the image and 1..maxTagDepth for one the
	// implication graph fanned out under the row above it.
	Depth int
	// Marker is the single annotation the row carries: a confidence
	// percentage when nothing but an auto-tagger vouches for the tag, a
	// source count when several do, or the stale flag.
	Marker     string
	MarkerKind string
	// MarkerHint lists every source that applied or re-confirmed the tag.
	MarkerHint string
	// NameHint is the row's hover: the tag in full (the column ellipsises
	// long names) and then the spellings aliased to it. An implied row
	// names the parent it hangs under instead.
	NameHint string
	// Source is the writer the row is attributed to: where it belongs in
	// the by-source view, and what the "Just added" list keys off.
	Source string
	// RemoveURL is what the row's button calls. In the by-source view it
	// withdraws that source's claim rather than deleting the tag, so a
	// tag another source also vouches for stays on the image.
	RemoveURL string
}

// Removable reports whether the row owns a remove button. An implied row
// goes when the parent justifying it goes, so it has none of its own.
func (r tagSidebarRow) Removable() bool { return !r.IsImplied }

// tagSidebarSection is one group of the sidebar list: a category in the
// default view, a source in the by-source view.
type tagSidebarSection struct {
	Name  string
	Color string
	Rows  []tagSidebarRow
	// DeleteURL clears the whole section, whatever it holds - a one-tag
	// category is still a category, and hunting for the row button
	// instead would be a different gesture for the same intent.
	DeleteURL   string
	DeleteCount int
	// DeleteShared counts the section's rows another source also vouches
	// for. Those survive the withdrawal - they only stop being listed
	// here - so the confirm can say how much of it stays.
	DeleteShared int
}

// Tag sidebar view modes. The default groups by category; the operator
// can switch to grouping by the sources that applied each tag.
const (
	tagModeCategory = "category"
	tagModeSource   = "source"
)

const tagModeCookieName = "monbooru_tag_mode"

// tagSidebar is everything the detail sidebar's tag section renders.
// Carried whole so the full page render, the pages grid and the htmx
// tag refresh all feed the partial the same shape.
type tagSidebar struct {
	ImageID    int64
	CSRFToken  string
	Sections   []tagSidebarSection
	StaleCount int
	// Mode names the grouping in effect. The header offers both, with the
	// active one rendered as plain text.
	Mode string
}

// normalizeTagMode drops anything outside the closed pair to the
// default, so a stale cookie or a hand-edited URL can't ask for a view
// that does not exist.
func normalizeTagMode(v string) string {
	if v == tagModeSource {
		return tagModeSource
	}
	return tagModeCategory
}

// readTagModeCookie returns the operator's stored grouping.
func readTagModeCookie(r *http.Request) string {
	c, err := r.Cookie(tagModeCookieName)
	if err != nil {
		return tagModeCategory
	}
	return normalizeTagMode(c.Value)
}

func writeTagModeCookie(w http.ResponseWriter, mode string) {
	if mode == tagModeSource {
		http.SetCookie(w, &http.Cookie{
			Name:     tagModeCookieName,
			Value:    tagModeSource,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   31_536_000,
			SameSite: http.SameSiteLaxMode,
		})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: tagModeCookieName, Value: "", Path: "/", MaxAge: -1})
}

// buildTagSidebar reads the provenance an image's tags carry and lays
// them out for the sidebar: one section per category, natural order
// within one, and each implied tag under the parent that fanned it out.
func (s *Server) buildTagSidebar(imageID int64, csrfToken, mode string, imageTags []models.ImageTag) tagSidebar {
	sb := tagSidebar{ImageID: imageID, CSRFToken: csrfToken, Mode: mode}
	if len(imageTags) == 0 {
		return sb
	}
	ledger, err := s.tagSvc().TagSourcesForImage(imageID)
	if err != nil {
		logx.Warnf("TagSourcesForImage(%d): %v", imageID, err)
	}
	taggerNames := distinctTaggerNames(imageTags, true)
	aliases := s.aliasNames(imageTags)

	rows := make(map[int64]tagSidebarRow, len(imageTags))
	for _, t := range imageTags {
		r := tagSidebarRow{
			ImageTag:  t,
			NameHint:  nameHint(t.TagName, aliases[t.TagID]),
			Source:    rowSource(t),
			RemoveURL: fmt.Sprintf("/images/%d/tags/%d", imageID, t.TagID),
		}
		annotateTagRow(&r, ledger[t.TagID], taggerNames)
		if t.Stale {
			sb.StaleCount++
		}
		rows[t.TagID] = r
	}
	implied := s.impliedUnder(imageTags)
	if mode == tagModeSource {
		sb.Sections = groupTagRowsBySource(imageID, imageTags, rows, implied, ledger, taggerNames)
	} else {
		sb.Sections = groupTagRowsByCategory(imageID, imageTags, rows, implied)
	}
	return sb
}

// groupTagRowsBySource lays the rows out by the sources that applied
// them: the operator first, then sites, then auto-taggers. A tag several
// sources vouch for is listed under each, so the repetition itself is
// the agreement - which is why the row drops the source-count marker it
// carries in the category view.
func groupTagRowsBySource(imageID int64, imageTags []models.ImageTag,
	rows map[int64]tagSidebarRow, implied map[int64][]int64,
	ledger map[int64][]tags.TagSource, taggerNames []string) []tagSidebarSection {

	byLabel := map[string][]models.ImageTag{}
	shared := map[string]int{}
	for _, t := range imageTags {
		if t.IsImplied {
			// Fan-out rows have no ledger of their own; they ride the
			// parent that justifies them, wherever it is listed.
			continue
		}
		labels := ledger[t.TagID]
		if len(labels) == 0 {
			// No ledger row: fall back to the row's own attribution, the
			// same rule the ledger backfill used, so the withdrawal SQL
			// and this listing agree on where the tag belongs.
			fallback := cmp.Or(t.TaggerName, "user")
			byLabel[fallback] = append(byLabel[fallback], t)
			continue
		}
		for _, src := range labels {
			byLabel[src.Source] = append(byLabel[src.Source], t)
			if len(labels) > 1 {
				shared[src.Source]++
			}
		}
	}

	names := slices.SortedFunc(maps.Keys(byLabel), func(a, b string) int {
		if ra, rb := rankSource(a, taggerNames), rankSource(b, taggerNames); ra != rb {
			return cmp.Compare(ra, rb)
		}
		return strings.Compare(a, b)
	})

	out := make([]tagSidebarSection, 0, len(names))
	for _, name := range names {
		group := byLabel[name]
		sortForDisplay(group)
		section := tagSidebarSection{
			Name:         name,
			DeleteCount:  len(group),
			DeleteShared: shared[name],
		}
		// Each group nests its own copy of the subtrees, so a parent
		// listed under three sources brings its implied tags along all
		// three times.
		attached := map[int64]bool{}
		for _, t := range group {
			row := rows[t.TagID]
			if row.MarkerKind == "sources" {
				row.Marker, row.MarkerKind = "", ""
			}
			row.RemoveURL = fmt.Sprintf("/images/%d/source-contribution?source=%s&tag=%d",
				imageID, url.QueryEscape(name), t.TagID)
			section.Rows = append(section.Rows, row)
			section.Rows = append(section.Rows, nestImplied(nil, t.TagID, 1, rows, implied, attached)...)
		}
		if section.DeleteCount > 0 {
			section.DeleteURL = fmt.Sprintf("/images/%d/source-contribution?source=%s", imageID, url.QueryEscape(name))
		}
		out = append(out, section)
	}
	return out
}

// rankSource puts the operator's own tags first and the auto-taggers
// last, so the by-source list reads from most to least deliberate.
func rankSource(name string, taggerNames []string) int {
	switch {
	case name == "user":
		return 0
	case slices.Contains(taggerNames, name):
		return 2
	default:
		return 1
	}
}

// nameHint spells the row's hover. The name comes first because the
// column ellipsises anything long, and that is what the operator is
// hovering to read.
func nameHint(name, aliases string) string {
	if aliases == "" {
		return name
	}
	return name + "\naliases: " + aliases
}

// rowSource names the writer a row is attributed to.
func rowSource(t models.ImageTag) string {
	switch {
	case t.IsImplied:
		return "implied"
	case t.IsAuto:
		return "auto"
	case t.TaggerName != "":
		return t.TaggerName
	default:
		return "user"
	}
}

// annotateTagRow picks the row's one marker. A stale tag says so first -
// it is the fact the operator acts on. Otherwise a tag only an
// auto-tagger vouches for shows how sure the model was, and a tag
// several sources agree on shows how many.
func annotateTagRow(r *tagSidebarRow, sources []tags.TagSource, taggerNames []string) {
	r.MarkerHint = sourceHint(sources, r.TaggerName, r.Confidence)
	switch {
	case r.Stale:
		r.Marker, r.MarkerKind = "stale", "stale"
	case autoOnly(r.ImageTag, sources, taggerNames) && r.Confidence != nil:
		r.Marker, r.MarkerKind = strconv.Itoa(int(*r.Confidence*100))+"%", "conf"
	case len(sources) > 1:
		r.Marker, r.MarkerKind = "x"+strconv.Itoa(len(sources)), "sources"
	}
}

// autoOnly reports whether an auto-tagger is the only thing vouching for
// the tag. A row the ledger has no entry for falls back to its own
// attribution, which is all the pre-ledger rows carry.
func autoOnly(t models.ImageTag, sources []tags.TagSource, taggerNames []string) bool {
	if !t.IsAuto {
		return false
	}
	for _, src := range sources {
		if !slices.Contains(taggerNames, src.Source) {
			return false
		}
	}
	return true
}

// sourceHint lists every source that applied or re-confirmed the tag,
// one per line, with the confidence beside the tagger that inferred it.
func sourceHint(sources []tags.TagSource, taggerName string, confidence *float64) string {
	lines := make([]string, 0, len(sources))
	for _, src := range sources {
		line := src.Source
		// Ledger rows are stamped ISO; older backfilled ones carry the
		// image_tags spelling with a space. The date is the useful part.
		if i := strings.IndexAny(src.CreatedAt, "T "); i > 0 {
			line += "  " + src.CreatedAt[:i]
		}
		if confidence != nil && src.Source == taggerName {
			line += fmt.Sprintf("  %d%%", int(*confidence*100))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// groupTagRowsByCategory lays the rows out as the sidebar renders them:
// categories in reading order, tags in natural order within one, each
// implied tag directly under the parent that brought it.
func groupTagRowsByCategory(imageID int64, imageTags []models.ImageTag,
	rows map[int64]tagSidebarRow, implied map[int64][]int64) []tagSidebarSection {

	tops := make([]models.ImageTag, 0, len(imageTags))
	for _, t := range imageTags {
		if !t.IsImplied {
			tops = append(tops, t)
		}
	}
	// Nest in display order, so a tag two parents imply lands under the
	// one the operator reads first rather than whichever the query
	// happened to return first.
	sortForDisplay(tops)
	attached := map[int64]bool{}
	subtrees := make(map[int64][]tagSidebarRow, len(tops))
	for _, t := range tops {
		subtrees[t.TagID] = nestImplied(nil, t.TagID, 1, rows, implied, attached)
	}
	// An implied tag whose parents have all gone - a propagation job still
	// catching up - heads its own entry rather than disappearing.
	for _, t := range imageTags {
		if t.IsImplied && !attached[t.TagID] {
			tops = append(tops, t)
		}
	}
	sortForDisplay(tops)

	var out []tagSidebarSection
	for _, t := range tops {
		if len(out) == 0 || out[len(out)-1].Name != t.Category {
			out = append(out, tagSidebarSection{Name: t.Category, Color: t.Color})
		}
		section := &out[len(out)-1]
		section.Rows = append(section.Rows, rows[t.TagID])
		section.Rows = append(section.Rows, subtrees[t.TagID]...)
		if !t.IsImplied {
			section.DeleteCount++
		}
	}
	for i := range out {
		if out[i].DeleteCount > 0 {
			out[i].DeleteURL = fmt.Sprintf("/images/%d/category-tags?cat=%s", imageID, url.QueryEscape(out[i].Name))
		}
	}
	return out
}

// nestImplied walks the implication graph down from parent, appending
// each implied tag the image carries under the row that brought it. A
// tag reached from two parents renders under the first, so the list
// stays one line per tag.
func nestImplied(out []tagSidebarRow, parentID int64, depth int,
	rows map[int64]tagSidebarRow, implied map[int64][]int64, attached map[int64]bool) []tagSidebarRow {

	for _, childID := range implied[parentID] {
		if attached[childID] {
			continue
		}
		attached[childID] = true
		r := rows[childID]
		r.Depth = min(depth, maxTagDepth)
		r.NameHint += "\nimplied by " + rows[parentID].TagName
		out = append(out, r)
		out = nestImplied(out, childID, depth+1, rows, implied, attached)
	}
	return out
}

// sortForDisplay orders tags the way the sidebar reads them: categories
// in reading order, names naturally within one so a tag is where the
// alphabet says it is rather than where its global usage ranks it.
func sortForDisplay(list []models.ImageTag) {
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rankCategory(list[i].Category), rankCategory(list[j].Category)
		if ri != rj {
			return ri < rj
		}
		if list[i].Category != list[j].Category {
			return list[i].Category < list[j].Category
		}
		return gallery.NaturalLess(strings.ToLower(list[i].TagName), strings.ToLower(list[j].TagName))
	})
}

// rankCategory places a category in the reading order; anything the
// operator created sorts alphabetically ahead of general.
func rankCategory(name string) int {
	if r, ok := categoryRank[name]; ok {
		return r
	}
	return customCategoryRank
}

// aliasNames maps each of the image's tags to the alias spellings
// pointing at it, joined for the row's hover.
func (s *Server) aliasNames(imageTags []models.ImageTag) map[int64]string {
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		if !t.IsImplied {
			ids = append(ids, t.TagID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	byCanonical, err := s.tagSvc().AliasesForTagIDs(ids)
	if err != nil {
		logx.Warnf("AliasesForTagIDs: %v", err)
		return nil
	}
	out := make(map[int64]string, len(byCanonical))
	for id, list := range byCanonical {
		names := make([]string, len(list))
		for i, a := range list {
			names[i] = a.Name
		}
		sort.Strings(names)
		out[id] = strings.Join(names, ", ")
	}
	return out
}

// impliedUnder maps each on-image tag to the implied tags it justifies,
// so the sidebar can nest a fanned-out tag under the parent that
// brought it instead of listing it loose.
func (s *Server) impliedUnder(imageTags []models.ImageTag) map[int64][]int64 {
	onImage := make(map[int64]bool, len(imageTags))
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		onImage[t.TagID] = t.IsImplied
		ids = append(ids, t.TagID)
	}
	edges, err := s.tagSvc().ImplicationsForParents(ids)
	if err != nil {
		logx.Warnf("ImplicationsForParents: %v", err)
		return nil
	}
	out := make(map[int64][]int64, len(edges))
	for parent, list := range edges {
		for _, im := range list {
			if isImplied, ok := onImage[im.ImpliedID]; ok && isImplied {
				out[parent] = append(out[parent], im.ImpliedID)
			}
		}
	}
	return out
}
