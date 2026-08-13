package api

import (
	"cmp"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register gif decoder for canDecodeImage
	_ "image/jpeg" // register jpeg decoder for canDecodeImage
	_ "image/png"  // register png decoder for canDecodeImage
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp" // register webp decoder for canDecodeImage

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tagger"
	"github.com/monbooru/monbooru/internal/tags"
)

// applyCreateProvenance writes the supplied provenance fields onto a
// freshly-ingested row. Only non-empty values are written; the row's
// empty-string / NULL defaults already cover the unset case, so a bare
// create touches nothing. Validation has already run, so a failure here
// is a DB-level error.
func applyCreateProvenance(g Gallery, imageID int64, source, postID, url, md5, parentURL, collection, commentary, original string, order *int) error {
	if source != "" || url != "" {
		if err := writeSourceProvenance(g, imageID, source, postID, url, md5, parentURL); err != nil {
			return err
		}
	}
	// Annotations stay with the caller: a failed note write warns rather than
	// failing a create whose row already landed.
	if _, err := applySourceProvenance(g, imageID, source, postID, commentary, original, nil); err != nil {
		return err
	}
	if collection != "" {
		return gallery.SetHomeCollection(g.DB, imageID, collection, order)
	}
	return nil
}

// writeSourceProvenance records one origin row: membership plus its md5
// and parent-URL columns, keyed by (source, postID).
func writeSourceProvenance(g Gallery, imageID int64, source, postID, url, md5, parentURL string) error {
	if err := gallery.AddSourceMembership(g.DB, imageID, source, postID, url); err != nil {
		return err
	}
	if err := gallery.SetSourceMD5(g.DB, imageID, source, postID, md5); err != nil {
		return err
	}
	return gallery.SetSourceParentURL(g.DB, imageID, source, postID, parentURL)
}

// applySourceProvenance writes the per-source commentary, original, and
// annotations an enrich or duplicate-merge push carries, skipping empty
// values. On failure the returned step names what was being applied so
// each caller can map the error to its own reporting.
func applySourceProvenance(g Gallery, imageID int64, source, postID, commentary, original string, notes []models.Annotation) (string, error) {
	if source == "" {
		return "", nil
	}
	if commentary != "" {
		if err := gallery.SetSourceCommentary(g.DB, imageID, source, postID, commentary); err != nil {
			return "commentary", err
		}
	}
	if original != "" {
		if err := gallery.SetSourceOriginal(g.DB, imageID, source, postID, original); err != nil {
			return "the original source", err
		}
	}
	if len(notes) > 0 {
		if err := gallery.ReplaceSourceAnnotations(g.DB, imageID, source, postID, notes); err != nil {
			return "notes", err
		}
	}
	return "", nil
}

// linkParentRelations turns a booru parent/child declaration into derivative
// edges once both sides are in the gallery: a pushed post links under the
// image already holding its declared parent URL, and images whose origins
// declare the pushed post's URL as parent link under it. Conflicts (an
// existing relation on the pair, a derivative that already has a source, a
// not-related mark) are standing operator decisions, so they are skipped
// quietly; linking is best-effort and never fails the push.
func linkParentRelations(g Gallery, imageID int64, url, parentURL string) {
	if g.RelationsSvc == nil {
		return
	}
	if parentURL != "" {
		if parentID, ok := gallery.ImageIDBySourceURL(g.DB, parentURL); ok && parentID != imageID {
			if err := g.RelationsSvc.AddDerivativeEdge(parentID, imageID); err != nil {
				logx.Debugf("api: parent link %d -> %d skipped: %v", parentID, imageID, err)
			}
		}
	}
	if url == "" {
		return
	}
	children, err := gallery.ChildIDsByParentURL(g.DB, url)
	if err != nil {
		logx.Warnf("api: child lookup for %q: %v", url, err)
		return
	}
	for _, child := range children {
		if child == imageID {
			continue
		}
		if err := g.RelationsSvc.AddDerivativeEdge(imageID, child); err != nil {
			logx.Debugf("api: child link %d -> %d skipped: %v", imageID, child, err)
		}
	}
}

// mergeSummary reports what a duplicate-merge folded into an existing image.
type mergeSummary struct {
	TagsAdded    int  `json:"tags_added"`
	TagsRetired  int  `json:"tags_retired"`
	RatingFilled bool `json:"rating_filled"`
	SourceAdded  bool `json:"source_added"`
}

// mergeSource folds a re-pushed file's provenance and tags into an existing
// image instead of discarding them (issue #6): the origin is recorded and the
// tags imported from that source are reconciled against the incoming set,
// with the rating protected. Attribution is the source label so each source
// owns its slice; tags the source dropped are flagged stale, never removed.
// A push with no source label leaves tags untouched. The
// second return carries unresolvable-tag warnings for the response envelope.
// A booru origin arriving while the primary is the url-less "ptr" row takes
// the primary over: a lookup that hits both backends should lead with the
// booru post, whatever order monloader's enrich calls landed in.
//
// Takes no receiver so ApplyPTRTags can hand the batch PTR pass the same
// path a per-image enrich walks.
func mergeSource(g Gallery, imageID int64, source, postID, url, md5, parentURL string, rawTags []string) (mergeSummary, []string, error) {
	var sum mergeSummary
	if source != "" || url != "" {
		if err := writeSourceProvenance(g, imageID, source, postID, url, md5, parentURL); err != nil {
			return sum, nil, err
		}
		if source != "" && !strings.EqualFold(source, "ptr") {
			var primary string
			if err := g.DB.Read.QueryRow(`SELECT source FROM images WHERE id = ?`, imageID).Scan(&primary); err == nil &&
				strings.EqualFold(strings.TrimSpace(primary), "ptr") {
				if err := gallery.MakeSourcePrimary(g.DB, imageID, source, postID); err != nil {
					return sum, nil, err
				}
			}
		}
		sum.SourceAdded = true
	}
	var warnings []string
	if source != "" && len(rawTags) > 0 {
		tagIDs, warns := resolveTagNames(g, rawTags, source)
		warnings = warns
		// The per-site tag slice is shared by every post of that site on the
		// image, so the reconcile only runs while this origin is the site's
		// sole one; alongside a sibling post the merge is add-only.
		var origins int
		if err := g.DB.Read.QueryRow(
			`SELECT COUNT(*) FROM image_sources WHERE image_id = ? AND site = ?`, imageID, source,
		).Scan(&origins); err != nil {
			return sum, warnings, err
		}
		r, err := g.TagSvc.SyncSourceTags(imageID, tagIDs, source, origins <= 1)
		if err != nil {
			return sum, warnings, err
		}
		sum.TagsAdded, sum.TagsRetired, sum.RatingFilled = r.Added, r.Retired, r.RatingFilled
	}
	return sum, warnings, nil
}

// enrichImage handles POST /api/v1/images/{id}/enrich: applies fetched
// metadata (tags, provenance, artist commentary, positional notes) to an
// existing image with no file upload - the metadata-only counterpart of a
// push, used by monloader's source refetch. It shares mergeSource with the
// duplicate branch for tags + provenance. When verify is set and a
// source_md5 is supplied, the image's stored bytes are md5'd on demand and
// compared first; a mismatch means the source returned a different file -
// a repointed post, or a page URL that resolves to some other file - so
// nothing changes (409 hash_mismatch).
func (h *Handler) enrichImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var canonPath string
	switch err := g.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonPath); {
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	var body struct {
		Tags       []string         `json:"tags"`
		Source     string           `json:"source"`
		PostID     string           `json:"post_id"`
		URL        string           `json:"url"`
		SourceMD5  string           `json:"source_md5"`
		ParentURL  string           `json:"parent_url"`
		Verify     bool             `json:"verify"`
		Similarity float64          `json:"similarity"`
		Commentary string           `json:"commentary"`
		Original   string           `json:"original"`
		Notes      []annotationJSON `json:"notes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := validateMaxLen("commentary", strings.TrimSpace(body.Commentary), maxImageCommentaryLen); badRequest(w, err) {
		return
	}
	if err := validateMaxLen("original", strings.TrimSpace(body.Original), maxImageOriginalLen); badRequest(w, err) {
		return
	}
	sourceMD5 := strings.TrimSpace(body.SourceMD5)
	if err := validateMaxLen("source_md5", sourceMD5, maxSourceMD5Len); badRequest(w, err) {
		return
	}
	postID := strings.TrimSpace(body.PostID)
	if err := validateMaxLen("post_id", postID, maxSourcePostIDLen); badRequest(w, err) {
		return
	}
	parentURL := strings.TrimSpace(body.ParentURL)
	if err := validateImageURL(parentURL); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "parent_url: "+err.Error())
		return
	}
	source := strings.TrimSpace(body.Source)
	if err := validateImageSource(source); badRequest(w, err) {
		return
	}
	if err := validateImageURL(strings.TrimSpace(body.URL)); badRequest(w, err) {
		return
	}
	verified := true
	md5Verdict := ""
	if body.Verify {
		if sourceMD5 == "" {
			verified = false // asked to verify, but the source reported no md5
		} else {
			got, err := gallery.Md5File(canonPath)
			if err != nil {
				g.recordFetch(id, "error", "could not verify the file; fetch not applied")
				apiError(w, http.StatusInternalServerError, "internal_error", "cannot hash image: "+err.Error())
				return
			}
			if !strings.EqualFold(got, sourceMD5) {
				md5Verdict = "differ"
				// A similarity-matched origin serves a different file by
				// design, so its mismatch is expected rather than a repointed
				// post; apply the fetch and report it unverified. The flag may
				// still sit on a (site, "") row the merge below has not adopted
				// yet, so both keys are checked.
				if body.Similarity <= 0 && !gallery.SourceSimilarityMatched(g.DB, id, source, postID) &&
					!gallery.SourceSimilarityMatched(g.DB, id, source, "") {
					// The verdict is exactly what the [upgrade] gate needs, so
					// it lands even though the fetch itself is refused.
					if err := gallery.SetSourceMD5Match(g.DB, id, source, postID, md5Verdict); err != nil {
						logx.Warnf("api enrich: record md5 verdict: %v", err)
					}
					g.recordFetch(id, "mismatch", "the source returned a different file (hash mismatch); no tags applied")
					apiError(w, http.StatusConflict, "hash_mismatch", "the source returned a different file")
					return
				}
				verified = false
			} else {
				md5Verdict = "match"
			}
		}
	}
	sum, tagWarnings, err := mergeSource(g, id, source, postID, strings.TrimSpace(body.URL), sourceMD5, parentURL, body.Tags)
	if err != nil {
		g.recordFetch(id, "error", "fetch failed while applying tags")
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	linkParentRelations(g, id, strings.TrimSpace(body.URL), parentURL)
	if err := gallery.SetSourceSimilarity(g.DB, id, source, postID, body.Similarity); err != nil {
		g.recordFetch(id, "error", "fetch failed while recording the match score")
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// Recorded after the merge so a first enrich's fresh origin row exists
	// to carry it.
	if md5Verdict != "" {
		if err := gallery.SetSourceMD5Match(g.DB, id, source, postID, md5Verdict); err != nil {
			g.recordFetch(id, "error", "fetch failed while recording the hash verdict")
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}
	// Artist commentary and positional notes are attributed to the same
	// source, so a refetch pulls them in alongside the tags. Both replace what
	// the source last carried; an empty payload leaves the stored value be.
	if step, err := applySourceProvenance(g, id, source, postID,
		strings.TrimSpace(body.Commentary), strings.TrimSpace(body.Original), annotationsFromInput(body.Notes)); err != nil {
		g.recordFetch(id, "error", "fetch failed while applying "+step)
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	g.invalidate()
	g.recordFetch(id, "ok", fetchSummary(sum, len(body.Tags)))
	// A hash lookup reports its hit by enriching, so an enrich landing on an
	// image with an attempt in flight concludes it. The source implies which
	// backend answered.
	recordLookupHit(g, id, source)
	resp := map[string]any{"merge": sum, "verified": verified}
	if len(tagWarnings) > 0 {
		resp["tag_warnings"] = tagWarnings
	}
	writeJSON(w, http.StatusOK, resp)
}

// fetchSummary is the operator-facing confirmation a source refetch surfaces
// once the enrich lands; it names the tag delta when the fetch changed
// anything. A tagless enrich that recorded a source (monloader's source-only
// similarity match) must not claim tags were fetched.
func fetchSummary(sum mergeSummary, tagsSent int) string {
	switch {
	case tagsSent == 0 && sum.SourceAdded:
		return "Recorded the source; no tags were fetched."
	case sum.TagsAdded > 0 && sum.TagsRetired > 0:
		return fmt.Sprintf("Fetched tags from the source (+%d; %d no longer listed there).", sum.TagsAdded, sum.TagsRetired)
	case sum.TagsAdded > 0:
		return fmt.Sprintf("Fetched tags from the source (+%d).", sum.TagsAdded)
	case sum.TagsRetired > 0:
		return fmt.Sprintf("Fetched tags from the source (%d no longer listed there).", sum.TagsRetired)
	default:
		return "Fetched tags from the source."
	}
}

// fetchStatusReport handles POST /api/v1/images/{id}/fetch-status: monloader
// reports a source-fetch outcome that never reached enrich (a fetch that hit an
// unsupported URL, timed out, or was blocked) so the detail page's poll can
// surface it instead of spinning to the poll cap. Body: {state, message}.
func (h *Handler) fetchStatusReport(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		State   string `json:"state"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.State) == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "state is required")
		return
	}
	g.recordFetch(id, body.State, body.Message)
	recordLookupTerminal(g, id, body.State)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ApplyPTRTags folds a batch PTR hit into an image exactly as a per-image
// PTR enrich does: the url-less `ptr` origin, the source-attributed tags, and
// the same reconcile with the same stale semantics. The scheduled PTR phase
// reads the hashes from monloader in bulk and applies them itself, so this is
// what keeps both paths writing one set of rules.
func ApplyPTRTags(g Gallery, imageID int64, tags []string) error {
	if _, _, err := mergeSource(g, imageID, "ptr", "", "", "", "", tags); err != nil {
		return err
	}
	g.invalidate()
	return nil
}

// recordLookupHit concludes an in-flight attempt on the backend the enrich's
// source implies: the PTR labels its own writes, anything else came off the
// online walk.
func recordLookupHit(g Gallery, imageID int64, source string) {
	backend := lookup.BackendBooru
	if strings.EqualFold(strings.TrimSpace(source), "ptr") {
		backend = lookup.BackendPTR
	}
	if err := lookup.RecordInFlight(g.DB, imageID, backend, lookup.ResultHit, time.Now()); err != nil {
		logx.Warnf("api: lookup hit for image %d: %v", imageID, err)
	}
}

// recordLookupTerminal concludes whatever the image has in flight from a
// pre-enrich report. Only hash_not_found is evidence about the image; a
// dropped job and every failure code are evidence about the plumbing, so they
// clear the in-flight state and leave the ladder where it was.
func recordLookupTerminal(g Gallery, imageID int64, state string) {
	var result string
	switch state {
	case "pending", "ok":
		return
	case "hash_not_found":
		result = lookup.ResultMiss
	default:
		result = lookup.ResultError
	}
	if err := lookup.RecordInFlight(g.DB, imageID, "", result, time.Now()); err != nil {
		logx.Warnf("api: lookup outcome for image %d: %v", imageID, err)
	}
}

// replaceImageFile handles POST /api/v1/images/{id}/file: the file-carrying
// sibling of enrich. The uploaded bytes replace the image's file in place -
// the row and everything attached to it survive while every content-derived
// column and artifact is re-derived - and the accompanying metadata lands
// through the same merge as a push. The uploaded original already existing
// as another row is a refusal, never an implicit merge or delete; the pair
// is recorded as potential duplicates for the standing dup workflow.
func (h *Handler) replaceImageFile(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var curSHA, fileType string
	var oldW, oldH *int
	var oldSize int64
	switch err := g.DB.Read.QueryRow(
		`SELECT sha256, file_type, width, height, file_size FROM images WHERE id = ?`, id,
	).Scan(&curSHA, &fileType, &oldW, &oldH, &oldSize); {
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if gallery.IsVideoType(fileType) || fileType == models.FileTypeCBZ {
		g.recordFetch(id, "error", "this file type cannot be replaced")
		apiError(w, http.StatusConflict, "wrong_type", "only image rows can have their file replaced")
		return
	}
	if !isMultipart(r.Header.Get("Content-Type")) {
		apiError(w, http.StatusBadRequest, "invalid_request", "multipart body required")
		return
	}
	if maxBytes := int64(h.cfg.Gallery.MaxFileSizeMB) * 1024 * 1024; maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+4096)
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds max size")
		return
	}
	file, fh, err := r.FormFile("file")
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "missing file field")
		return
	}
	defer func() { _ = file.Close() }()

	source := strings.TrimSpace(r.FormValue("source"))
	postID := strings.TrimSpace(r.FormValue("post_id"))
	url := strings.TrimSpace(r.FormValue("url"))
	claimedMD5 := strings.TrimSpace(r.FormValue("md5"))
	parentURL := strings.TrimSpace(r.FormValue("parent_url"))
	commentary := strings.TrimSpace(r.FormValue("commentary"))
	original := strings.TrimSpace(r.FormValue("original"))
	notes := parseNotesField(r.FormValue("notes"))
	var tags []string
	if tagsJSON := r.FormValue("tags"); tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "tags must be a JSON array of names")
			return
		}
	}
	if err := validateCreateProvenance(source, postID, url, claimedMD5, parentURL, "", commentary, original, nil); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Staged under the thumbnails dir - outside the watched gallery tree, so
	// the watcher never sees a half-written intermediate. sha256 and md5 come
	// from the same streaming pass.
	staged, err := os.CreateTemp(g.ThumbnailsPath, "replace-*"+filepath.Ext(fh.Filename))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to stage upload")
		return
	}
	stagedPath := staged.Name()
	discardStaged := func() { _ = os.Remove(stagedPath) }
	shaH, md5H := sha256.New(), md5.New()
	if _, err := io.Copy(io.MultiWriter(staged, shaH, md5H), file); err != nil {
		_ = staged.Close()
		discardStaged()
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to save upload")
		return
	}
	_ = staged.Close()
	newSHA := hex.EncodeToString(shaH.Sum(nil))
	newMD5 := hex.EncodeToString(md5H.Sum(nil))

	applyMeta := func() (mergeSummary, []string, bool) {
		sum, tagWarnings, err := mergeSource(g, id, source, postID, url, claimedMD5, parentURL, tags)
		if err != nil {
			g.recordFetch(id, "error", "replace failed while applying tags")
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return sum, nil, false
		}
		linkParentRelations(g, id, url, parentURL)
		if step, err := applySourceProvenance(g, id, source, postID, commentary, original, notes); err != nil {
			g.recordFetch(id, "error", "replace failed while applying "+step)
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return sum, tagWarnings, false
		}
		// The origin now serves exactly the local bytes: similarity and the
		// md5 ledger reset so the [upgrade] gate closes.
		if source != "" {
			if err := gallery.MarkSourceExact(g.DB, id, source, postID, newMD5); err != nil {
				g.recordFetch(id, "error", "replace failed while recording the source state")
				apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return sum, tagWarnings, false
			}
		}
		return sum, tagWarnings, true
	}

	if strings.EqualFold(newSHA, curSHA) {
		// The local file already is the original; only the metadata lands.
		discardStaged()
		sum, tagWarnings, ok := applyMeta()
		if !ok {
			return
		}
		g.invalidate()
		msg := "The file already matches the source."
		if source != "" {
			msg += " " + fetchSummary(sum, len(tags))
		}
		g.recordFetch(id, "ok", msg)
		resp := map[string]any{"replaced": false, "merge": sum}
		if len(tagWarnings) > 0 {
			resp["tag_warnings"] = tagWarnings
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	var otherID int64
	if err := g.DB.Read.QueryRow(
		`SELECT id FROM images WHERE sha256 = ? AND id != ?`, newSHA, id,
	).Scan(&otherID); err == nil {
		discardStaged()
		// Record the pair so the existing pair-decide workflow takes over;
		// an existing relation or a not-related mark wins, like the
		// parent-url auto-link.
		if g.RelationsSvc != nil {
			if err := g.RelationsSvc.AddDuplicate(id, otherID); err != nil {
				logx.Debugf("api replace: duplicate link %d - %d skipped: %v", id, otherID, err)
			}
		}
		g.recordFetch(id, "already_exists",
			fmt.Sprintf("You already hold the original as image %d.", otherID))
		apiError(w, http.StatusConflict, "already_exists",
			fmt.Sprintf("the original already exists as image %d", otherID))
		return
	}

	newType, ftErr := gallery.DetectFileType(stagedPath)
	if ftErr != nil {
		discardStaged()
		g.recordFetch(id, "error", "the source served an unsupported file type")
		apiError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}
	if gallery.IsVideoType(newType) || newType == models.FileTypeCBZ {
		discardStaged()
		g.recordFetch(id, "error", "the source serves a video or archive; only image files can replace an image")
		apiError(w, http.StatusConflict, "wrong_type", "the replacement must be an image file")
		return
	}
	if !canDecodeImage(stagedPath) {
		rescued := newType == models.FileTypeJPEG &&
			gallery.NormalizeImage(stagedPath) == nil && canDecodeImage(stagedPath)
		if !rescued {
			discardStaged()
			g.recordFetch(id, "error", "the downloaded file does not decode as an image")
			apiError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
			return
		}
		// The rescue re-encoded the staged bytes, so the hashes moved.
		if reSHA, err := gallery.HashFile(stagedPath); err == nil {
			newSHA = reSHA
		}
		if reMD5, err := gallery.Md5File(stagedPath); err == nil {
			newMD5 = reMD5
		}
	}

	if err := gallery.ApplyReplacedFile(g.DB, g.ThumbnailsPath, id, stagedPath, newSHA, newType); err != nil {
		discardStaged()
		logx.Warnf("api replace image %d: %v", id, err)
		g.recordFetch(id, "error", "the file replacement failed")
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	sum, tagWarnings, ok := applyMeta()
	if !ok {
		return
	}
	g.invalidate()

	var newW, newH *int
	var newSize int64
	_ = g.DB.Read.QueryRow(`SELECT width, height, file_size FROM images WHERE id = ?`, id).
		Scan(&newW, &newH, &newSize)
	msg := fmt.Sprintf("Replaced the file (%s -> %s, %d kB -> %d kB).",
		dimsLabel(oldW, oldH), dimsLabel(newW, newH), (oldSize+512)/1024, (newSize+512)/1024)
	if source != "" {
		msg += " " + fetchSummary(sum, len(tags))
	}
	g.recordFetch(id, "ok", msg)
	resp := map[string]any{"replaced": true, "merge": sum}
	if len(tagWarnings) > 0 {
		resp["tag_warnings"] = tagWarnings
	}
	writeJSON(w, http.StatusOK, resp)
}

// dimsLabel renders WxH for the replace summary, tolerating rows whose
// dimensions never probed.
func dimsLabel(w, h *int) string {
	if w == nil || h == nil {
		return "?x?"
	}
	return fmt.Sprintf("%dx%d", *w, *h)
}

// canDecodeImage opens path and runs image.DecodeConfig on the first
// few bytes. Used as a fast post-DetectFileType guard so a text file
// with an image extension is rejected before the row reaches the DB
// with a null width / height. Archive and video file types skip this
// check; the cbz path does its own integrity verification inside
// Ingest and video frames decode via ffmpeg later in the thumbnail
// step.
func canDecodeImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	_, _, err = image.DecodeConfig(f)
	return err == nil
}

// buildImageResponse fetches an image plus its tags and assembles the
// JSON response struct.
func (h *Handler) buildImageResponse(g Gallery, imageID int64) (*imageResponse, error) {
	img, err := models.ScanImageRow(g.DB.Read.QueryRow(
		`SELECT `+models.ImageRowColumns+` FROM images i WHERE i.id = ?`, imageID))
	if err != nil {
		return nil, err
	}

	var aliases []string
	if byID, err := loadAliasesForImages(g, []int64{imageID}); err != nil {
		logx.Warnf("buildImageResponse aliases: %v", err)
	} else {
		aliases = byID[imageID]
	}
	var tags []imageTagJSON
	if byID, err := loadTagsForImages(g, []int64{imageID}); err != nil {
		logx.Warnf("buildImageResponse tags: %v", err)
	} else {
		tags = byID[imageID]
	}

	resp := makeImageResponse(g, img, tags, aliases)
	if srcs, err := loadTagSourcesForImage(g, imageID); err != nil {
		logx.Warnf("buildImageResponse tag sources: %v", err)
	} else {
		resp.TagSources = srcs
	}
	if cols, err := gallery.CollectionsForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse collections: %v", err)
	} else {
		for _, c := range cols {
			resp.Collections = append(resp.Collections, collectionJSON{Name: c.Name, Order: c.Order})
		}
	}
	if srcs, err := gallery.SourcesForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse sources: %v", err)
	} else {
		for _, s := range srcs {
			resp.Sources = append(resp.Sources, sourceJSON{Site: s.Site, PostID: s.PostID, URL: s.URL, Commentary: s.Commentary, Original: s.Original, Similarity: s.Similarity})
		}
	}
	if anns, err := gallery.AnnotationsForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse annotations: %v", err)
	} else {
		for _, a := range anns {
			resp.Annotations = append(resp.Annotations, annotationJSON{Site: a.Site, PostID: a.PostID, X: a.X, Y: a.Y, W: a.W, H: a.H, Body: a.Body})
		}
	}
	addLookupState(g, imageID, &resp)
	return &resp, nil
}

// addLookupState fills the scheduled-lookup opt-in and the recorded history.
// Both are read-only reporting; a failure logs and leaves them absent rather
// than failing the whole image read.
func addLookupState(g Gallery, imageID int64, resp *imageResponse) {
	var on, ptrOn bool
	if err := g.DB.Read.QueryRow(
		`SELECT scheduled_lookup, scheduled_lookup_ptr FROM images WHERE id = ?`, imageID,
	).Scan(&on, &ptrOn); err != nil {
		logx.Warnf("buildImageResponse scheduled_lookup: %v", err)
		return
	}
	resp.ScheduledLookup, resp.ScheduledLookupPTR = &on, &ptrOn
	rows, err := lookup.ForImage(g.DB, imageID)
	if err != nil {
		logx.Warnf("buildImageResponse lookup history: %v", err)
		return
	}
	for backend, r := range rows {
		entry := lookupJSON{LastResult: r.LastResult, Attempts: r.Attempts}
		if !r.LastAt.IsZero() {
			entry.LastAt = r.LastAt.Format(time.RFC3339)
		}
		if !r.NextDueAt.IsZero() {
			entry.NextDueAt = r.NextDueAt.Format(time.RFC3339)
		}
		if resp.Lookup == nil {
			resp.Lookup = map[string]lookupJSON{}
		}
		resp.Lookup[backend] = entry
	}
}

func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// patchImage handles PATCH /api/v1/images/{id}: edits the operator-
// editable fields source, url, collection, collection_order,
// is_favorited, and is_inbox. Pointer fields carry presence: an absent
// (or JSON null) field is left alone, a present one is written. An empty
// string clears a text field; clearing collection nulls a stranded
// collection_order in the same write (mirroring the detail-page editor)
// unless an order is supplied alongside. To clear collection_order on
// its own, clear the collection. Returns the updated image object.
func (h *Handler) patchImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	var body struct {
		Source             *string `json:"source"`
		URL                *string `json:"url"`
		Collection         *string `json:"collection"`
		CollectionOrder    *int    `json:"collection_order"`
		IsFavorited        *bool   `json:"is_favorited"`
		IsInbox            *bool   `json:"is_inbox"`
		ScheduledLookup    *bool   `json:"scheduled_lookup"`
		ScheduledLookupPTR *bool   `json:"scheduled_lookup_ptr"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	updates := []string{}
	args := []any{}
	cacheAffecting := false

	// source / url edit the primary origin. Fill the unpatched half from the
	// current primary (the scalar mirror) so a one-field PATCH keeps the
	// other, then apply through SetPrimarySource after the main UPDATE.
	setSrc := false
	var srcSite, srcURL string
	if body.Source != nil || body.URL != nil {
		if err := g.DB.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, id).Scan(&srcSite, &srcURL); serverError(w, err) {
			return
		}
		if body.Source != nil {
			s := strings.TrimSpace(*body.Source)
			if err := validateImageSource(s); badRequest(w, err) {
				return
			}
			srcSite = s
		}
		if body.URL != nil {
			u := strings.TrimSpace(*body.URL)
			if err := validateImageURL(u); badRequest(w, err) {
				return
			}
			srcURL = u
		}
		setSrc = true
		cacheAffecting = true
	}
	// Collection / collection_order map onto the home membership; the
	// resolved label and order are applied through SetHomeCollection after
	// the main UPDATE so image_collections stays in sync. An absent order
	// next to a present label keeps the stored position (rename is sticky).
	setHome := false
	var homeName string
	var homeOrder *int
	if body.Collection != nil || body.CollectionOrder != nil {
		var curSeries string
		var curOrder sql.NullInt64
		if err := g.DB.Read.QueryRow(`SELECT series, series_order FROM images WHERE id = ?`, id).Scan(&curSeries, &curOrder); serverError(w, err) {
			return
		}
		homeName = curSeries
		if body.Collection != nil {
			c := strings.TrimSpace(*body.Collection)
			if err := validateImageCollection(c); badRequest(w, err) {
				return
			}
			homeName = c
		}
		if body.CollectionOrder != nil {
			n := *body.CollectionOrder
			if n < 1 {
				apiError(w, http.StatusBadRequest, "invalid_request", "collection_order must be 1 or higher")
				return
			}
			if homeName == "" {
				apiError(w, http.StatusBadRequest, "invalid_request", "collection_order requires a non-empty collection")
				return
			}
			homeOrder = &n
		} else if homeName != "" && curOrder.Valid {
			v := int(curOrder.Int64)
			homeOrder = &v
		}
		setHome = true
		cacheAffecting = true
	}
	if body.IsFavorited != nil {
		updates = append(updates, "is_favorited = ?")
		args = append(args, boolToInt(*body.IsFavorited))
		cacheAffecting = true
	}
	if body.IsInbox != nil {
		updates = append(updates, "is_inbox = ?")
		args = append(args, boolToInt(*body.IsInbox))
		cacheAffecting = true
	}
	if body.ScheduledLookup != nil {
		updates = append(updates, "scheduled_lookup = ?")
		args = append(args, boolToInt(*body.ScheduledLookup))
	}
	if body.ScheduledLookupPTR != nil {
		updates = append(updates, "scheduled_lookup_ptr = ?")
		args = append(args, boolToInt(*body.ScheduledLookupPTR))
	}
	if len(updates) == 0 && !setHome && !setSrc {
		apiError(w, http.StatusBadRequest, "invalid_request", "no editable fields supplied")
		return
	}

	if len(updates) > 0 {
		args = append(args, id)
		if _, err := g.DB.Write.Exec(`UPDATE images SET `+strings.Join(updates, ", ")+` WHERE id = ?`, args...); serverError(w, err) {
			return
		}
	}
	if setHome {
		if err := gallery.SetHomeCollection(g.DB, id, homeName, homeOrder); serverError(w, err) {
			return
		}
	}
	if setSrc {
		if err := gallery.SetPrimarySource(g.DB, id, srcSite, srcURL); err != nil {
			if errors.Is(err, gallery.ErrSourceIdentityExists) {
				apiError(w, http.StatusConflict, "conflict", err.Error())
				return
			}
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}
	// Turning the schedule back on is itself a reset: on an exhausted image
	// a spent ladder would otherwise mean nothing happens, exactly as the
	// detail page's [look again] avoids.
	for backend, want := range map[string]*bool{
		lookup.BackendBooru: body.ScheduledLookup,
		lookup.BackendPTR:   body.ScheduledLookupPTR,
	} {
		if want == nil || !*want {
			continue
		}
		if err := lookup.Reset(g.DB, id, backend, time.Now()); err != nil {
			logx.Warnf("api: lookup reset for image %d: %v", id, err)
		}
	}
	// source, collection, favorite, and inbox all feed cached aggregates
	// (the sidebar source / collection lists, fav/inbox counts, and the
	// match-id cache keyed on fav:/inbox:), so invalidate on any of them.
	if cacheAffecting {
		g.invalidate()
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// createInput carries one create request's parsed and validated fields,
// whichever mode supplied them.
type createInput struct {
	imgPath         string
	initialTags     []string
	folder          string
	autotag         bool
	taggerName      string
	via             string              // caller-supplied label; stored on images.origin and inherited by initial tags
	source          string              // operator-edited provenance label; set on the new row when non-empty
	postID          string              // the source's post id, keying the origin row apart from other posts on the same site
	url             string              // canonical web URL; set on the new row when non-empty
	md5             string              // md5 the source claimed; recorded on the origin row as the audit trail
	parentURL       string              // canonical URL of the post's declared parent; recorded on the origin row and linked as a derivative edge when present
	commentary      string              // artist commentary for the pushed source; folded in on create/merge
	original        string              // upstream artist source the post declared; folded in on create/merge
	notes           []models.Annotation // positional note boxes for the pushed source
	collection      string              // collection label (images.series); set on the new row when non-empty
	collectionOrder *int                // 1-based position within collection; nil = unset
	uploadedToDisk  bool                // true when we wrote the file ourselves (multipart)
}

// parseCreateMultipart reads mode A (multipart upload): validates the
// fields, then writes the file straight to its final destination so the
// watcher sees the real filename. ok=false means the error response was
// already written.
func (h *Handler) parseCreateMultipart(w http.ResponseWriter, r *http.Request, g Gallery) (createInput, bool) {
	var in createInput
	maxBytes := int64(h.cfg.Gallery.MaxFileSizeMB) * 1024 * 1024
	// MaxFileSizeMB <= 0 disables the per-file cap (the watcher, Sync and the
	// web upload treat it the same); skip MaxBytesReader so a bare 4 KiB body
	// cap doesn't reject every push. createImage enforces the real limit when
	// one is set.
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+4096)
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds max size")
		return in, false
	}
	file, fh, err := r.FormFile("file")
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "missing file field")
		return in, false
	}
	defer func() { _ = file.Close() }()

	in.folder = strings.TrimSpace(r.FormValue("folder"))
	in.autotag = isTrue(r.FormValue("autotag"))
	in.taggerName = strings.TrimSpace(r.FormValue("tagger_name"))
	in.via = strings.TrimSpace(r.FormValue("via"))
	if err := validateVia(in.via); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}
	in.source = strings.TrimSpace(r.FormValue("source"))
	in.postID = strings.TrimSpace(r.FormValue("post_id"))
	in.url = strings.TrimSpace(r.FormValue("url"))
	in.md5 = strings.TrimSpace(r.FormValue("md5"))
	in.parentURL = strings.TrimSpace(r.FormValue("parent_url"))
	in.commentary = strings.TrimSpace(r.FormValue("commentary"))
	in.original = strings.TrimSpace(r.FormValue("original"))
	in.notes = parseNotesField(r.FormValue("notes"))
	if tagsJSON := r.FormValue("tags"); tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &in.initialTags); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "tags must be a JSON array of names")
			return in, false
		}
	}
	in.collection = strings.TrimSpace(r.FormValue("collection"))
	if raw := strings.TrimSpace(r.FormValue("collection_order")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "collection_order must be an integer")
			return in, false
		}
		in.collectionOrder = &n
	}
	if err := validateCreateProvenance(in.source, in.postID, in.url, in.md5, in.parentURL, in.collection, in.commentary, in.original, in.collectionOrder); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}

	destDir, destErr := gallery.ResolveSubdir(g.GalleryPath, in.folder)
	if destErr != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
		return in, false
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to create folder: "+err.Error())
		return in, false
	}

	// Write directly to the final destination so the watcher sees
	// the real filename rather than a temp one (which would get
	// marked missing as soon as we renamed it).
	dstPath := gallery.UniqueDestPath(destDir, fh.Filename)
	dst, err := os.Create(dstPath)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to create destination file")
		return in, false
	}
	if _, err := io.Copy(dst, file); err != nil {
		_ = dst.Close()
		_ = os.Remove(dstPath)
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to save upload")
		return in, false
	}
	_ = dst.Close()

	in.imgPath = dstPath
	in.uploadedToDisk = true
	return in, true
}

// parseCreateJSON reads mode B (path reference): validates the fields and
// constrains the caller-supplied path to the gallery root. ok=false means
// the error response was already written.
func (h *Handler) parseCreateJSON(w http.ResponseWriter, r *http.Request, g Gallery) (createInput, bool) {
	var in createInput
	var body struct {
		Path            string           `json:"path"`
		Tags            []string         `json:"tags"`
		Folder          string           `json:"folder"`
		Autotag         bool             `json:"autotag"`
		TaggerName      string           `json:"tagger_name"`
		Via             string           `json:"via"`
		Source          string           `json:"source"`
		PostID          string           `json:"post_id"`
		URL             string           `json:"url"`
		MD5             string           `json:"md5"`
		ParentURL       string           `json:"parent_url"`
		Commentary      string           `json:"commentary"`
		Original        string           `json:"original"`
		Notes           []annotationJSON `json:"notes"`
		Collection      string           `json:"collection"`
		CollectionOrder *int             `json:"collection_order"`
	}
	if !decodeJSON(w, r, &body) {
		return in, false
	}
	if body.Path == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "path is required")
		return in, false
	}
	in.imgPath = body.Path
	in.initialTags = body.Tags
	in.folder = strings.TrimSpace(body.Folder)
	in.autotag = body.Autotag
	in.taggerName = strings.TrimSpace(body.TaggerName)
	in.via = strings.TrimSpace(body.Via)
	if err := validateVia(in.via); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}
	in.source = strings.TrimSpace(body.Source)
	in.postID = strings.TrimSpace(body.PostID)
	in.url = strings.TrimSpace(body.URL)
	in.md5 = strings.TrimSpace(body.MD5)
	in.parentURL = strings.TrimSpace(body.ParentURL)
	in.commentary = strings.TrimSpace(body.Commentary)
	in.original = strings.TrimSpace(body.Original)
	in.notes = annotationsFromInput(body.Notes)
	in.collection = strings.TrimSpace(body.Collection)
	in.collectionOrder = body.CollectionOrder
	if err := validateCreateProvenance(in.source, in.postID, in.url, in.md5, in.parentURL, in.collection, in.commentary, in.original, in.collectionOrder); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}

	// Relative path + folder: resolve under <gallery>/<folder>/<path>.
	// Absolute paths go through the gate just below.
	if in.folder != "" && !filepath.IsAbs(in.imgPath) {
		destDir, destErr := gallery.ResolveSubdir(g.GalleryPath, in.folder)
		if destErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
			return in, false
		}
		in.imgPath = filepath.Join(destDir, in.imgPath)
	}

	// Constrain the caller-supplied path to the gallery root. The
	// operator owns the gallery folder and the API is the operator-
	// facing surface, so an ingest-by-path that quietly registers a
	// row pointing outside the gallery would have a later
	// DELETE /api/v1/images/{id} unlink files the operator never
	// meant to manage. Mirror the upload form's containment.
	absPath, absErr := filepath.Abs(in.imgPath)
	if absErr != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid path")
		return in, false
	}
	galleryAbs, gErr := filepath.Abs(g.GalleryPath)
	if gErr != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "gallery path unresolvable")
		return in, false
	}
	if !gallery.PathInside(galleryAbs, absPath) {
		apiError(w, http.StatusBadRequest, "invalid_request", "path must be inside the gallery root")
		return in, false
	}
	in.imgPath = absPath

	// Translate the common client-side mistake (path doesn't exist)
	// to a 400 with a sanitised message so the response body doesn't
	// echo the operator's filesystem layout and the status class
	// reflects the caller error rather than a server failure.
	if _, statErr := os.Stat(in.imgPath); os.IsNotExist(statErr) {
		apiError(w, http.StatusBadRequest, "not_found", "file not found")
		return in, false
	}
	return in, true
}

// createImage handles POST /api/v1/images. Accepts either multipart
// (with `file`, `tags`, `folder`, `autotag`, `tagger_name`, `via`) or
// JSON (with `path`, `tags`, `folder`, `autotag`, `tagger_name`,
// `via`). In JSON mode `folder` only applies to relative paths; either
// form must resolve inside the gallery root. `via` lands on `images.origin` and
// is attached to each initial tag's `image_tags.tagger_name`. The
// optional provenance fields `source`, `url`, `collection`, and
// `collection_order` are written onto the new row; a duplicate-SHA
// insert instead merges the pushed source, tags, commentary and notes
// into the existing row and adds the collection as a membership that
// never displaces an existing home.
func (h *Handler) createImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var in createInput
	if isMultipart(r.Header.Get("Content-Type")) {
		in, ok = h.parseCreateMultipart(w, r, g)
	} else {
		in, ok = h.parseCreateJSON(w, r, g)
	}
	if !ok {
		return
	}

	// Enforce gallery.max_file_size_mb for both modes. Multipart also
	// has MaxBytesReader; this mainly guards the JSON path-reference
	// mode where the caller supplies an absolute path.
	if maxMB := h.cfg.Gallery.MaxFileSizeMB; maxMB > 0 {
		if info, err := os.Stat(in.imgPath); err == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				if in.uploadedToDisk {
					_ = os.Remove(in.imgPath)
				}
				apiError(w, http.StatusRequestEntityTooLarge, "file_too_large",
					fmt.Sprintf("file exceeds max size (%d MB)", maxMB))
				return
			}
		}
	}

	fileType, ftErr := gallery.DetectFileType(in.imgPath)
	if ftErr != nil {
		if in.uploadedToDisk {
			_ = os.Remove(in.imgPath)
		}
		apiError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}
	// DetectFileType only checks the extension, so a follow-up
	// DecodeConfig confirms the bytes parse as an image before the row
	// lands in the DB. cbz integrity is verified inside Ingest and
	// video frames decode later via ffmpeg, so both buckets skip this.
	if !gallery.IsVideoType(fileType) && fileType != models.FileTypeCBZ {
		if !canDecodeImage(in.imgPath) {
			// ffmpeg decodes JPEGs with a chroma subsampling ratio Go's
			// image/jpeg refuses (some CDN resizers emit these); re-encode the
			// uploaded file in place so the dimension probe, thumbnail, and
			// phash that follow can read it. Only a file we just wrote is
			// rewritten, never an operator's path-referenced original.
			rescued := in.uploadedToDisk && fileType == models.FileTypeJPEG &&
				gallery.NormalizeImage(in.imgPath) == nil && canDecodeImage(in.imgPath)
			if !rescued {
				if in.uploadedToDisk {
					_ = os.Remove(in.imgPath)
				}
				apiError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
				return
			}
		}
	}

	// Caller-supplied `via` wins; otherwise multipart defaults to
	// "upload" and JSON path-reference defaults to "ingest".
	origin := in.via
	if origin == "" {
		if in.uploadedToDisk {
			origin = models.OriginUpload
		} else {
			origin = models.OriginIngest
		}
	}

	img, isDuplicate, err := gallery.Ingest(g.DB, g.GalleryPath, g.ThumbnailsPath, in.imgPath, origin)
	if err != nil {
		if in.uploadedToDisk {
			_ = os.Remove(in.imgPath)
		}
		logx.Warnf("api createImage ingest: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	g.invalidate()
	if isDuplicate {
		// A multipart upload just wrote a second copy of bytes the gallery
		// already holds; keeping it leaves a redundant file (and alias) on
		// disk. Drop both so a re-push is metadata-only. A JSON path-reference
		// duplicate is the operator's own second path, so it stays recorded as
		// an alias. Either way the pushed tags and provenance fold into the
		// existing row instead of being discarded (issue #6).
		aliasAdded := true
		if in.uploadedToDisk {
			aliasAdded = false
			gallery.DropDuplicateCopy(g.DB, img.ID, in.imgPath, "api createImage")
		}
		sum, tagWarnings, mergeErr := mergeSource(g, img.ID, in.source, in.postID, in.url, in.md5, in.parentURL, in.initialTags)
		if mergeErr != nil {
			logx.Warnf("api createImage merge: %v", mergeErr)
			apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+mergeErr.Error())
			return
		}
		linkParentRelations(g, img.ID, in.url, in.parentURL)
		if step, err := applySourceProvenance(g, img.ID, in.source, in.postID, in.commentary, in.original, in.notes); err != nil {
			logx.Warnf("api createImage %s: %v", step, err)
			apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
			return
		}
		if in.collection != "" {
			// Additive: a pool push whose page the gallery already holds still
			// files that image into the pool, without displacing an existing home.
			if err := gallery.AddCollectionMembership(g.DB, img.ID, in.collection, in.collectionOrder); err != nil {
				logx.Warnf("api createImage collection: %v", err)
				apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
				return
			}
		}
		g.invalidate()
		resp, respErr := h.buildImageResponse(g, img.ID)
		if respErr != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
			return
		}
		envelope := map[string]any{
			"image":       resp,
			"alias_added": aliasAdded,
			"merge":       sum,
		}
		if len(tagWarnings) > 0 {
			envelope["tag_warnings"] = tagWarnings
		}
		writeJSON(w, http.StatusOK, envelope)
		return
	}

	// A freshly-created row records its provenance directly; the duplicate
	// path above merges instead.
	if err := applyCreateProvenance(g, img.ID, in.source, in.postID, in.url, in.md5, in.parentURL, in.collection, in.commentary, in.original, in.collectionOrder); err != nil {
		logx.Warnf("api createImage provenance: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to set provenance fields")
		return
	}
	linkParentRelations(g, img.ID, in.url, in.parentURL)
	if in.source != "" && len(in.notes) > 0 {
		if err := gallery.ReplaceSourceAnnotations(g.DB, img.ID, in.source, in.postID, in.notes); err != nil {
			logx.Warnf("api createImage annotations: %v", err)
		}
	}

	// Imported tags are attributed to their source so each source owns a
	// prunable slice; a sourceless push keeps the caller's via label.
	tagVia := in.via
	if in.source != "" {
		tagVia = in.source
	}
	tagWarnings := h.applyInitialTags(g, img.ID, in.initialTags, tagVia)

	var autotagNote string
	if in.autotag {
		if !tagger.IsAvailable(h.cfg) {
			autotagNote = "autotag skipped: tagger not available"
		} else {
			selected, selErr := h.selectedTaggers(g.Name, in.taggerName)
			if selErr != nil {
				autotagNote = "autotag skipped: " + selErr.Error()
			} else if err := h.jobs.Start("autotag"); err != nil {
				autotagNote = "autotag skipped: a job is already running"
			} else {
				imgID := img.ID
				database := g.DB
				invalidate := g.InvalidateCaches
				mangaCache := gallery.MangaCacheDir(g.ThumbnailsPath)
				go func() {
					skipped, err := tagger.RunWithTaggers(h.jobs.Context(), database, h.cfg, []int64{imgID}, selected, h.jobs, h.cfg.Tagger.ExecutionProvider, mangaCache)
					if invalidate != nil {
						invalidate()
					}
					if err != nil {
						h.jobs.Fail(err.Error())
						return
					}
					if skipped > 0 {
						h.jobs.Complete(fmt.Sprintf("auto-tagger skipped image #%d", imgID))
						return
					}
					h.jobs.Complete(fmt.Sprintf("auto-tagged image #%d", imgID))
				}()
				autotagNote = "autotag job started"
			}
		}
	}

	resp, err := h.buildImageResponse(g, img.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
		return
	}

	// Wrap the response when we have side-channel info to attach.
	if len(tagWarnings) > 0 || autotagNote != "" {
		envelope := map[string]any{"image": resp}
		if len(tagWarnings) > 0 {
			envelope["tag_warnings"] = tagWarnings
		}
		if autotagNote != "" {
			envelope["autotag"] = autotagNote
		}
		writeJSON(w, http.StatusCreated, envelope)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// selectedTaggers resolves a caller-supplied tagger_name to a concrete
// list of taggers running on the named gallery. Empty name means every
// tagger enabled + available + applicable to that gallery.
func (h *Handler) selectedTaggers(gallery, name string) ([]tagger.TaggerStatus, error) {
	enabled := tagger.EnabledTaggersForGallery(h.cfg, gallery)
	if name == "" {
		return enabled, nil
	}
	for _, t := range enabled {
		if t.Name == name {
			return []tagger.TaggerStatus{t}, nil
		}
	}
	return nil, fmt.Errorf("tagger %q is not enabled or available for gallery %q", name, gallery)
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (h *Handler) deleteImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}

	result, err := gallery.DeleteImage(g.DB, g.GalleryPath, g.ThumbnailsPath, id, tags.RemoveAllTagsFromImageTx, relationsOnDelete(g.RelationsSvc))
	if err != nil {
		// ErrNoRows on the canonical-path lookup is the genuine "no such
		// id"; a busy write pool or a filesystem refusal is ours, and a
		// caller retrying one told it does not exist never gets there.
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, http.StatusNotFound, "not_found", "image not found")
			return
		}
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	g.invalidate()

	// Empty-source-folder cleanup is opt-in via ?delete_empty_folder=true.
	// Operators create folders deliberately, so a delete leaves an emptied
	// folder in place by default, matching the UI (§7.6); when asked, prune
	// it and report the removal in a structured 200.
	folderRemoved := false
	if r.URL.Query().Get("delete_empty_folder") == "true" && !result.IsMissing && result.FolderPath != "" {
		fullFolderPath := filepath.Join(g.GalleryPath, result.FolderPath)
		if !gallery.PathInside(g.GalleryPath, fullFolderPath) {
			logx.Warnf("api deleteImage: refusing to remove folder %q outside gallery root %q", fullFolderPath, g.GalleryPath)
		} else if entries, readErr := os.ReadDir(fullFolderPath); readErr == nil && len(entries) == 0 {
			if removeErr := os.Remove(fullFolderPath); removeErr == nil {
				folderRemoved = true
			} else {
				logx.Warnf("api deleteImage: failed to remove empty folder %q: %v", fullFolderPath, removeErr)
			}
		}
	}

	if folderRemoved {
		writeJSON(w, http.StatusOK, map[string]any{
			"folder_deleted": true,
			"folder":         result.FolderPath,
		})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) searchImages(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	queryStr := q.Get("q")
	sortStr := q.Get("sort")
	sortStr = cmp.Or(sortStr, "newest")
	orderStr := q.Get("order")
	orderStr = cmp.Or(orderStr, "desc")

	offset, limit := parsePage(r, h.cfg.UI.PageSize, 200)
	pageNum := offset/limit + 1

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid search query: "+parseErr.Error())
		return
	}
	// Stable random ordering across paginated calls relies on the caller
	// passing the same seed back; without one, every call reseeds and
	// pages overlap. Spec §8.3.
	var randomSeed int64
	if seedStr := q.Get("seed"); seedStr != "" {
		if s, err := strconv.ParseInt(seedStr, 10, 64); err == nil && s != 0 {
			randomSeed = s
		}
	}
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		Page:       pageNum,
		Limit:      limit,
		RandomSeed: randomSeed,
	}
	if sortStr == "order" {
		sq.OrderCollection = search.PinnedCollectionName(expr)
	}

	result, err := search.Execute(g.DB, sq)
	if serverError(w, err) {
		return
	}

	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}
	tagsByID, tagsErr := loadTagsForImages(g, ids)
	if tagsErr != nil {
		// Tags load failure shouldn't blank the whole search response;
		// log and fall through with empty tag lists per row.
		logx.Warnf("api searchImages tag load: %v", tagsErr)
		tagsByID = nil
	}
	aliasesByID, aliasErr := loadAliasesForImages(g, ids)
	if aliasErr != nil {
		logx.Warnf("api searchImages alias load: %v", aliasErr)
		aliasesByID = nil
	}

	images := make([]imageResponse, 0, len(result.Results))
	for _, img := range result.Results {
		images = append(images, makeImageResponse(g, img, tagsByID[img.ID], aliasesByID[img.ID]))
	}

	writePage(w, result.Page, result.Limit, result.Total, images)
}

// loadAliasesForImages batch-loads non-canonical image_paths rows for
// every id in the slice in one round-trip, mirroring the per-row read
// in buildImageResponse. Used by the search projection so a multi-id
// response carries the same alias array shape as the single-image GET.
func loadAliasesForImages(g Gallery, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders, args := db.InPlaceholders(ids)
	rows, err := g.DB.Read.Query(
		`SELECT image_id, path FROM image_paths
		 WHERE is_canonical = 0 AND image_id IN (`+placeholders+`)
		 ORDER BY image_id, id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var imageID int64
		var path string
		if err := rows.Scan(&imageID, &path); err != nil {
			return nil, err
		}
		out[imageID] = append(out[imageID], path)
	}
	return out, rows.Err()
}

// loadTagsForImages batch-loads image_tags ⋈ tags ⋈ tag_categories for
// every id in the slice with a single round-trip. Empty input returns
// an empty map so callers can skip the if-empty check.
func loadTagsForImages(g Gallery, ids []int64) (map[int64][]imageTagJSON, error) {
	out := make(map[int64][]imageTagJSON, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders, args := db.InPlaceholders(ids)
	rows, err := g.DB.Read.Query(`
		SELECT it.image_id, t.name, tc.name, it.is_auto, it.confidence, it.tagger_name
		FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE it.image_id IN (`+placeholders+`)
		ORDER BY it.image_id, tc.name, t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var imageID int64
		var tj imageTagJSON
		var tn *string
		if err := rows.Scan(&imageID, &tj.Name, &tj.Category, &tj.IsAuto, &tj.Confidence, &tn); err != nil {
			return nil, err
		}
		tj.TaggerName = tn
		out[imageID] = append(out[imageID], tj)
	}
	return out, rows.Err()
}

// loadTagSourcesForImage reads the per-tag source ledger for one image,
// keyed by the tag's category:name form (bare for general) to match the
// search syntax.
func loadTagSourcesForImage(g Gallery, imageID int64) (map[string][]string, error) {
	rows, err := g.DB.Read.Query(`
		SELECT t.name, tc.name, its.source
		FROM image_tag_sources its
		JOIN tags t ON t.id = its.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE its.image_id = ?
		ORDER BY t.name, its.created_at, its.source`, imageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var name, cat, source string
		if err := rows.Scan(&name, &cat, &source); err != nil {
			return nil, err
		}
		key := name
		if cat != "general" {
			key = cat + ":" + name
		}
		out[key] = append(out[key], source)
	}
	return out, rows.Err()
}

// listImageTags handles GET /api/v1/images/:id/tags. Mirrors the
// post-mutation response shape from addImageTags / removeImageTags so
// a caller has one tag-listing endpoint to pin against. The full image
// object remains reachable via GET /api/v1/images/:id for callers who
// need adjacent metadata.
func (h *Handler) listImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, loadImageTagsJSON(g, id))
}

// addImageTags handles POST /api/v1/images/:id/tags. Each entry can
// be a plain name (general category) or "category:name", matching the
// web UI's tag input.
func (h *Handler) addImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	var body struct {
		Tags []string `json:"tags"`
		Via  string   `json:"via"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Tags) == 0 {
		// A missing or empty `tags` field would loop zero times and
		// return 200 + the existing tag list - a silent success the
		// caller can't tell apart from a real no-op. The OpenAPI
		// declares `tags` required; reject the request shape.
		apiError(w, http.StatusBadRequest, "invalid_request",
			"`tags` is required and must contain at least one name")
		return
	}
	via := strings.TrimSpace(body.Via)
	if err := validateVia(via); badRequest(w, err) {
		return
	}

	tagWarnings := h.applyInitialTags(g, id, body.Tags, via)
	g.invalidate()

	h.writeImageTagsResponse(w, g, id, tagWarnings)
}

// writeImageTagsResponse is the shared post-mutation tail of the tag add /
// remove handlers: the image's tag list, wrapped with warnings when any
// token failed to resolve.
func (h *Handler) writeImageTagsResponse(w http.ResponseWriter, g Gallery, id int64, tagWarnings []string) {
	tags := loadImageTagsJSON(g, id)
	if len(tagWarnings) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"tags":         tags,
			"tag_warnings": tagWarnings,
		})
		return
	}
	writeJSON(w, http.StatusOK, tags)
}

// loadImageTagsJSON reads just the tag list the tag endpoints answer with.
// The full image response carries the same join, but its provenance,
// collection and lookup reads are freight no tag response serves.
func loadImageTagsJSON(g Gallery, imageID int64) []imageTagJSON {
	byID, err := loadTagsForImages(g, []int64{imageID})
	if err != nil {
		logx.Warnf("image tags: %v", err)
		return []imageTagJSON{}
	}
	if tags := byID[imageID]; tags != nil {
		return tags
	}
	return []imageTagJSON{}
}

// imageExists short-circuits the tag-mutation handlers so a request
// against a missing id returns 404 before any per-token work runs.
// Without it the per-tag inserts hit the FK constraint and surface as
// warnings the caller never sees (the final buildImageResponse 404
// supersedes them), and a token's GetOrCreateTag run still leaves a
// stray vocabulary row behind.
func imageExists(g Gallery, id int64) bool {
	var n int
	return g.DB.Read.QueryRow(`SELECT 1 FROM images WHERE id = ?`, id).Scan(&n) == nil
}

// applyInitialTags resolves each raw token (`bare` or `category:bare`)
// to a tag id (creating missing rows), then fans the batch through
// AddTagsToOneImage in one writer tx. Per-tag failures land in
// warnings without aborting; the apply call's own failure does too.
func (h *Handler) applyInitialTags(g Gallery, imgID int64, rawTags []string, via string) []string {
	// Tags applied through the REST API are attributed to "api" when the
	// caller gives no explicit source, so they read with an api origin on
	// the tags page (and an api source group on the detail page) rather
	// than looking like anonymous UI adds. A caller-supplied via still
	// wins and is recorded verbatim.
	via = cmp.Or(via, "api")
	tagIDs, warnings := resolveTagNames(g, rawTags, via)
	if len(tagIDs) > 0 {
		if _, err := g.TagSvc.AddTagsToOneImage(imgID, tagIDs, via); err != nil {
			warnings = append(warnings, "apply tags: "+err.Error())
		}
	}
	return warnings
}

// resolveTagNames turns raw `bare` / `category:bare` tokens into tag ids,
// creating missing rows stamped with origin (the pushing site or the
// caller's via). Per-token failures land in warnings without aborting
// the batch.
func resolveTagNames(g Gallery, rawTags []string, origin string) ([]int64, []string) {
	var warnings []string
	tagIDs := make([]int64, 0, len(rawTags))
	for _, tagName := range rawTags {
		catID, bareName, err := resolveCategoryTag(g, tagName)
		if err != nil {
			warnings = append(warnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tag, err := g.TagSvc.GetOrCreateTagFrom(bareName, catID, origin)
		if err != nil {
			warnings = append(warnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tagIDs = append(tagIDs, tag.ID)
	}
	return tagIDs, warnings
}

// resolveCategoryTag splits "artist:foo" into (artist_id, "foo") when
// "artist" names a real category, otherwise returns (general_id, input)
// so colon-bearing tag names like "nier:automata" or ":3" round-trip
// without a warning.
func resolveCategoryTag(g Gallery, input string) (int64, string, error) {
	input = strings.TrimSpace(input)
	catName := "general"
	tagName := input
	if idx := strings.Index(input, ":"); idx > 0 {
		catID, ok, err := categoryIDByName(g, input[:idx])
		if err != nil {
			return 0, "", err
		}
		if ok {
			return catID, input[idx+1:], nil
		}
	}
	catID, ok, err := categoryIDByName(g, catName)
	if err != nil {
		return 0, "", err
	}
	if !ok {
		return 0, "", fmt.Errorf("unknown category %q", catName)
	}
	return catID, tagName, nil
}

// removeImageTags handles DELETE /api/v1/images/:id/tags. Each entry
// is plain (any single match) or "category:name" (exact category). A
// plain name matching more than one category on the image returns 409
// so the caller can disambiguate.
func (h *Handler) removeImageTags(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Tags) == 0 {
		// Match the POST path: a wrong-shape body must be rejected
		// rather than 200ing with the current tag list and no diagnostic.
		apiError(w, http.StatusBadRequest, "invalid_request",
			"`tags` is required and must contain at least one name")
		return
	}

	var tagWarnings []string
	tagIDs := make([]int64, 0, len(body.Tags))
	for _, tagName := range body.Tags {
		tagID, err := h.resolveImageTagID(g, id, tagName)
		if err != nil {
			apiError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if tagID == 0 {
			// Tag not on this image; silently ignored per the docs.
			continue
		}
		tagIDs = append(tagIDs, tagID)
	}
	if len(tagIDs) > 0 {
		if err := g.TagSvc.RemoveTagsFromOneImage(id, tagIDs); err != nil {
			tagWarnings = append(tagWarnings, "remove tags: "+err.Error())
		}
	}
	g.invalidate()

	h.writeImageTagsResponse(w, g, id, tagWarnings)
}

// resolveImageTagID returns the tag_id attached to imageID that matches
// tagName. A "category:name" input targets that exact category when
// the prefix is a real category; otherwise the whole string is matched
// as a literal tag name. A real-category prefix that misses on the
// image falls through to the literal-name branch so an oddly-stored
// general tag like "artist:foo" is still removable. A plain name is
// accepted only when it resolves to exactly one tag on the image.
// (0, nil) means the tag isn't present.
func (h *Handler) resolveImageTagID(g Gallery, imageID int64, tagName string) (int64, error) {
	tagName = strings.TrimSpace(tagName)
	if idx := strings.Index(tagName, ":"); idx > 0 {
		catName := tagName[:idx]
		_, ok, err := categoryIDByName(g, catName)
		if err != nil {
			return 0, err
		}
		if ok {
			bareName := tagName[idx+1:]
			var tagID int64
			if err := g.DB.Read.QueryRow(
				`SELECT t.id FROM image_tags it
				 JOIN tags t             ON t.id  = it.tag_id
				 JOIN tag_categories tc  ON tc.id = t.category_id
				 WHERE it.image_id = ? AND t.name = ? AND tc.name = ?`,
				imageID, bareName, catName,
			).Scan(&tagID); err == nil {
				return tagID, nil
			}
			// Category-qualified miss: fall through.
		}
	}

	ids, err := db.QueryIDs(g.DB.Read,
		`SELECT t.id FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = ?`,
		imageID, tagName,
	)
	if err != nil {
		return 0, fmt.Errorf("tag lookup failed: %w", err)
	}
	switch len(ids) {
	case 0:
		return 0, nil
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("tag %q exists on this image in multiple categories; use category:name", tagName)
	}
}

func isMultipart(ct string) bool {
	return strings.HasPrefix(ct, "multipart/form-data")
}
