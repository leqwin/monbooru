package web

import (
	"database/sql"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/tags"
)

// validOrderModes enumerates the three session walk orders. Anything
// else collapses to the default (smallest_distance_first).
var validOrderModes = map[string]bool{
	"smallest_distance_first": true,
	"largest_file_first":      true,
	"random":                  true,
}

// collectionPairExcl hides queue pairs whose two images share a
// collection absent from collection_find_relations (the per-collection
// opt-in toggled on /collections): membership already relates the
// images, so the session skips those pairs unless the operator enables
// the switch. The verdict is the stored flag the bootstrap triggers
// maintain, so the queue scans stay free of per-row membership probes.
// Splices anywhere the queue is aliased `p`.
const collectionPairExcl = "p.collection_hidden = 0"

// validDetectors enumerates the session's detector scopes. "both" is
// the unfiltered walk; anything else collapses to it.
var validDetectors = map[string]bool{
	"phash": true,
	"tags":  true,
	"both":  true,
}

// detectorFilter narrows the queue to pairs one detector found. A
// pair both detectors nominated satisfies either scope, and a pair the
// operator reopened satisfies both: it is there because they asked for
// it, so no scope should hide it.
func detectorFilter(mode string) string {
	switch mode {
	case "phash":
		return " AND p.source IN ('phash', 'both', 'review')"
	case "tags":
		return " AND p.source IN ('tags', 'both', 'review')"
	}
	return ""
}

// orderClauseForMode returns the ORDER BY tail the queue SELECT uses.
// The walk only serves unskipped rows, so the mode's own keys are the
// whole order.
func orderClauseForMode(mode string) string {
	base := "ORDER BY "
	switch mode {
	case "largest_file_first":
		return base + "(COALESCE(ia.file_size, 0) + COALESCE(ib.file_size, 0)) DESC, p.distance ASC, p.a_image_id ASC"
	case "random":
		return base + "random()"
	}
	return base + "p.distance ASC, (COALESCE(ia.file_size, 0) + COALESCE(ib.file_size, 0)) DESC, p.a_image_id ASC"
}

// sessionPairView is everything the swipe page needs about one pair.
// A nil view signals an empty queue; the template renders the
// "nothing left" stub.
type sessionPairView struct {
	A         sessionImageView
	B         sessionImageView
	Distance  int
	Remaining int
	Order     string
	// Source names the detector that queued the pair, and Score the tag
	// similarity behind it (0 on a phash-only row). The card renders
	// them because provenance shifts the prior: a pixel match suggests
	// duplicate or version, a tag match suggests variant or based-on.
	Source string
	Score  float64
	// LeftID names whichever of A or B the template should render in
	// the left slot, so the four verdicts commit the likely direction
	// without a swap: the bigger-filesize side on a pixel match, the
	// older image on a tag match. W swap reassigns it client-side.
	LeftID int64
	// SharedAncestor names the nearest image both sides descend from,
	// 0 otherwise. The bridge renders it so a pair from one tree reads
	// as tree context rather than as two strangers.
	SharedAncestor int64
}

// ScorePercent renders the tag score the way the card reads it.
func (v sessionPairView) ScorePercent() int {
	return int(math.Round(v.Score * 100))
}

// FromTags reports whether tag similarity had a hand in queueing the
// pair, which is what gates the shared-tag evidence row.
func (v sessionPairView) FromTags() bool {
	return v.Source == relations.SourceTags || v.Source == relations.SourceBoth
}

// sessionImageView mirrors models.Image but only carries the bits the
// session UI renders (id, size, dimensions, tag count, file type).
// Loaded alongside the queue row in a single SELECT. FileType drives
// the compare slider's <img>-vs-<video> branch.
type sessionImageView struct {
	ID       int64
	Width    sql.NullInt64
	Height   sql.NullInt64
	FileSize int64
	Filename string
	FileType string
	TagCount int
}

// sessionPage renders /relations/session. Picks the next queue row
// according to the persisted order mode (or the ?order= override)
// and serves the two-cell swipe view.
func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	order := r.URL.Query().Get("order")
	if order == "" {
		order = loadSessionOrder(cx)
	}
	if !validOrderModes[order] {
		order = "smallest_distance_first"
	}
	if validOrderModes[r.URL.Query().Get("order")] {
		// Operator switched modes from the picker; persist so a reload picks
		// up the same shuffle. Gate on validity so a bogus ?order= doesn't
		// overwrite the saved preference with the fallback.
		saveSessionOrder(cx, order)
	}
	// The scope opens on the unfiltered walk every time: narrowing it is
	// a choice for the sitting, carried on the URL through the decide
	// loop, not a preference that outlives it. With the tag pass off
	// only one detector is left, so the walk pins to that one.
	tagPairs := s.tagPairsEnabled()
	detector := "phash"
	if tagPairs {
		detector = "both"
		if v := r.URL.Query().Get("detector"); validDetectors[v] {
			detector = v
		}
	}
	ceiling := resolveCeiling(r, cx)
	// review-again links carry the exact pair to reopen; pin it so the
	// operator lands on the pair they clicked, not whatever sorts first.
	pinA, _ := strconv.ParseInt(r.URL.Query().Get("a"), 10, 64)
	pinB, _ := strconv.ParseInt(r.URL.Query().Get("b"), 10, 64)
	pair, counts, err := loadNextPair(cx, order, detector, ceiling, pinA, pinB)
	if err != nil {
		logx.Warnf("session next pair: %v", err)
		http.Error(w, "load pair", http.StatusInternalServerError)
		return
	}
	var leftFacts, rightFacts relationCompareFacts
	var sharedTags []tags.SharedTag
	var sharedTotal int
	if pair != nil {
		// The template puts the bigger-filesize side in slot "left". The
		// compare table mirrors that orientation so the operator's eye
		// reads "left vs right" without the W swap reshuffling the rows.
		leftID := pair.LeftID
		rightID := pair.A.ID
		if leftID == pair.A.ID {
			rightID = pair.B.ID
		}
		leftFacts, rightFacts, err = loadCompareFacts(cx, leftID, rightID)
		if err != nil {
			logx.Debugf("session compare facts: %v", err)
		}
		// A pixel match explains itself on sight; a tag match does not,
		// so the pair's strongest shared tags ride along as the reason
		// it is on screen at all.
		if pair.FromTags() {
			shared, total, sErr := tags.SharedTags(cx.DB, leftID, rightID, sharedTagsShown)
			if sErr != nil {
				logx.Debugf("session shared tags: %v", sErr)
			}
			sharedTags, sharedTotal = shared, total
		}
		if src, ok, sErr := relations.CommonDerivativeAncestor(cx.DB, pair.A.ID, pair.B.ID); sErr != nil {
			logx.Debugf("session shared ancestor: %v", sErr)
		} else if ok {
			pair.SharedAncestor = src
		}
	}
	s.renderTemplate(w, "relations_session.html", sessionPageData{
		baseData:        s.base(r, "relations", "Session - "+s.booruName()),
		Pair:            pair,
		Remaining:       counts.Open,
		HiddenByCeiling: counts.HiddenByCeiling,
		Skipped:         counts.Skipped,
		Ceiling:         ceiling.Level(),
		Order:           order,
		Detector:        detector,
		TagPairs:        tagPairs,
		ActiveGallery:   s.activeName,
		Left:            leftFacts,
		Right:           rightFacts,
		SharedTags:      sharedTags,
		SharedTagsTotal: sharedTotal,
	})
}

// sharedTagsShown caps the evidence row at what reads in one line; the
// total rides alongside as "+N more".
const sharedTagsShown = 6

type sessionPageData struct {
	baseData
	Pair *sessionPairView
	// Remaining is what the walk can still serve. HiddenByCeiling is the
	// number of unresolved pairs filtered out because at least one side
	// carries a rating tag above the cookie ceiling, Skipped the ones
	// set aside this sitting.
	Remaining       int
	HiddenByCeiling int
	Skipped         int
	Ceiling         string
	Order           string
	Detector        string
	// TagPairs mirrors the tag-similarity switch: with the pass off the
	// detector picker is hidden, since every queued pair is a pixel match.
	TagPairs      bool
	ActiveGallery string
	// Left / Right hold the comparison-table data oriented to the
	// template's left/right slots so the table reads consistently with
	// the thumbs.
	Left  relationCompareFacts
	Right relationCompareFacts
	// SharedTags carries the heaviest tags behind a tag-sourced match,
	// empty on a phash-only pair. SharedTagsTotal is the full count.
	SharedTags      []tags.SharedTag
	SharedTagsTotal int
}

// loadSessionOrder reads the order_mode for the singleton session
// row, defaulting if the row is missing.
func loadSessionOrder(cx *galleryCtx) string {
	var mode string
	err := cx.DB.Read.QueryRow(`SELECT order_mode FROM relation_session WHERE id = 1`).Scan(&mode)
	if err == sql.ErrNoRows {
		return "smallest_distance_first"
	}
	if err != nil {
		return "smallest_distance_first"
	}
	return mode
}

// saveSessionOrder upserts the singleton row.
func saveSessionOrder(cx *galleryCtx, mode string) {
	_, err := cx.DB.Write.Exec(
		`INSERT INTO relation_session (id, order_mode) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET order_mode = excluded.order_mode`,
		mode,
	)
	if err != nil {
		logx.Debugf("save session order: %v", err)
	}
}

// sessionQueueCounts splits the scoped queue the way the page reads
// it: what the walk can serve now, what the rating ceiling holds back,
// and what the operator has skipped. A skipped pair stays queued but
// out of the walk until "Reset skipped" on the hub puts it back, so a
// Skip always moves the sitting forward.
type sessionQueueCounts struct {
	Open            int
	HiddenByCeiling int
	Skipped         int
}

// loadNextPair pulls the next queue row plus both image rows and the
// queue breakdown. The counts come from one lightweight covering scan
// of the queue table. ceiling gates each side of the pair on the
// absence of a rating tag above the cookie level so the session walks
// only what the operator's ceiling already lets them see in the
// gallery.
func loadNextPair(cx *galleryCtx, order, detector string, ceiling *Ceiling, pinA, pinB int64) (*sessionPairView, sessionQueueCounts, error) {
	scope := detectorFilter(detector)
	// The ceiling gate reads the stored pair rank, so the counts and the
	// pick below stay free of per-row image_tags probes.
	where, args := "", []any(nil)
	if rank, active := ceiling.RankCeiling(); active {
		where = "p.max_rating_rank <= ?"
		args = []any{rank}
	}
	openExpr := "p.skipped_at IS NULL"
	if where != "" {
		openExpr += " AND " + where
	}
	var counts sessionQueueCounts
	var unskipped int
	countQ := `
		SELECT COALESCE(SUM(CASE WHEN p.skipped_at IS NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ` + openExpr + ` THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN p.skipped_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM potential_relation_pairs p
		WHERE ` + collectionPairExcl + scope
	if err := cx.DB.Read.QueryRow(countQ, args...).Scan(&unskipped, &counts.Open, &counts.Skipped); err != nil {
		return nil, counts, err
	}
	counts.HiddenByCeiling = unskipped - counts.Open
	pinned := pinA > 0 && pinB > 0
	if counts.Open == 0 && !pinned {
		return nil, counts, nil
	}
	selectBase := `
		SELECT p.a_image_id, p.b_image_id, p.distance, p.source, COALESCE(p.score, 0),
		       ia.canonical_path, COALESCE(ia.width, 0), COALESCE(ia.height, 0), ia.file_size, ia.file_type,
		       ib.canonical_path, COALESCE(ib.width, 0), COALESCE(ib.height, 0), ib.file_size, ib.file_type
		FROM potential_relation_pairs p
		JOIN images ia ON ia.id = p.a_image_id
		JOIN images ib ON ib.id = p.b_image_id`
	orderedQ := selectBase + "\n\t\tWHERE " + collectionPairExcl + scope + " AND p.skipped_at IS NULL"
	if where != "" {
		orderedQ += " AND " + where
	}
	orderedQ += "\n\t\t" + orderClauseForMode(order) + "\n\t\tLIMIT 1"
	query, qargs := orderedQ, args
	if pinned {
		lo, hi := pinA, pinB
		if lo > hi {
			lo, hi = hi, lo
		}
		// A pinned pair is what the operator explicitly asked to see, so
		// neither the detector scope nor the skipped filter applies.
		pinnedQ := selectBase + "\n\t\tWHERE " + collectionPairExcl
		if where != "" {
			pinnedQ += " AND " + where
		}
		pinnedQ += " AND p.a_image_id = ? AND p.b_image_id = ?\n\t\tLIMIT 1"
		query, qargs = pinnedQ, append(append([]any{}, args...), lo, hi)
	}
	var aPath, bPath string
	var aW, aH, bW, bH sql.NullInt64
	view := sessionPairView{Order: order, Remaining: counts.Open}
	scan := func(q string, a ...any) error {
		return cx.DB.Read.QueryRow(q, a...).Scan(
			&view.A.ID, &view.B.ID, &view.Distance, &view.Source, &view.Score,
			&aPath, &aW, &aH, &view.A.FileSize, &view.A.FileType,
			&bPath, &bW, &bH, &view.B.FileSize, &view.B.FileType,
		)
	}
	err := scan(query, qargs...)
	if err == sql.ErrNoRows && pinned {
		// Pinned pair isn't in the visible queue (already resolved, or
		// hidden by the ceiling); fall back to the normal ordered pick.
		err = scan(orderedQ, args...)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, counts, nil
		}
		return nil, counts, err
	}
	view.A.Width = aW
	view.A.Height = aH
	view.B.Width = bW
	view.B.Height = bH
	view.A.Filename = path.Base(aPath)
	view.B.Filename = path.Base(bPath)
	view.A.TagCount = countTags(cx, view.A.ID)
	view.B.TagCount = countTags(cx, view.B.ID)
	// Bigger file first is a duplicate heuristic: the larger file is the
	// likelier original. A tag-sourced pair is usually a variant or a
	// derivative, where the buttons read "right is based on left", so
	// the older image - A, since the queue canonicalises on ascending id
	// and id follows ingest order - belongs on the left instead.
	view.LeftID = view.A.ID
	if view.Source != relations.SourceTags &&
		(view.B.FileSize > view.A.FileSize ||
			(view.B.FileSize == view.A.FileSize && view.B.ID < view.A.ID)) {
		view.LeftID = view.B.ID
	}
	return &view, counts, nil
}

// relationCompareFacts is one side of the under-thumbs comparison
// table. Strings are pre-formatted so the template just renders the
// rows. UniqueTags lists tag names this side carries that the other
// does not; UniqueTagsTotal is the full count (the template caps the
// visible names and shows "+N more").
type relationCompareFacts struct {
	ImageID         int64
	ResolutionW     int64
	ResolutionH     int64
	FileSize        int64
	AddedAt         string
	TagCount        int
	UniqueTags      []compareTag
	UniqueTagsTotal int
	Format          string
	Collection      string
}

// compareTag is one tag name in the comparison table, carrying the
// category it belongs to so the cell renders it in the category's
// colour like every other tag surface.
type compareTag struct {
	Name     string
	Category string
	Color    string
}

// loadCompareFacts loads the comparison table data for two image ids.
// One SELECT per side covers width/height/file_size/ingested_at/
// canonical_path; a second SELECT computes the tag-delta lists. Tag
// counts are loaded through the existing countTags helper.
func loadCompareFacts(cx *galleryCtx, leftID, rightID int64) (relationCompareFacts, relationCompareFacts, error) {
	left := relationCompareFacts{ImageID: leftID}
	right := relationCompareFacts{ImageID: rightID}
	if err := scanCompareFacts(cx, leftID, &left); err != nil {
		return left, right, err
	}
	if err := scanCompareFacts(cx, rightID, &right); err != nil {
		return left, right, err
	}
	leftUnique, rightUnique, err := loadTagDelta(cx, leftID, rightID)
	if err != nil {
		return left, right, err
	}
	const shown = 5
	left.UniqueTagsTotal = len(leftUnique)
	right.UniqueTagsTotal = len(rightUnique)
	left.UniqueTags = leftUnique[:min(shown, len(leftUnique))]
	right.UniqueTags = rightUnique[:min(shown, len(rightUnique))]
	return left, right, nil
}

func scanCompareFacts(cx *galleryCtx, id int64, dst *relationCompareFacts) error {
	var w, h sql.NullInt64
	var addedAt, series sql.NullString
	var canonical, fileType string
	if err := cx.DB.Read.QueryRow(
		`SELECT COALESCE(width, 0), COALESCE(height, 0), file_size, ingested_at, canonical_path, file_type, series
		 FROM images WHERE id = ?`, id,
	).Scan(&w, &h, &dst.FileSize, &addedAt, &canonical, &fileType, &series); err != nil {
		return err
	}
	if w.Valid {
		dst.ResolutionW = w.Int64
	}
	if h.Valid {
		dst.ResolutionH = h.Int64
	}
	if addedAt.Valid {
		dst.AddedAt = humanISODate(addedAt.String)
	}
	if dot := strings.LastIndexByte(canonical, '.'); dot >= 0 {
		dst.Format = strings.ToLower(canonical[dot:])
	}
	if series.Valid {
		dst.Collection = series.String
	}
	dst.TagCount = countTags(cx, id)
	return nil
}

// loadTagDelta returns the names of every tag carried by exactly one
// of the two image ids. The HAVING COUNT(*) = 1 clause splits the join
// into "left only" vs "right only" by re-reading the per-row image_id.
// Rating tags are excluded because the table caller is comparing the
// images, not their ratings.
func loadTagDelta(cx *galleryCtx, leftID, rightID int64) (left []compareTag, right []compareTag, err error) {
	rows, err := cx.DB.Read.Query(`
		WITH delta AS (
			SELECT it.tag_id, MAX(it.image_id) AS owner_id
			FROM image_tags it
			LEFT JOIN tags t ON t.id = it.tag_id
			LEFT JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id IN (?, ?)
			  AND (tc.name IS NULL OR tc.name != 'rating')
			GROUP BY it.tag_id
			HAVING COUNT(*) = 1
		)
		SELECT delta.owner_id, t.name, COALESCE(tc.name, ''), COALESCE(tc.color, '')
		FROM delta
		JOIN tags t ON t.id = delta.tag_id
		LEFT JOIN tag_categories tc ON tc.id = t.category_id
		ORDER BY t.name
		LIMIT 200`, leftID, rightID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var owner int64
		var t compareTag
		var color string
		if scanErr := rows.Scan(&owner, &t.Name, &t.Category, &color); scanErr != nil {
			return nil, nil, scanErr
		}
		t.Color = tags.SafeCategoryColor(color)
		switch owner {
		case leftID:
			left = append(left, t)
		case rightID:
			right = append(right, t)
		}
	}
	return left, right, rows.Err()
}

func countTags(cx *galleryCtx, id int64) int {
	var n int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ?`, id).Scan(&n); err != nil {
		return 0
	}
	return n
}

// sessionDecidePost is the swipe page's decision endpoint. Form:
//   - a, b: image ids (canonical pair order from the queue)
//   - type: duplicate|alternate|version|derivative|not_related|skip
//   - left: image id the operator considers "left" after any W swap.
//     When omitted, "a" is left by default. Every Add* call below
//     receives (left, right) regardless of relation symmetry so the
//     directional semantic stays explicit at the call site.
//
// On success, redirects (HTMX) back to the session page so the next
// pair renders.
func (s *Server) sessionDecidePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	a, ok := formInt64(w, r, "a")
	if !ok {
		return
	}
	b, ok := formInt64(w, r, "b")
	if !ok {
		return
	}
	decision := r.FormValue("type")
	left := a
	if raw := r.FormValue("left"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && (v == a || v == b) {
			left = v
		}
	}
	right := a
	if left == a {
		right = b
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if decision == "skip" {
		if _, err := cx.DB.Write.Exec(
			`UPDATE potential_relation_pairs SET skipped_at = ? WHERE a_image_id = ? AND b_image_id = ?`,
			now, a, b,
		); err != nil {
			logx.Warnf("session skip: %v", err)
			http.Error(w, "skip", http.StatusInternalServerError)
			return
		}
		sessionRedirect(w, r)
		return
	}

	// Every service call takes (left, right): for duplicate the first
	// arg becomes original when a new group forms; for version/
	// derivative the first arg is parent/source; the two symmetric
	// types canonicalise internally so the call shape is uniform.
	var err error
	switch decision {
	case "duplicate":
		err = cx.RelationsSvc.AddDuplicate(left, right)
	case "alternate":
		err = cx.RelationsSvc.AddAlternate(left, right)
	case "version":
		err = cx.RelationsSvc.AddVersionEdge(left, right)
	case "derivative":
		err = cx.RelationsSvc.AddDerivativeEdge(left, right)
	case "not_related":
		err = cx.RelationsSvc.AddNotRelated(left, right)
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown decision.")
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	if _, err := cx.DB.Write.Exec(
		`DELETE FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, a, b,
	); err != nil {
		logx.Warnf("session queue drop: %v", err)
	}
	cx.InvalidateCaches()
	if decision == "duplicate" && writeDuplicatePostDecideHeaders(w, cx, left, right) {
		return
	}
	sessionRedirect(w, r)
}

// writeDuplicatePostDecideHeaders fills the X-Relations-Post-Decision
// header set so the session template can pop a "Delete this duplicate
// from disk?" dialog instead of auto-advancing. Returns true when the
// headers were written (the caller skips the redirect) and false when
// the resolved state doesn't merit a prompt (no group, member missing
// a row, etc.) - the caller then falls through to the usual redirect.
func writeDuplicatePostDecideHeaders(w http.ResponseWriter, cx *galleryCtx, left, right int64) bool {
	// Find the dup group that now contains the pair. Both sides are
	// members; the non-original side is the one the dialog targets.
	var gid, original int64
	if err := cx.DB.Read.QueryRow(`
		SELECT g.id, g.original_image_id
		FROM dup_group_members ma
		JOIN dup_group_members mb ON ma.group_id = mb.group_id
		JOIN dup_groups g ON g.id = ma.group_id
		WHERE ma.image_id = ? AND mb.image_id = ?
		LIMIT 1`, left, right,
	).Scan(&gid, &original); err != nil {
		logx.Debugf("dup post-decide group lookup: %v", err)
		return false
	}
	nonOriginal := left
	if left == original {
		nonOriginal = right
	}
	var canonical string
	if err := cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, nonOriginal).Scan(&canonical); err != nil {
		logx.Debugf("dup post-decide filename: %v", err)
		return false
	}
	var hasUnique int
	if err := cx.DB.Read.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT it.tag_id
			FROM image_tags it
			LEFT JOIN tags t ON t.id = it.tag_id
			LEFT JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id = ?
			  AND (tc.name IS NULL OR tc.name != 'rating')
			  AND NOT EXISTS (
			    SELECT 1 FROM image_tags it2 WHERE it2.image_id = ? AND it2.tag_id = it.tag_id
			  )
			LIMIT 1
		)`, nonOriginal, original,
	).Scan(&hasUnique); err != nil {
		logx.Debugf("dup post-decide unique tags: %v", err)
	}
	w.Header().Set("X-Relations-Post-Decision", "duplicate-cleanup")
	w.Header().Set("X-Relations-Duplicate-ID", strconv.FormatInt(nonOriginal, 10))
	w.Header().Set("X-Relations-Duplicate-OriginalID", strconv.FormatInt(original, 10))
	w.Header().Set("X-Relations-Duplicate-GroupID", strconv.FormatInt(gid, 10))
	w.Header().Set("X-Relations-Duplicate-Filename", path.Base(canonical))
	if hasUnique > 0 {
		w.Header().Set("X-Relations-Duplicate-HasUniqueTags", "1")
	} else {
		w.Header().Set("X-Relations-Duplicate-HasUniqueTags", "0")
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

// sessionRedirect sends the operator back to /relations/session so
// the next pair renders. For HTMX requests, emits an HX-Redirect
// header so the swap reloads the whole page.
func sessionRedirect(w http.ResponseWriter, r *http.Request) {
	dest := "/relations/session?order=" + url.QueryEscape(r.FormValue("order"))
	// The scope is not stored, so it rides the round trip: a decision
	// taken inside one detector's walk must not drop back to Both.
	if d := r.FormValue("detector"); validDetectors[d] {
		dest += "&detector=" + url.QueryEscape(d)
	}
	hxRedirect(w, r, dest)
}
