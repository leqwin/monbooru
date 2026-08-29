package web

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
	"github.com/monbooru/monbooru/internal/upgrade"
)

// ratingCeilingPost sets or clears the monbooru_rating_ceiling cookie.
// Posting level=explicit (or any out-of-enum value) clears the cookie so
// the empty-storage steady state means "no ceiling".
func (s *Server) ratingCeilingPost(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	level = cmp.Or(level, r.FormValue("level"))
	writeRatingCookie(w, level)
	w.WriteHeader(http.StatusNoContent)
}

// toggleBoolColumn flips a 0/1 column in images via RETURNING, drops
// the per-gallery caches, and writes one of two pre-rendered button
// fragments depending on the new value. Shared by toggleFavorite and
// toggleInbox; oob (nil for favorite) appends an out-of-band fragment
// so a layout counter can follow the flip.
func (s *Server) toggleBoolColumn(w http.ResponseWriter, r *http.Request, column, onHTML, offHTML string, oob func(*http.Request) string) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var newVal int
	if err := s.db().Write.QueryRow(
		`UPDATE images SET `+column+` = 1 - `+column+` WHERE id = ? RETURNING `+column, id,
	).Scan(&newVal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Cached match-id sets keyed off the toggled column (`?q=fav:true`,
	// `?q=inbox:true`) and the cached inbox count both go stale on flip.
	if cx := s.Active(); cx != nil {
		cx.InvalidateCaches()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if newVal == 1 {
		_, _ = w.Write([]byte(onHTML))
	} else {
		_, _ = w.Write([]byte(offHTML))
	}
	if oob != nil {
		_, _ = w.Write([]byte(oob(r)))
	}
}

// toggleFavorite returns the swap HTML for the favorite button. The
// FE0E after the filled heart pins it to text presentation; bare U+2665
// falls back to a colour-emoji font and outsizes the outline heart.
func (s *Server) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, "is_favorited",
		`<button type="submit" id="fav-btn" class="btn-fav active" title="Unfavorite">♥&#xFE0E;</button>`,
		`<button type="submit" id="fav-btn" class="btn-fav" title="Favorite">♡</button>`,
		nil,
	)
}

// toggleInbox returns the swap HTML for the inbox button. The title
// names the click action (Archive / Send to inbox); the label names
// the row's current state (In inbox / Archived) so the button reads
// as "this is what it is" with the action surfaced on hover.
func (s *Server) toggleInbox(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, "is_inbox",
		`<button type="submit" id="inbox-btn" class="btn-inbox active" title="Archive (i)">In inbox</button>`,
		`<button type="submit" id="inbox-btn" class="btn-inbox" title="Send to inbox (i)">Archived</button>`,
		s.inboxNavOOB,
	)
}

// inboxNavOOB re-renders the topbar inbox link out-of-band so its count
// follows the toggle; swapping the detail button alone leaves the layout
// counter stale until the next full render. Mirrors base()'s ceiling-aware
// InboxCountUnder so the OOB value matches a full render.
func (s *Server) inboxNavOOB(r *http.Request) string {
	cx := s.Active()
	if cx == nil {
		return ""
	}
	n, err := cx.InboxCountUnder(resolveCeiling(r, cx))
	if err != nil {
		return ""
	}
	suffix := ""
	if n > 0 {
		suffix = fmt.Sprintf(" (%d)", n)
	}
	return fmt.Sprintf(`<a id="inbox-nav" href="/?q=inbox:true" hx-swap-oob="true">Inbox%s</a>`, suffix)
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}

	back := parseBackContext(r)

	// When the caller arrived via a Similar-images click the URL carries
	// ref=<sourceID> and the back_* params describe the source's gallery
	// context, not a search the current image is part of. Snapshotting the
	// valid source id up front keeps the post-delete redirect aimed at the
	// original image instead of jumping to an arbitrary neighbour of the
	// unrelated back_* search.
	var refID *int64
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if parsed, err := strconv.ParseInt(refStr, 10, 64); err == nil && parsed != id {
			refID = &parsed
		}
	}

	// Compute the neighbour before the delete so we don't miss it once the
	// current row is gone. When the caller carried back_* params the detail
	// page had a search context; we keep the user in that stream by jumping
	// to the adjacent image instead of bouncing back to the gallery. Ref
	// takes precedence over adjacency: the current image may not even be in
	// the referring search.
	var prevID, nextID *int64
	if refID == nil && (back.Sort != "" || back.Q != "") {
		sortStr := back.Sort
		sortStr = cmp.Or(sortStr, "newest")
		orderStr := back.Order
		orderStr = cmp.Or(orderStr, search.DefaultOrder(sortStr))
		prevID, nextID = s.findAdjacentImages(r.Context(), id, back.Q, sortStr, orderStr, back.Seed, resolveCeiling(r, s.Active()))
	}

	_, err := gallery.DeleteImage(s.db(), s.galleryPath(), s.thumbnailsPath(), id, tags.RemoveAllTagsFromImageTx, s.onImageDeleteCallback())
	if err != nil {
		// ErrNoRows on the initial canonical-path lookup is the genuine
		// "no such image id" case; everything else (write-pool busy,
		// FK constraint, filesystem permission) is a server-side
		// failure and should not masquerade as 404.
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		logx.Errorf("delete image %d: %v", id, err)
		flashStatus(w, http.StatusInternalServerError, "Delete failed; check server log.")
		return
	}
	s.Active().InvalidateCaches()

	redirectURL := ""
	switch {
	case refID != nil:
		redirectURL = back.DetailURL(*refID)
	case nextID != nil:
		redirectURL = back.DetailURL(*nextID)
	case prevID != nil:
		redirectURL = back.DetailURL(*prevID)
	default:
		redirectURL = back.GalleryURL()
	}

	flashText := fmt.Sprintf("Deleted image #%d.", id)
	if isHTMXRequest(r) {
		// Ref case: the user arrived here via a Similar-images click, which
		// itself may be any depth into a chain. Redirecting to the source
		// would push a fresh history entry that drops the ref chain - the
		// post-delete source page then has no data-ref and Escape escapes
		// straight to the gallery. Fire a delete-go-back trigger instead so
		// the client can prefer history.back(), landing on the source's
		// original URL (with its own ref intact) and keeping the chain
		// walkable. The fallback URL handles the cold-load case where the
		// browser has no predecessor (direct link, bookmarked tab).
		if refID != nil {
			setFlashHeader(w, flashText, "ok", map[string]any{
				"delete-go-back": map[string]any{"fallback": redirectURL},
			})
			w.WriteHeader(http.StatusOK)
			return
		}
		setFlashHeader(w, flashText, "ok", nil)
		w.Header().Set("HX-Redirect", redirectURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func (s *Server) promoteCanonical(w http.ResponseWriter, r *http.Request) {
	// HTMX callers get the failure as a flash and stay on the detail page;
	// a plain form submit falls back to http.Error.
	fail := func(msg string, code int) {
		if isHTMXRequest(r) {
			setFlashHeader(w, msg, "err", nil)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, msg, code)
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	newCanonical := r.FormValue("path")
	if newCanonical == "" {
		fail("path required", http.StatusBadRequest)
		return
	}

	// Refuse to promote anything that isn't already a tracked alias of this
	// image. Without this check, a user could write an arbitrary string
	// into images.canonical_path and coerce serveImageFile into serving a
	// sibling file whose path happens to live inside the gallery root.
	var aliasExists int
	if err := s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_paths WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	).Scan(&aliasExists); err != nil {
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	if aliasExists == 0 {
		fail("path is not an alias of this image", http.StatusBadRequest)
		return
	}
	if _, statErr := os.Stat(newCanonical); statErr != nil {
		fail("cannot set canonical: file is missing on disk", http.StatusBadRequest)
		return
	}

	if err := s.promoteCanonicalPath(id, newCanonical,
		`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`, id, newCanonical); err != nil {
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	hxDone(w, r, "Canonical path updated.", "", fmt.Sprintf("/images/%d", id))
}

// promoteCanonicalPath demotes every path of the image, promotes the one
// promoteWhere selects, and repoints the row at newPath. folder_path
// travels with it and the caches drop: promoting a path in another
// folder moves the image for folder:/folderonly: search and the cached
// folder tree.
func (s *Server) promoteCanonicalPath(imageID int64, newPath, promoteSQL string, promoteArgs ...any) error {
	newFolder := gallery.FolderPath(s.galleryPath(), newPath)
	if err := db.InWriteTx(s.db().Write, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ?`, imageID); err != nil {
			return err
		}
		if _, err := tx.Exec(promoteSQL, promoteArgs...); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
			newPath, newFolder, imageID)
		return err
	}); err != nil {
		return err
	}
	s.Active().InvalidateCaches()
	return nil
}

const (
	maxExternalSourceLen  = gallery.MaxSourceLabelLen
	maxExternalURLLen     = gallery.MaxSourceURLLen
	maxImageCommentaryLen = gallery.MaxCommentaryLen
	maxImageOriginalLen   = gallery.MaxOriginalLen
	maxAnnotationBodyLen  = gallery.MaxAnnotationBodyLen
	maxImageNoteLen       = 10000
)

// setSource upserts one origin for an image: adding it, updating its url /
// position, or (with a prev identity) relabelling an existing origin. Each
// origin is a site label plus the post's url; images.source / images.url
// mirror the primary one. URLs must start with http:// or https:// so the
// rendered <a href> survives both the html/template scheme sanitiser and the
// explicit allowlist. The detail dialog ships one origin per submit and gets
// HX-Refresh on success, matching setCollection.
func (s *Server) setSource(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	url := strings.TrimSpace(r.FormValue("url"))
	if site == "" && url == "" {
		externalErr(w, r, "source label or url required", http.StatusBadRequest)
		return
	}
	if len(site) > maxExternalSourceLen {
		externalErr(w, r, fmt.Sprintf("source too long (max %d chars)", maxExternalSourceLen), http.StatusBadRequest)
		return
	}
	if url != "" {
		if len(url) > maxExternalURLLen {
			externalErr(w, r, fmt.Sprintf("url too long (max %d chars)", maxExternalURLLen), http.StatusBadRequest)
			return
		}
		if !gallery.ValidExternalURL(url) {
			externalErr(w, r, "url must start with http:// or https://", http.StatusBadRequest)
			return
		}
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	prevSite := strings.TrimSpace(r.FormValue("prev_site"))
	prevPost := strings.TrimSpace(r.FormValue("prev_post"))
	hasPrev := r.FormValue("has_prev") == "1" || prevSite != "" || prevPost != ""
	if hasPrev && (!strings.EqualFold(prevSite, site) || prevPost != postID) {
		if err := gallery.RenameSourceMembership(s.db(), id, prevSite, prevPost, site, postID, url); err != nil {
			externalErr(w, r, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if err := gallery.AddSourceMembership(s.db(), id, site, postID, url); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, "Source updated.", "", "/images/"+strconv.FormatInt(id, 10))
}

// sourceMembershipAction is the shared skeleton for the origin-row edit
// handlers: resolve the image, read site + post id, run the mutation,
// invalidate caches, and redirect back to the detail page.
func (s *Server) sourceMembershipAction(w http.ResponseWriter, r *http.Request, successMsg string, action func(id int64, site, postID string) error) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	postID := strings.TrimSpace(r.FormValue("post_id"))
	if err := action(id, site, postID); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, successMsg, "", "/images/"+strconv.FormatInt(id, 10))
}

// removeSource drops one origin from an image, keyed by its site + post id.
func (s *Server) removeSource(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Source removed.", func(id int64, site, postID string) error {
		return gallery.RemoveSourceMembership(s.db(), id, site, postID)
	})
}

// makeSourcePrimary reorders one origin to primary, so its site / url become
// the scalar mirror the search executor and exports ride.
func (s *Server) makeSourcePrimary(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Primary source updated.", func(id int64, site, postID string) error {
		return gallery.MakeSourcePrimary(s.db(), id, site, postID)
	})
}

// keepLocalFile records or clears the operator's ruling that this origin's
// file is not worth taking, which withholds [upgrade] and drops the image
// out of upgrade:any until the post claims a different md5.
func (s *Server) keepLocalFile(w http.ResponseWriter, r *http.Request) {
	kept := r.FormValue("keep") == "1"
	msg := "Upgrade offered again."
	if kept {
		msg = "Keeping the local file."
	}
	s.sourceMembershipAction(w, r, msg, func(id int64, site, postID string) error {
		return gallery.SetSourceUpgradeKept(s.db(), id, site, postID, kept)
	})
}

// fetchSource enqueues a metadata-only refetch of one source's URL on
// monloader, which maps the post's tags + commentary + notes and enriches this
// image back through the enrich endpoint. The button renders only when
// monloader is paired; a click that can't reach monloader surfaces the error
// inline. The refetch is asynchronous: this records a pending state and returns
// a pill that polls fetchStatusHandler, so the page reflects the enrich outcome.
func (s *Server) fetchSource(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	url := strings.TrimSpace(r.FormValue("url"))
	if url == "" {
		externalErr(w, r, "this source has no url to fetch", http.StatusBadRequest)
		return
	}
	// The route bypasses ContextMiddleware so the outbound call below never
	// runs under ctxMu (a hanging monloader would stall a gallery switch);
	// snapshot the active name instead.
	galleryName := s.activeGallery()
	s.recordFetchStatus(galleryName, id, "pending", "")
	if err := s.EnqueueMetadataFetch(r.Context(), id, galleryName, url); err != nil {
		s.clearFetchStatus(galleryName, id)
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	if isHTMXRequest(r) {
		writeFetchPending(w, id, 0)
		return
	}
	http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// lookupImage enqueues a hash lookup on monloader: backend "all" runs every
// backend monloader has enabled (the booru walk by md5, the PTR index by
// sha256), backend "ptr" targets the PTR alone (the url-less "ptr" source's
// fetch action and the dialog's PTR-only choice), backend "booru" the online
// walk alone. The md5 is hashed on demand from the stored bytes (never
// persisted - sha256 stays the only content key); the result comes back
// through the same enrich / fetch-status callbacks as a source refetch, so
// the pending pill and its poll are reused unchanged.
func (s *Server) lookupImage(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	backend := r.FormValue("backend")
	if backend != "all" && backend != "ptr" && backend != "booru" {
		externalErr(w, r, "unknown lookup backend", http.StatusBadRequest)
		return
	}
	// This route bypasses ContextMiddleware so the outbound call never runs
	// under ctxMu; snapshot the gallery once so the row read and the enqueue
	// name can't straddle a concurrent switch.
	cx := s.Active()
	if cx == nil {
		externalErr(w, r, "no active gallery", http.StatusServiceUnavailable)
		return
	}
	galleryName := cx.Name
	var sha, storedMD5 string
	if err := cx.DB.Read.QueryRow(
		`SELECT sha256, md5 FROM images WHERE id = ?`, id,
	).Scan(&sha, &storedMD5); err != nil {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	var md5 string
	if backend != "ptr" {
		var err error
		if md5, err = lookupMD5(r.Context(), cx, id, storedMD5); err != nil {
			externalErr(w, r, "cannot hash the file: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	hashes := "md5 " + md5 + ", sha256 " + sha
	switch backend {
	case "ptr":
		hashes = "sha256 " + sha
	case "booru":
		hashes = "md5 " + md5
	}
	s.recordFetchLookup(galleryName, id, hashes)
	jobID, err := s.EnqueueHashLookup(r.Context(), id, galleryName, backend, md5, sha, false, false)
	if err != nil {
		s.clearFetchStatus(galleryName, id)
		if errors.Is(err, errPTRUnavailable) {
			externalErr(w, r, err.Error(), http.StatusConflict)
			return
		}
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.recordLookupEnqueued(cx, id, backend, jobID)
	if isHTMXRequest(r) {
		writeFetchPending(w, id, 0)
		return
	}
	http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// replaceImage enqueues a replace job on monloader: download the origin's
// post file and push it back over this image's bytes. Offered per origin
// when its file is known (recorded hash mismatch) or presumed (similarity
// match with no hash claim) to differ from the local one; the gate is
// re-checked here so a stale page can't fire a replace the row no longer
// justifies. Rides the same pending pill / fetch-status flow as a refetch.
// Replacing bytes is the strongest fetch action, so a request while any
// fetch is pending on the image is refused instead of stacked.
func (s *Server) replaceImage(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	postID := strings.TrimSpace(r.FormValue("post_id"))
	// This route bypasses ContextMiddleware so the outbound call never runs
	// under ctxMu; snapshot the gallery once so the row read and the enqueue
	// name can't straddle a concurrent switch.
	cx := s.Active()
	if cx == nil {
		externalErr(w, r, "no active gallery", http.StatusServiceUnavailable)
		return
	}
	galleryName := cx.Name
	src := models.ImageSource{Site: site, PostID: postID}
	if err := cx.DB.Read.QueryRow(
		`SELECT url, similarity, md5_match, upgrade_kept FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
		id, site, postID,
	).Scan(&src.URL, &src.Similarity, &src.MD5Match, &src.UpgradeKept); err != nil {
		externalErr(w, r, "source not found", http.StatusNotFound)
		return
	}
	if !upgrade.Eligible(src) {
		externalErr(w, r, "this source's file is not known to differ from the local one", http.StatusConflict)
		return
	}
	if e, ok := s.loadFetchStatus(galleryName, id); ok && e.State == "pending" {
		externalErr(w, r, "a fetch is already running for this image; wait for it to finish", http.StatusConflict)
		return
	}
	s.recordFetchStatus(galleryName, id, "pending", "")
	if err := s.EnqueueReplace(r.Context(), id, galleryName, src.URL); err != nil {
		s.clearFetchStatus(galleryName, id)
		externalErr(w, r, "could not reach monloader: "+err.Error(), http.StatusBadGateway)
		return
	}
	if isHTMXRequest(r) {
		writeFetchPending(w, id, 0)
		return
	}
	http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// imageIDForm parses the {id} path segment plus the form body shared by the
// detail-page editor handlers, rendering the failure inline for HTMX callers.
func imageIDForm(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, false
	}
	if err := r.ParseForm(); err != nil {
		externalErr(w, r, "bad form", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// externalErr renders the validation error inline for HTMX callers
// (so the dialog can keep its target slot up to date) and falls back
// to plain http.Error for non-HTMX callers.
func externalErr(w http.ResponseWriter, r *http.Request, msg string, code int) {
	hxErr(w, r, msg, msg, code)
}

// hxErr is externalErr's two-message twin for handlers whose htmx flash
// is friendlier than the terse plain-HTTP error.
func hxErr(w http.ResponseWriter, r *http.Request, htmxMsg, plainMsg string, code int) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		flashStatus(w, http.StatusOK, htmxMsg)
		return
	}
	http.Error(w, plainMsg, code)
}

// setNote writes the operator's freeform note for an image. Import paths never
// touch it, so a re-pull can't overwrite what the operator typed here.
func (s *Server) setNote(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if len(note) > maxImageNoteLen {
		externalErr(w, r, fmt.Sprintf("note too long (max %d chars)", maxImageNoteLen), http.StatusBadRequest)
		return
	}
	res, err := s.db().Write.Exec(`UPDATE images SET note = ? WHERE id = ?`, note, id)
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	hxDone(w, r, "Note updated.", "", "/images/"+strconv.FormatInt(id, 10))
}

// setSourceText sets one text field (commentary or original) on an origin,
// length-capped by maxLen. label names the field in the too-long error; the
// setter binds the concrete gallery mutation.
func (s *Server) setSourceText(w http.ResponseWriter, r *http.Request, field, label string, maxLen int, successMsg string, setter func(id int64, site, postID, val string) error) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	site := strings.TrimSpace(r.FormValue("site"))
	if site == "" {
		externalErr(w, r, "source label required", http.StatusBadRequest)
		return
	}
	val := strings.TrimSpace(r.FormValue(field))
	if len(val) > maxLen {
		externalErr(w, r, fmt.Sprintf("%s too long (max %d chars)", label, maxLen), http.StatusBadRequest)
		return
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	if err := setter(id, site, postID, val); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, successMsg, "", "/images/"+strconv.FormatInt(id, 10))
}

// setSourceCommentary sets the artist commentary attributed to one origin. A
// re-pull of that source overwrites it, so it is the source's text, not the
// operator's durable note.
func (s *Server) setSourceCommentary(w http.ResponseWriter, r *http.Request) {
	s.setSourceText(w, r, "commentary", "commentary", maxImageCommentaryLen, "Commentary updated.",
		func(id int64, site, postID, val string) error {
			return gallery.SetSourceCommentary(s.db(), id, site, postID, val)
		})
}

// removeSourceCommentary clears one origin's commentary, leaving the origin
// itself in the sources list.
func (s *Server) removeSourceCommentary(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Commentary removed.", func(id int64, site, postID string) error {
		return gallery.SetSourceCommentary(s.db(), id, site, postID, "")
	})
}

// setSourceOriginal sets the upstream artist source attributed to one origin;
// a re-pull of that source overwrites it. Deliberately freeform - unlike the
// image-level original source's http(s):// check - since a booru declares it as
// one or more lines, URL or not, rendered link-or-text per line. Enforcing a
// URL here would reject that and desync web edits from the enrich / API-create
// paths that fill the field with no such gate.
func (s *Server) setSourceOriginal(w http.ResponseWriter, r *http.Request) {
	s.setSourceText(w, r, "original", "original source", maxImageOriginalLen, "Original source updated.",
		func(id int64, site, postID, val string) error {
			return gallery.SetSourceOriginal(s.db(), id, site, postID, val)
		})
}

// removeSourceOriginal clears one origin's upstream artist source, leaving
// the origin itself in the sources list.
func (s *Server) removeSourceOriginal(w http.ResponseWriter, r *http.Request) {
	s.sourceMembershipAction(w, r, "Original source removed.", func(id int64, site, postID string) error {
		return gallery.SetSourceOriginal(s.db(), id, site, postID, "")
	})
}

// setAnnotation adds a new operator-drawn box (no id) or edits an existing box
// by id, source-pulled or operator-drawn. Coordinates are original-image pixels;
// they are validated non-negative and clamped to the image bounds when the
// dimensions are known. A dimensionless
// image (video / undecodable) accepts the box but won't overlay it - the
// editable list still shows it.
func (s *Server) setAnnotation(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	var wImg, hImg sql.NullInt64
	if err := s.db().Read.QueryRow(`SELECT width, height FROM images WHERE id = ?`, id).Scan(&wImg, &hImg); err != nil {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	x, okX := annotationCoord(r, "x")
	y, okY := annotationCoord(r, "y")
	bw, okW := annotationCoord(r, "w")
	bh, okH := annotationCoord(r, "h")
	if !okX || !okY || !okW || !okH {
		externalErr(w, r, "coordinates must be non-negative integers", http.StatusBadRequest)
		return
	}
	if wImg.Valid && hImg.Valid && wImg.Int64 > 0 && hImg.Int64 > 0 {
		iw, ih := int(wImg.Int64), int(hImg.Int64)
		x = min(x, iw)
		y = min(y, ih)
		bw = min(bw, iw-x)
		bh = min(bh, ih-y)
	}
	if bw <= 0 || bh <= 0 {
		externalErr(w, r, "the box has no area inside the image", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if len(body) > maxAnnotationBodyLen {
		externalErr(w, r, fmt.Sprintf("annotation too long (max %d chars)", maxAnnotationBodyLen), http.StatusBadRequest)
		return
	}
	if raw := strings.TrimSpace(r.FormValue("id")); raw != "" {
		annID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			externalErr(w, r, "bad annotation id", http.StatusBadRequest)
			return
		}
		if err := gallery.UpdateAnnotation(s.db(), annID, x, y, bw, bh, body); err != nil {
			externalErr(w, r, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if err := gallery.AddManualAnnotation(s.db(), id, x, y, bw, bh, body); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, "Annotation updated.", "", "/images/"+strconv.FormatInt(id, 10))
}

// removeAnnotation drops one operator-drawn box by id.
func (s *Server) removeAnnotation(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	annID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
	if err != nil {
		externalErr(w, r, "bad annotation id", http.StatusBadRequest)
		return
	}
	if err := gallery.DeleteAnnotation(s.db(), annID); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, "Annotation removed.", "", "/images/"+strconv.FormatInt(id, 10))
}

// previewMarkup renders a dialog's draft body through the renderer the page
// itself uses, so a preview can never disagree with what saving would show -
// including whether a referenced tag exists in this gallery.
func (s *Server) previewMarkup(w http.ResponseWriter, r *http.Request) {
	if _, ok := imageIDForm(w, r); !ok {
		return
	}
	s.renderTemplate(w, "partials/markup_preview.html", s.renderMarkup(r.FormValue("body")))
}

// annotationCoord parses one non-negative integer coordinate field.
func annotationCoord(r *http.Request, field string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(field)))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// setCollection upserts one membership for an image: adding the image to a
// collection, updating that collection's position, or (with a prev value)
// renaming an existing membership. The detail dialog ships one membership
// per submit and gets HX-Refresh on success, matching setSource.
func (s *Server) setCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		externalErr(w, r, "collection label required", http.StatusBadRequest)
		return
	}
	if len(name) > maxExternalSourceLen {
		externalErr(w, r, fmt.Sprintf("collection too long (max %d chars)", maxExternalSourceLen), http.StatusBadRequest)
		return
	}
	var order *int
	if raw := strings.TrimSpace(r.FormValue("collection_order")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			externalErr(w, r, "order must be an integer or empty", http.StatusBadRequest)
			return
		}
		if n < 1 {
			externalErr(w, r, "order must be 1 or higher", http.StatusBadRequest)
			return
		}
		order = &n
	}
	var err error
	if prev := strings.TrimSpace(r.FormValue("prev")); prev != "" && !strings.EqualFold(prev, name) {
		err = gallery.RenameCollectionMembership(s.db(), id, prev, name, order)
	} else {
		err = gallery.AddCollectionMembership(s.db(), id, name, order)
	}
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, "Collection updated.", "", "/images/"+strconv.FormatInt(id, 10))
}

// removeCollection drops one membership from an image.
func (s *Server) removeCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := imageIDForm(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		externalErr(w, r, "collection label required", http.StatusBadRequest)
		return
	}
	if err := gallery.RemoveCollectionMembership(s.db(), id, name); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, "Collection removed.", "", "/images/"+strconv.FormatInt(id, 10))
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	pathID, ok := pathInt64(w, r, "pathID")
	if !ok {
		return
	}

	// Refuse to remove the canonical path; callers must promote another alias
	// first, otherwise the image would lose its on-disk reference entirely.
	var isCanon int
	var aliasPath string
	if err := s.db().Read.QueryRow(
		`SELECT is_canonical, path FROM image_paths WHERE id = ? AND image_id = ?`, pathID, id,
	).Scan(&isCanon, &aliasPath); err != nil {
		http.Error(w, "alias path not found", http.StatusNotFound)
		return
	}
	if isCanon == 1 {
		http.Error(w, "cannot delete canonical path", http.StatusBadRequest)
		return
	}

	if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, pathID); err != nil {
		logx.Warnf("delete alias row %d: %v", pathID, err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	if aliasPath != "" {
		// Defense-in-depth: every legitimate insert into image_paths goes
		// through ingest / sync / rebaseImagePaths and stays under the
		// gallery root, so the gate below is a no-op today. A future
		// compatibility translator that forgets the relative-path rule
		// would otherwise let this os.Remove unlink any file the process
		// can reach.
		if err := unlinkUnderGallery(s.galleryPath(), aliasPath); err != nil {
			logx.Warnf("delete alias file %q: %v", aliasPath, err)
		}
	}

	if isHTMXRequest(r) {
		// Empty body for HTMX outerHTML swap - removes the row.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(""))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}

// unlinkUnderGallery is os.Remove gated on gallery.PathInside so a
// stray absolute path in image_paths can never let an alias-deletion
// or duplicate-prune handler unlink files outside the active gallery.
func unlinkUnderGallery(galleryRoot, victim string) error {
	galleryAbs, err := filepath.Abs(galleryRoot)
	if err != nil {
		return fmt.Errorf("resolve gallery root: %w", err)
	}
	victimAbs, err := filepath.Abs(victim)
	if err != nil {
		return fmt.Errorf("resolve victim: %w", err)
	}
	if !gallery.PathInside(galleryAbs, victimAbs) {
		return fmt.Errorf("refuse: path %q is outside gallery root", victim)
	}
	if err := os.Remove(victim); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// singleImageMoveJob is the shell moveImage and renameImage share. A
// `move` job is taken even for one image so the watcher suppression the
// batch path relies on applies here too; the job is brief and
// auto-dismisses like any other. op returns the success flash, and
// answered when it has already written the response body itself.
func (s *Server) singleImageMoveJob(w http.ResponseWriter, r *http.Request, doneMsg string, op func(id int64) (flash string, answered bool, err error)) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	if !s.startJob(w, models.JobTypeMove) {
		return
	}
	flash, answered, err := op(id)
	if err != nil {
		s.jobs.Fail(err.Error())
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Active().InvalidateCaches()
	s.jobs.Complete(doneMsg)
	if answered {
		return
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, flash, "ok", nil)
		w.Header().Set("HX-Redirect", fmt.Sprintf("/images/%d", id))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}

// moveImage relocates the one image at {id} into the requested folder.
func (s *Server) moveImage(w http.ResponseWriter, r *http.Request) {
	targetFolder := strings.TrimSpace(r.FormValue("folder"))
	tmpl, parseErr := gallery.ParseNameTemplate(targetFolder, gallery.ScopeMove)
	s.singleImageMoveJob(w, r, "Moved image.", func(id int64) (string, bool, error) {
		if parseErr != nil {
			return "", false, parseErr
		}
		folder, err := s.singleName(r.Context(), tmpl, targetFolder, id)
		if err != nil {
			return "", false, err
		}
		if _, err := gallery.MoveImage(s.db(), s.galleryPath(), id, folder); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Moved image to %s.", cmp.Or(folder, "gallery root")), false, nil
	})
}

// singleName resolves what one image is renamed or moved to: the literal
// when the template carries no tokens, otherwise the row's own render.
func (s *Server) singleName(ctx context.Context, tmpl *gallery.NameTemplate, literal string, id int64) (string, error) {
	if !tmpl.HasTokens() {
		return literal, nil
	}
	facts, err := gallery.LoadNameFacts(ctx, s.db(), s.activeGallery(), id, 0, tmpl)
	if err != nil {
		return "", err
	}
	return tmpl.Render(facts)
}

// renameImage renames the one image at {id}'s file in place. The
// collections-order tile renames inline and wants the final name back
// (a collision may have suffixed it), so that shape answers before the
// shared redirect tail.
func (s *Server) renameImage(w http.ResponseWriter, r *http.Request) {
	newName := strings.TrimSpace(r.FormValue("name"))
	inline := r.FormValue("inline") == "1"
	tmpl, parseErr := gallery.ParseNameTemplate(newName, gallery.ScopeRename)
	s.singleImageMoveJob(w, r, "Renamed image.", func(id int64) (string, bool, error) {
		if parseErr != nil {
			return "", false, parseErr
		}
		name, err := s.singleName(r.Context(), tmpl, newName, id)
		if err != nil {
			return "", false, err
		}
		res, err := gallery.RenameImage(s.db(), s.galleryPath(), id, name)
		if err != nil {
			return "", false, err
		}
		newBase := filepath.Base(res.NewCanonicalPath)
		if inline {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(newBase))
			return "", true, nil
		}
		return fmt.Sprintf("Renamed image to %s.", newBase), false, nil
	})
}

// nextPrefix returns the smallest string strictly greater than prefix
// under the default BINARY collation: prefix with its last byte
// incremented, or prefix+"\xff" when the last byte already saturates.
// Used by folder autocomplete to bound a `>=, <` range scan instead of
// a LIKE-trailing-wildcard scan.
func nextPrefix(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1])
		}
	}
	return prefix + "\xff"
}

// nocasePrefixRange returns the half-open [lo, hi) bounds for a
// case-insensitive prefix match. Folding to lower case keeps the
// byte-incremented upper bound consistent with COLLATE NOCASE (which folds
// to lower case); a raw upper-case-ending prefix like "Z" would otherwise
// exclude every lower-case continuation.
func nocasePrefixRange(prefix string) (lo, hi string) {
	lo = strings.ToLower(prefix)
	return lo, nextPrefix(lo)
}
