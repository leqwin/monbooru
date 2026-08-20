package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// sha256DuplicateRow is one alias path on the SHA-256 duplicates table:
// the owning image, its canonical path on disk, and the alias path to
// remove. The walker page renders one row per alias so a single image
// with three non-canonical paths shows up three times.
type sha256DuplicateRow struct {
	ImageID       int64
	CanonicalPath string
	PathID        int64
	AliasPath     string
}

// markedDuplicateRow names one (group, original, non-original) pairing
// for the marked-duplicates walker table. HasTagsToCopy is true when
// at least one non-rating tag carried by a duplicate of the group is
// absent on the original - it gates the [copy tags] button so empty
// groups don't surface a no-op action.
type markedDuplicateRow struct {
	GroupID       int64
	OriginalID    int64
	DuplicateID   int64
	MarkedAt      string
	HasTagsToCopy bool
}

// relationsWalkerData is the shared template payload for the two
// duplicate walkers. Both render as tables.
type relationsWalkerData struct {
	baseData
	ActiveGallery string
	Kind          string // "sha256" or "marked"
	Sha256Rows    []sha256DuplicateRow
	MarkedRows    []markedDuplicateRow
	Total         int
	Page          int
	TotalPages    int
}

// HasRows reports whether the walk found anything, which is what gates the
// delete-all toolbar. Only one of the two slices is ever populated.
func (d relationsWalkerData) HasRows() bool {
	return len(d.Sha256Rows) > 0 || len(d.MarkedRows) > 0
}

// duplicatesWalkerPageSize caps each walker page. A find-pairs run can
// mark tens of thousands of members on a large library, and both tables
// lift a thumbnail per row.
const duplicatesWalkerPageSize = 100

// walkerPageOffset resolves ?page= against a row count, clamping a
// past-the-end page onto the last one the way the gallery does.
func walkerPageOffset(r *http.Request, total int) (page, totalPages, offset int) {
	page = 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		page = p
	}
	totalPages = 1
	if total > 0 {
		totalPages = (total + duplicatesWalkerPageSize - 1) / duplicatesWalkerPageSize
	}
	if page > totalPages {
		page = totalPages
	}
	return page, totalPages, (page - 1) * duplicatesWalkerPageSize
}

// sha256WalkerPage renders every non-canonical alias path the gallery
// carries in one table. The Walk button on the Relations page links
// here.
func (s *Server) sha256WalkerPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	ceiling := resolveCeiling(r, cx)
	from := ` FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0`
	args := []any{}
	if where, wargs := ceiling.WhereOne("i.id"); where != "" {
		from += ` WHERE ` + where
		args = append(args, wargs...)
	}
	var total int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		logx.Warnf("sha256 walker count: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	page, totalPages, offset := walkerPageOffset(r, total)
	rows, err := cx.DB.Read.Query(
		`SELECT i.id, i.canonical_path, ip.id, ip.path`+from+` ORDER BY i.id, ip.id LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), duplicatesWalkerPageSize, offset)...)
	if err != nil {
		logx.Warnf("sha256 walker query: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()
	var out []sha256DuplicateRow
	for rows.Next() {
		var dr sha256DuplicateRow
		if scanErr := rows.Scan(&dr.ImageID, &dr.CanonicalPath, &dr.PathID, &dr.AliasPath); scanErr != nil {
			logx.Warnf("sha256 walker scan: %v", scanErr)
			http.Error(w, "scan duplicates", http.StatusInternalServerError)
			return
		}
		out = append(out, dr)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "iterate duplicates", http.StatusInternalServerError)
		return
	}
	s.renderTemplate(w, "relations_duplicates_sha256.html", relationsWalkerData{
		baseData:      s.base(r, "relations", "Duplicate files - "+s.booruName()),
		ActiveGallery: s.activeName,
		Kind:          "sha256",
		Sha256Rows:    out,
		Total:         total,
		Page:          page,
		TotalPages:    totalPages,
	})
}

// markedWalkerPage lists every (original, duplicate) pairing across
// every dup_group in one table. One row per non-original member,
// ordered by the membership's created_at descending so the freshest
// markings land at the top - the operator's most recent decisions are
// the ones most likely to need a follow-up action.
func (s *Server) markedWalkerPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	ceiling := resolveCeiling(r, cx)
	from := ` FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id != g.original_image_id`
	args := []any{}
	if where, wargs := ceiling.WhereTwo("g.original_image_id", "m.image_id"); where != "" {
		from += ` AND ` + where
		args = append(args, wargs...)
	}
	var total int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		logx.Warnf("marked walker count: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	page, totalPages, offset := walkerPageOffset(r, total)
	rows, err := cx.DB.Read.Query(
		`SELECT g.id, g.original_image_id, m.image_id, m.created_at`+from+
			` ORDER BY m.created_at DESC, g.id DESC, m.image_id LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), duplicatesWalkerPageSize, offset)...)
	if err != nil {
		logx.Warnf("marked walker query: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()
	var out []markedDuplicateRow
	for rows.Next() {
		var dr markedDuplicateRow
		var marked string
		if scanErr := rows.Scan(&dr.GroupID, &dr.OriginalID, &dr.DuplicateID, &marked); scanErr != nil {
			logx.Warnf("marked walker scan: %v", scanErr)
			http.Error(w, "scan duplicates", http.StatusInternalServerError)
			return
		}
		dr.MarkedAt = humanISOTime(marked)
		out = append(out, dr)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "iterate duplicates", http.StatusInternalServerError)
		return
	}
	if err := annotateTagsToCopy(cx, out); err != nil {
		logx.Warnf("marked walker tags-to-copy: %v", err)
	}
	s.renderTemplate(w, "relations_duplicates_marked.html", relationsWalkerData{
		baseData:      s.base(r, "relations", "Duplicate images - "+s.booruName()),
		ActiveGallery: s.activeName,
		Kind:          "marked",
		MarkedRows:    out,
		Total:         total,
		Page:          page,
		TotalPages:    totalPages,
	})
}

// annotateTagsToCopy sets HasTagsToCopy on every row whose group still
// carries at least one non-rating tag missing from its original. Runs
// one SELECT to gather the set of eligible group ids and stamps the
// result across the slice in O(N) so the walker doesn't pay a per-row
// query.
func annotateTagsToCopy(cx *galleryCtx, rows []markedDuplicateRow) error {
	if len(rows) == 0 {
		return nil
	}
	eligible := map[int64]bool{}
	q, err := cx.DB.Read.Query(`
		SELECT DISTINCT g.id
		FROM dup_groups g
		JOIN dup_group_members m ON m.group_id = g.id
		JOIN image_tags it ON it.image_id = m.image_id
		LEFT JOIN tags t ON t.id = it.tag_id
		LEFT JOIN tag_categories c ON c.id = t.category_id
		WHERE m.image_id != g.original_image_id
		  AND (c.name IS NULL OR c.name != 'rating')
		  AND NOT EXISTS (
		    SELECT 1 FROM image_tags it2
		    WHERE it2.image_id = g.original_image_id AND it2.tag_id = it.tag_id
		  )`)
	if err != nil {
		return err
	}
	defer func() { _ = q.Close() }()
	for q.Next() {
		var gid int64
		if err := q.Scan(&gid); err != nil {
			return err
		}
		eligible[gid] = true
	}
	if err := q.Err(); err != nil {
		return err
	}
	for i := range rows {
		if eligible[rows[i].GroupID] {
			rows[i].HasTagsToCopy = true
		}
	}
	return nil
}

// sha256WalkerRemoveOnePost removes a specific alias path (by id) and
// its file from disk, then bounces back to the walker so the refreshed
// table no longer carries the deleted row.
func (s *Server) sha256WalkerRemoveOnePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	pathID, err := strconv.ParseInt(r.FormValue("path_id"), 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid path id.")
		return
	}
	var aliasPath string
	if err := s.db().Read.QueryRow(
		`SELECT path FROM image_paths WHERE id = ? AND is_canonical = 0`,
		pathID,
	).Scan(&aliasPath); err != nil {
		flashStatus(w, http.StatusNotFound, "Not a non-canonical path.")
		return
	}
	if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, pathID); err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if aliasPath != "" {
		if err := unlinkUnderGallery(s.galleryPath(), aliasPath); err != nil {
			logx.Warnf("sha256 walker unlink %q: %v", aliasPath, err)
		}
	}
	redirectWalker(w, r, "sha256")
}

// markedWalkerDeleteOnePost deletes one image from a dup group through
// the same gallery.DeleteImage path the detail page uses; the
// relations service's OnImageDeleteTx hook cleans the group membership.
func (s *Server) markedWalkerDeleteOnePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	imageID, err := strconv.ParseInt(r.FormValue("image_id"), 10, 64)
	if err != nil {
		flashStatus(w, http.StatusBadRequest, "Invalid image id.")
		return
	}
	if _, err := gallery.DeleteImage(s.db(), s.galleryPath(), s.thumbnailsPath(), imageID, tags.RemoveAllTagsFromImageTx, s.onImageDeleteCallback()); err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Active().InvalidateCaches()
	redirectWalker(w, r, "marked")
}

// markedWalkerDeleteAllPost removes every non-original member from
// every dup_group. Each delete rides gallery.DeleteImage so the file
// is removed alongside the row. The walker page filters rows by the
// active ceiling (so members above the cookie level never surface);
// the bulk-delete mirrors that filter so the operator can't wipe
// rows they can't see.
func (s *Server) markedWalkerDeleteAllPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	q := `
		SELECT m.image_id
		FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id != g.original_image_id`
	args := []any{}
	if where, wargs := resolveCeiling(r, cx).WhereTwo("g.original_image_id", "m.image_id"); where != "" {
		q += ` AND ` + where
		args = append(args, wargs...)
	}
	victims, err := db.QueryIDs(cx.DB.Read, q, args...)
	if err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(victims) == 0 {
		writeInlineFlash(w, "ok", "Removed 0 marked duplicate(s).")
		return
	}
	// Reserve a job slot so the per-image unlinks don't race a concurrent
	// autotag / rebuild-thumbs / vacuum; the response returns immediately
	// and the status bar surfaces progress.
	if !s.startJob(w, models.JobTypeDelete) {
		return
	}
	galleryPath := s.galleryPath()
	thumbnailsPath := s.thumbnailsPath()
	onDelete := s.onImageDeleteCallback()
	go func() {
		ctx := s.jobs.Context()
		total := len(victims)
		s.jobs.Update(0, total, "removing…")
		removed := 0
		for i, id := range victims {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("marked delete-all cancelled (%d/%d)", removed, total))
				s.Active().InvalidateCaches()
				return
			}
			if _, err := gallery.DeleteImage(s.db(), galleryPath, thumbnailsPath, id, tags.RemoveAllTagsFromImageTx, onDelete); err != nil {
				logx.Warnf("marked delete-all image %d: %v", id, err)
				continue
			}
			removed++
			if (i+1)%25 == 0 || i == total-1 {
				s.jobs.Update(i+1, total, "removing…")
			}
		}
		s.Active().InvalidateCaches()
		s.jobs.Complete(fmt.Sprintf("Removed %d marked duplicate(s).", removed))
	}()
	writeInlineFlash(w, "ok", "Marked duplicate removal started.")
}

// redirectWalker writes an HX-Redirect (or 303) back to the walker
// page so the refreshed table reflects the just-completed action.
func redirectWalker(w http.ResponseWriter, r *http.Request, kind string) {
	target := "/relations/duplicates/" + kind
	hxRedirect(w, r, target)
}
