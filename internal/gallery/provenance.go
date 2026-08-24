package gallery

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// MergeSummary reports what a push or a refetch folded into an image that
// was already in the gallery.
type MergeSummary struct {
	TagsAdded    int  `json:"tags_added"`
	TagsRetired  int  `json:"tags_retired"`
	RatingFilled bool `json:"rating_filled"`
	SourceAdded  bool `json:"source_added"`
}

// WriteSourceProvenance records one origin row: membership plus its md5,
// post-file facts and parent-URL columns, keyed by (source, postID).
func WriteSourceProvenance(database *db.DB, imageID int64, source, postID, url, md5, parentURL string, post PostFile) error {
	if err := AddSourceMembership(database, imageID, source, postID, url); err != nil {
		return err
	}
	if err := SetSourceMD5(database, imageID, source, postID, md5); err != nil {
		return err
	}
	if err := SetSourcePostFile(database, imageID, source, postID, post); err != nil {
		return err
	}
	return SetSourceParentURL(database, imageID, source, postID, parentURL)
}

// ApplySourceProvenance writes the per-source commentary, original, and
// annotations an enrich or duplicate-merge push carries, skipping empty
// values. On failure the returned step names what was being applied so
// each caller can map the error to its own reporting.
func ApplySourceProvenance(database *db.DB, imageID int64, source, postID, commentary, original string, notes []models.Annotation) (string, error) {
	if source == "" {
		return "", nil
	}
	if commentary != "" {
		if err := SetSourceCommentary(database, imageID, source, postID, commentary); err != nil {
			return "commentary", err
		}
	}
	if original != "" {
		if err := SetSourceOriginal(database, imageID, source, postID, original); err != nil {
			return "the original source", err
		}
	}
	if len(notes) > 0 {
		if err := ReplaceSourceAnnotations(database, imageID, source, postID, notes); err != nil {
			return "notes", err
		}
	}
	return "", nil
}

// ApplyCreateProvenance writes the supplied provenance fields onto a
// freshly-created image. Every field is optional; a bare create touches
// nothing. Validation has already run, so a failure here is a DB-level
// error.
func ApplyCreateProvenance(database *db.DB, imageID int64, source, postID, url, md5, parentURL, collection, commentary, original string, post PostFile, order *int) error {
	if source != "" || url != "" {
		if err := WriteSourceProvenance(database, imageID, source, postID, url, md5, parentURL, post); err != nil {
			return err
		}
	}
	// Annotations stay with the caller: a failed note write warns rather
	// than failing a create whose row already landed.
	if _, err := ApplySourceProvenance(database, imageID, source, postID, commentary, original, nil); err != nil {
		return err
	}
	if collection != "" {
		return SetHomeCollection(database, imageID, collection, order)
	}
	return nil
}

// MergeSource folds a re-pushed file's provenance and tags into an existing
// image instead of discarding them: the origin is recorded and the tags
// imported from that source are reconciled against the incoming set, with
// the rating protected. Attribution is the source label so each source owns
// its slice; tags the source dropped are flagged stale, never removed. A
// push with no source label leaves tags untouched. The second return
// carries unresolvable-tag warnings for the caller's response envelope.
//
// A booru origin arriving while the primary is the url-less "ptr" row takes
// the primary over: a lookup that hits both backends should lead with the
// booru post, whatever order the enrich calls landed in.
func MergeSource(database *db.DB, tagSvc *tags.Service, imageID int64, source, postID, url, md5, parentURL string, post PostFile, rawTags []string) (MergeSummary, []string, error) {
	var sum MergeSummary
	if source != "" || url != "" {
		if err := WriteSourceProvenance(database, imageID, source, postID, url, md5, parentURL, post); err != nil {
			return sum, nil, err
		}
		if source != "" && !strings.EqualFold(source, "ptr") {
			var primary string
			if err := database.Read.QueryRow(`SELECT source FROM images WHERE id = ?`, imageID).Scan(&primary); err == nil &&
				strings.EqualFold(strings.TrimSpace(primary), "ptr") {
				if err := MakeSourcePrimary(database, imageID, source, postID); err != nil {
					return sum, nil, err
				}
			}
		}
		sum.SourceAdded = true
	}
	var warnings []string
	if source != "" && len(rawTags) > 0 {
		tagIDs, warns := ResolveTagNames(database, tagSvc, rawTags, source)
		warnings = warns
		// The per-site tag slice is shared by every post of that site on the
		// image, so the reconcile only runs while this origin is the site's
		// sole one; alongside a sibling post the merge is add-only.
		var origins int
		if err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM image_sources WHERE image_id = ? AND site = ?`, imageID, source,
		).Scan(&origins); err != nil {
			return sum, warnings, err
		}
		r, err := tagSvc.SyncSourceTags(imageID, tagIDs, source, origins <= 1)
		if err != nil {
			return sum, warnings, err
		}
		sum.TagsAdded, sum.TagsRetired, sum.RatingFilled = r.Added, r.Retired, r.RatingFilled
	}
	return sum, warnings, nil
}

// ApplyPTRTags folds a batch PTR hit into an image exactly as a per-image
// PTR enrich does: the url-less `ptr` origin, the source-attributed tags,
// and the same reconcile with the same stale semantics. The scheduled PTR
// phase reads the hashes from monloader in bulk and applies them itself, so
// this is what keeps both paths writing one set of rules.
func ApplyPTRTags(database *db.DB, tagSvc *tags.Service, imageID int64, tagNames []string) error {
	_, _, err := MergeSource(database, tagSvc, imageID, "ptr", "", "", "", "", PostFile{}, tagNames)
	return err
}

// ResolveTagNames turns the raw names a push carries into tag ids,
// creating what the catalog lacks and stamping origin on the new rows. A
// name that will not resolve is a warning, not a failure: the rest of the
// push still lands.
func ResolveTagNames(database *db.DB, tagSvc *tags.Service, rawTags []string, origin string) ([]int64, []string) {
	var warnings []string
	tagIDs := make([]int64, 0, len(rawTags))
	for _, tagName := range rawTags {
		catID, bareName, err := resolveCategoryTag(database, tagName)
		if err != nil {
			warnings = append(warnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tag, err := tagSvc.GetOrCreateTagFrom(bareName, catID, origin)
		if err != nil {
			warnings = append(warnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tagIDs = append(tagIDs, tag.ID)
	}
	return tagIDs, warnings
}

// resolveCategoryTag splits "artist:foo" into (artist_id, "foo") when
// "artist" names a real category, otherwise returns (general_id, input) so
// colon-bearing tag names like "nier:automata" or ":3" round-trip without a
// warning.
func resolveCategoryTag(database *db.DB, input string) (int64, string, error) {
	input = strings.TrimSpace(input)
	if idx := strings.Index(input, ":"); idx > 0 {
		catID, ok, err := CategoryIDByName(database, input[:idx])
		if err != nil {
			return 0, "", err
		}
		if ok {
			return catID, input[idx+1:], nil
		}
	}
	catID, ok, err := CategoryIDByName(database, "general")
	if err != nil {
		return 0, "", err
	}
	if !ok {
		return 0, "", fmt.Errorf("unknown category %q", "general")
	}
	return catID, input, nil
}

// CategoryIDByName resolves a tag category by name, reporting whether it
// exists rather than treating an unknown one as an error.
func CategoryIDByName(database *db.DB, name string) (int64, bool, error) {
	var id int64
	err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
