package web

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// catTag pairs a resolved category ID with a tag name for creation/application.
type catTag struct {
	catID int64
	name  string
}

// parseTagInput parses multi-token tag input.
//
// Tokens are separated by whitespace. Each token becomes its own tag: a
// bare word, a "category:name" pair, or a double-quoted span whose
// internal spaces are collapsed to underscores (so `"red hair"` →
// `red_hair`). Quotes can follow a category prefix
// (`artist:"john doe"`).
//
// Examples:
//
//	red hair                 -> [{general, "red"}, {general, "hair"}]
//	"red hair" blue_eyes     -> [{general, "red_hair"}, {general, "blue_eyes"}]
//	artist:"john doe" 1girl  -> [{artist, "john_doe"}, {general, "1girl"}]
func (s *Server) parseTagInput(tagInput string) ([]catTag, []string, string) {
	tokens, err := splitTagTokens(tagInput)
	if err != nil {
		return nil, nil, err.Error()
	}

	// general category id is cached on galleryCtx at open time so this
	// hot path doesn't re-query the immutable built-in row.
	var generalID int64
	if cx := s.Active(); cx != nil {
		generalID = cx.GeneralCategoryID
	}

	categories, err := s.categoryIDsByName()
	if err != nil {
		return nil, nil, err.Error()
	}

	var catTags []catTag
	var rejected []string
	// A prefix that names no category is a literal tag name by design
	// ("nier:automata"), and it is also what a mistyped category looks
	// like. The caller says which reading it took rather than leaving the
	// operator with a catalog row they did not mean to create.
	var unknownCats []string
	for _, name := range tokens {
		if idx := strings.Index(name, ":"); idx > 0 {
			catName := name[:idx]
			tagName := name[idx+1:]
			if catID, ok := categories[catName]; ok {
				if tagName == "" {
					// `general:` (known category, empty name) was a silent
					// drop; surface it like the other malformed-token cases
					// so the user sees what their input did.
					rejected = append(rejected, "rejected: "+name+": empty tag name after category prefix")
					continue
				}
				catTags = append(catTags, catTag{catID, tagName})
				continue
			}
			// Prefix isn't a known category; treat the whole token as a
			// literal general-category tag (e.g. "nier:automata").
			if !slices.Contains(unknownCats, catName) {
				unknownCats = append(unknownCats, catName)
			}
		}
		catTags = append(catTags, catTag{generalID, name})
	}

	return catTags, unknownCats, strings.Join(rejected, "; ")
}

// unknownCategoryNote names the prefixes parseTagInput read as part of a
// tag name rather than as a category.
func unknownCategoryNote(unknownCats []string) string {
	return joinLabeled("no category named ", ", ", unknownCats)
}

// categoryIDsByName preloads every category in one read so a
// multi-token paste with category prefixes doesn't pay N read-pool
// round-trips. The tag_categories row count is tiny (single-digit
// builtins + a handful of user rows) so the map fits in a single small
// alloc. A truncated read would drop a category and silently reparse
// `character:foo` as a literal general tag, so a cursor error is
// surfaced rather than swallowed.
func (s *Server) categoryIDsByName() (map[string]int64, error) {
	type catRow struct {
		id   int64
		name string
	}
	cats, err := db.QueryAll(s.db().Read, func(rows *sql.Rows) (catRow, error) {
		var c catRow
		err := rows.Scan(&c.id, &c.name)
		return c, err
	}, `SELECT id, name FROM tag_categories`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(cats))
	for _, c := range cats {
		out[c.name] = c.id
	}
	return out, nil
}

// splitTagTokens splits tag-input into whitespace-separated tokens while
// respecting double-quoted spans. Inside a quoted span, internal spaces
// are replaced with underscores. Quoted spans may be preceded by a
// category prefix (`artist:"john doe"`). Unterminated quotes return an
// error.
func splitTagTokens(s string) ([]string, error) {
	var tokens []string
	var buf strings.Builder
	quoted := false
	inToken := false

	flush := func() {
		if !inToken {
			return
		}
		tokens = append(tokens, buf.String())
		buf.Reset()
		inToken = false
	}

	for _, r := range s {
		if r == '"' {
			quoted = !quoted
			inToken = true
			continue
		}
		if quoted {
			if r == ' ' || r == '\t' {
				buf.WriteRune('_')
			} else {
				buf.WriteRune(r)
			}
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			flush()
			continue
		}
		buf.WriteRune(r)
		inToken = true
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quote in tag input")
	}
	flush()
	return tokens, nil
}

func (s *Server) addTagToImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tag"))
	if tagInput == "" {
		http.Error(w, "tag required", http.StatusBadRequest)
		return
	}

	catTags, unknownCats, parseErrMsg := s.parseTagInput(tagInput)

	var added, rejected, dupes []string
	var promotedTokens []string
	var displacedRatings []string
	mutated := false

	// Resolve every token up front so the inserts ride one writer
	// round-trip; a 50-token paste pays one transaction instead of N.
	type resolved struct {
		name string
		tag  *models.Tag
	}
	prepared := make([]resolved, 0, len(catTags))
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			logx.Warnf("add tag %q: %v", ct.name, err)
			rejected = append(rejected, ct.name+": "+err.Error())
			continue
		}
		prepared = append(prepared, resolved{name: ct.name, tag: tag})
	}

	if len(prepared) > 0 {
		tagIDs := make([]int64, len(prepared))
		for i, p := range prepared {
			tagIDs[i] = p.tag.ID
		}
		results, err := s.tagSvc().AddTagsToOneImage(id, tagIDs, "")
		if err != nil {
			logx.Warnf("batch add tags to image %d: %v", id, err)
			// On batch failure surface a single rejection covering every
			// prepared token so the user knows none landed.
			for _, p := range prepared {
				rejected = append(rejected, p.name+": "+err.Error())
			}
		} else {
			for i, res := range results {
				name := prepared[i].name
				if res.Added || res.Promoted {
					mutated = true
				}
				if res.Added && !res.Promoted {
					added = append(added, name)
				}
				if !res.Added && !res.Promoted {
					dupes = append(dupes, name)
				}
				if res.Promoted {
					promotedTokens = append(promotedTokens, name)
				}
				displacedRatings = append(displacedRatings, res.DisplacedRatings...)
			}
		}
	}

	if mutated {
		s.Active().InvalidateCaches()
		w.Header().Set("HX-Trigger", "tags-changed")
	}

	// Distinguish "everything went in" from "some tokens failed" so a
	// pasted multi-token input doesn't leave the user diffing the under-
	// image list against their string. The input is cleared on full
	// success and on a clean partial (some applied, some duplicates):
	// the user can read the live tag list to confirm what's there. It
	// stays populated only when at least one token was rejected, so the
	// user can edit and resubmit.
	//
	// Three flash buckets the template renders in three colours: red
	// (errors only), orange (mixed success + reject), green (success
	// only). Build the parts once, then route them.
	addedPart := joinLabeled("added: ", ", ", added)
	promotedPart := joinLabeled("promoted to user tag: ", ", ", promotedTokens)
	dupesPart := ""
	if mutated {
		dupesPart = joinLabeled("already on image: ", ", ", dupes)
	}
	displacedPart := joinLabeled("replaced rating ", ", ", displacedRatings)
	rejectedPart := joinLabeled("rejected: ", "; ", rejected)
	unknownPart := unknownCategoryNote(unknownCats)

	joinNonEmpty := func(parts ...string) string {
		return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), "; ")
	}

	var addErrMsg, addWarnMsg, addOkMsg string
	switch {
	case parseErrMsg != "" && !mutated && len(rejected) == 0:
		addErrMsg = parseErrMsg
	case mutated && (len(rejected) > 0 || parseErrMsg != ""):
		// Mixed outcome: render in warn-orange and surface both
		// successes and the rejected tokens.
		addWarnMsg = joinNonEmpty(parseErrMsg, addedPart, promotedPart, dupesPart, displacedPart, unknownPart, rejectedPart)
	case len(rejected) > 0:
		addErrMsg = joinNonEmpty(parseErrMsg, rejectedPart)
	case len(dupes) > 0 && !mutated && parseErrMsg == "":
		// Whole submit hit only existing tags; preserve the prior
		// soft-error feedback so the user sees something happened.
		addErrMsg = "tag already on image: " + strings.Join(dupes, ", ")
	default:
		addOkMsg = joinNonEmpty(addedPart, promotedPart, dupesPart, displacedPart, unknownPart)
	}
	s.renderTagListWithSidebar(w, r, id, addErrMsg, addWarnMsg, addOkMsg, len(rejected) == 0 && parseErrMsg == "")
}

// renderTagListWithSidebar renders the image tag list partial and always emits
// OOB swaps of the detail sidebar and danger zone so tag groups and remove-tag
// buttons stay in sync without a page reload.
// errMsg / warnMsg / okMsg are shown as inline flashes if non-empty (red,
// orange, green); clearInput resets the add-tag input.
func (s *Server) renderTagListWithSidebar(w http.ResponseWriter, r *http.Request, id int64, errMsg, warnMsg, okMsg string, clearInput bool) {
	folderPath, imageTags, _ := s.tagSvc().GetImageTags(id)
	csrfToken := s.csrfToken(sessionFromContext(r.Context()))
	// The sidebar's grouping toggle rides this refresh: an explicit
	// tagmode switches the view and is stored for the next render.
	tagMode := readTagModeCookie(r)
	if requested := r.URL.Query().Get("tagmode"); requested != "" {
		tagMode = normalizeTagMode(requested)
		writeTagModeCookie(w, tagMode)
	}
	hasUserTags, hasStaleTags := userAndStaleTags(imageTags)
	back := parseBackContext(r)
	// The footer's tally is a per-render snapshot everywhere else; here
	// the swap that lands the tag can move it, so it rides along. The
	// callers invalidate before rendering, so this reads post-write.
	tagCount := 0
	if cx := s.Active(); cx != nil {
		tagCount, _ = cx.TagCount()
	}
	var canonicalPath string
	_ = s.db().Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonicalPath)
	// The delete confirm names the copies that go with the row, and this
	// fragment re-renders it out of band. Counted the way the Duplicates
	// panel counts, so the two never disagree.
	extraPaths := max(len(loadImagePaths(r.Context(), s.db(), id))-1, 0)
	filename := ""
	if canonicalPath != "" {
		filename = filepath.Base(canonicalPath)
	}
	s.renderTemplate(w, "partials/tag_list.html", map[string]any{
		"ImageID":          id,
		"ImageTags":        imageTags,
		"TagSidebar":       s.buildTagSidebar(id, csrfToken, tagMode, imageTags),
		"SidebarTags":      true,
		"SidebarCollapsed": sidebarCollapsed(r),
		"DangerZone":       true,
		"HasUserTags":      hasUserTags,
		"HasStaleTags":     hasStaleTags,
		"ImageTaggers":     distinctTaggerNames(imageTags, true),
		"ImageSources":     distinctTaggerNames(imageTags, false),
		"CanTransfer":      len(s.galleryList()) > 1,
		"BackQuery":        back.Q,
		"BackSort":         back.Sort,
		"BackOrder":        back.Order,
		"BackPage":         back.Page,
		"BackSeed":         back.Seed,
		"CSRFToken":        csrfToken,
		"EditMode":         true,
		"ErrMsg":           errMsg,
		"WarnMsg":          warnMsg,
		"OkMsg":            okMsg,
		"ClearInput":       clearInput,
		"CurrentFolder":    folderPath,
		"Filename":         filename,
		"ExtraPaths":       extraPaths,
		"TagCount":         tagCount,
	})
}

// trimmedValues normalises a repeated query parameter: whitespace is
// stripped and empty entries drop out. Repeated rather than
// comma-joined so a label carrying a comma survives the round trip.
func trimmedValues(raw []string) []string {
	var out []string
	for _, v := range raw {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// removeAutoTagsFromImageHandler removes auto-tagged rows from one image,
// optionally filtered by repeated `taggers` query parameters. An absent
// filter removes every auto-tag.
func (s *Server) removeAutoTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	names := trimmedValues(r.URL.Query()["taggers"])
	s.removeImageTagsHandler(w, r, func(id int64) (int, error) {
		return s.tagSvc().RemoveAutoTagsFromImage(id, names)
	})
}

// removeSourceTagsFromImageHandler removes the tags one or more external
// sources contributed to one image, filtered by repeated `sources` query
// parameters. The optional `stale` value ("1" / "0") narrows to one of
// the source's two detail-page groups.
func (s *Server) removeSourceTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	names := trimmedValues(r.URL.Query()["sources"])
	stale := r.URL.Query().Get("stale")
	if stale != "0" && stale != "1" {
		stale = ""
	}
	s.removeImageTagsHandler(w, r, func(id int64) (int, error) {
		return s.tagSvc().RemoveSourceTagsFromImage(id, names, stale)
	})
}

// removeCategoryTagsFromImageHandler removes the image's own tags in one
// category, the sidebar's per-category bulk action.
func (s *Server) removeCategoryTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	category := strings.TrimSpace(r.URL.Query().Get("cat"))
	s.removeImageTagsHandler(w, r, func(id int64) (int, error) {
		if category == "" {
			return 0, nil
		}
		return s.tagSvc().RemoveCategoryTagsFromImage(id, category)
	})
}

// dropSourceContributionHandler withdraws one source's claim on the
// image's tags - the by-source sidebar's group and row buttons. An
// optional repeated `tag` narrows it to those tags; without one the
// whole source backs out. A tag another source also vouches for stays
// on the image and only leaves that source's group.
func (s *Server) dropSourceContributionHandler(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	var tagIDs []int64
	for _, raw := range r.URL.Query()["tag"] {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			tagIDs = append(tagIDs, id)
		}
	}
	s.removeImageTagsWithMsg(w, r, func(id int64) (string, error) {
		if source == "" {
			return "", nil
		}
		covered, removed, err := s.tagSvc().DropSourceFromImageTags(id, source, tagIDs)
		return droppedSourceMsg(source, covered, removed), err
	})
}

// droppedSourceMsg names what a withdrawal did: tags that lost their
// last source left the image, the rest only left that source's group,
// and the two counts are what tells them apart.
func droppedSourceMsg(source string, covered, removed int) string {
	switch {
	case covered == 0:
		return ""
	case removed == covered:
		return removedTagsMsg(removed)
	case removed == 0:
		return fmt.Sprintf("dropped %s from %d tag%s", source, covered, plural(covered))
	default:
		return fmt.Sprintf("dropped %s from %d tag%s, %d removed", source, covered, plural(covered), removed)
	}
}

func (s *Server) removeUserTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	s.removeImageTagsHandler(w, r, s.tagSvc().RemoveUserTagsFromImage)
}

func (s *Server) removeAllTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	// RemoveAllTagsFromImage drops the rows in one statement (it is also the
	// image-delete callback), so the count for the flash comes from a probe.
	s.removeImageTagsHandler(w, r, func(id int64) (int, error) {
		var n int
		if err := s.db().Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ?`, id).Scan(&n); err != nil {
			return 0, err
		}
		return n, s.tagSvc().RemoveAllTagsFromImage(id)
	})
}

func (s *Server) removeStaleTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	s.removeImageTagsHandler(w, r, s.tagSvc().RemoveStaleTagsFromImage)
}

// removedTagsMsg is the removal counterpart of the add path's
// "added: ..." flash. A removal that matched nothing says nothing.
func removedTagsMsg(removed int) string {
	switch removed {
	case 0:
		return ""
	case 1:
		return "removed 1 tag"
	default:
		return fmt.Sprintf("removed %d tags", removed)
	}
}

// removeImageTagsHandler is the parse-id / call-remove / refresh body
// shared by the bulk-remove tag handlers; remove names the underlying
// service method.
func (s *Server) removeImageTagsHandler(w http.ResponseWriter, r *http.Request, remove func(int64) (int, error)) {
	s.removeImageTagsWithMsg(w, r, func(id int64) (string, error) {
		removed, err := remove(id)
		return removedTagsMsg(removed), err
	})
}

// removeImageTagsWithMsg is removeImageTagsHandler for the paths whose
// flash is more than a count of removed rows.
func (s *Server) removeImageTagsWithMsg(w http.ResponseWriter, r *http.Request, remove func(int64) (string, error)) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	msg, err := remove(id)
	if err != nil {
		s.renderTagListWithSidebar(w, r, id, err.Error(), "", "", false)
		return
	}
	s.Active().InvalidateCaches()
	w.Header().Set("HX-Trigger", "tags-changed")
	s.renderTagListWithSidebar(w, r, id, "", "", msg, false)
}

func (s *Server) removeTagFromImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tagID, ok := pathInt64(w, r, "tagID")
	if !ok {
		return
	}

	// Read the name first: the flash names what the click removed, the way
	// the add path names what it added.
	var name string
	_ = s.db().Read.QueryRow(`SELECT name FROM tags WHERE id = ?`, tagID).Scan(&name)

	if err := s.tagSvc().RemoveTagFromImage(id, tagID); err != nil {
		s.renderTagListWithSidebar(w, r, id, err.Error(), "", "", false)
		return
	}
	s.Active().InvalidateCaches()
	w.Header().Set("HX-Trigger", "tags-changed")
	okMsg := "removed 1 tag"
	if name != "" {
		okMsg = "removed: " + name
	}
	s.renderTagListWithSidebar(w, r, id, "", "", okMsg, false)
}

// joinLabeled renders "<label><items joined by sep>", or "" when there
// is nothing to label.
func joinLabeled(label, sep string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	return label + strings.Join(items, sep)
}

func (s *Server) changeTagCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	catIDStr := r.FormValue("category_id")
	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad category_id", http.StatusBadRequest)
		return
	}
	// Route through the tag service for validation and consistency.
	var svcErr error
	merged := false
	if r.FormValue("merge") == "1" {
		merged, svcErr = s.tagSvc().ChangeTagCategoryMerge(id, catID)
	} else {
		svcErr = s.tagSvc().ChangeTagCategory(id, catID)
	}
	if svcErr != nil {
		var coll *tags.ErrCategoryCollision
		if errors.As(svcErr, &coll) && isHTMXRequest(r) {
			// Offer the merge instead of dead-ending: the survivor keeps
			// the images, the moving tag becomes its alias.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w,
				`<div class="flash flash-err">%s <button type="button" class="btn-sm" onclick="mergeCategoryCollision(%d, %d)">Merge into it</button></div>`,
				template.HTMLEscapeString(svcErr.Error()), id, catID)
			return
		}
		if isHTMXRequest(r) {
			writeInlineFlash(w, "err", svcErr.Error())
			return
		}
		http.Error(w, svcErr.Error(), http.StatusInternalServerError)
		return
	}
	// cat:/category-qualified searches resolve via the moved tag's
	// new category, so cached match-id lists for those queries can't
	// survive the move.
	s.Active().InvalidateCaches()
	if isHTMXRequest(r) {
		if merged {
			writeInlineFlash(w, "ok", "Merged into the existing tag.")
			return
		}
		writeInlineFlash(w, "ok", "Category updated.")
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) getImageTagsHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", "", "", false)
}
