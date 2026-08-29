package web

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/relations"
)

// maxPhashDistance is the documented upper bound on the Find-pairs
// Hamming distance.
const maxPhashDistance = 12

// findPairsDistance returns the operator-configured Find-pairs default
// Hamming distance (saturating into the documented 0..maxPhashDistance
// range). Reads through cfgMu so a Settings -> Relations save is
// honoured on the next page render without a restart.
func (s *Server) findPairsDistance() int {
	s.cfgMu.Lock()
	d := s.cfg.Relations.DefaultDistance
	s.cfgMu.Unlock()
	if d < 0 {
		return 0
	}
	if d > maxPhashDistance {
		return maxPhashDistance
	}
	return d
}

// tagPairsEnabled reports whether the tag-similarity detector is on,
// read through cfgMu like findPairsDistance so a settings save lands on
// the next render.
func (s *Server) tagPairsEnabled() bool {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.cfg.Relations.TagPairs
}

// applyRelationsConfig mirrors the operator's [relations] block onto
// the relations package's runtime atomics. Called at boot and after
// every settings edit so a TOML save propagates without restart.
func applyRelationsConfig(rc config.RelationsConfig) {
	d := rc.DefaultDistance
	if d < 0 || d > maxPhashDistance {
		d = 4
	}
	relations.IncrementalProbeDistance.Store(int32(d))
	relations.IncrementalProbeEnabled.Store(rc.IncrementalOnIngest)
}

// settingsRelationsPost reads the form, persists to TOML, then
// re-applies the atomics. IncrementalOnIngest stays true (the
// on-ingest probe is always on). The tag-pairs switch is the
// exception: it is opt-in by design, so off is a state the operator
// keeps until they say otherwise.
func (s *Server) settingsRelationsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	d, err := strconv.Atoi(r.FormValue("default_distance"))
	if err != nil || d < 0 || d > maxPhashDistance {
		flashStatus(w, http.StatusBadRequest, "Distance must be an integer 0..12.")
		return
	}
	order := r.FormValue("default_session_order")
	if !validOrderModes[order] {
		flashStatus(w, http.StatusBadRequest, "Unknown session order.")
		return
	}
	threshold := config.DefaultTagPairThreshold
	// The form speaks percent, matching the ~N% the session cards show;
	// the config keeps the 0..1 fraction the scoring paths use.
	if raw := r.FormValue("tag_pair_threshold"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			flashStatus(w, http.StatusBadRequest, "Match strength must be a number between 70 and 100.")
			return
		}
		threshold = config.ClampTagPairThreshold(v / 100)
	}
	tagPairs := r.FormValue("tag_pairs") == "on"
	s.cfgMu.Lock()
	before := s.cfg.Relations
	s.cfg.Relations.DefaultDistance = d
	s.cfg.Relations.DefaultSessionOrder = order
	s.cfg.Relations.IncrementalOnIngest = true
	s.cfg.Relations.TagPairs = tagPairs
	s.cfg.Relations.TagPairThreshold = threshold
	rc := s.cfg.Relations
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		flashStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	applyRelationsConfig(rc)
	pruned := ""
	if before.DefaultDistance != d || before.TagPairs != tagPairs || before.TagPairThreshold != threshold {
		if n := s.pruneRelationQueues(r.Context(), rc); n > 0 {
			pruned = fmt.Sprintf(" %d stale pair(s) dropped.", n)
		}
	}
	logx.Infof("settings: relations { distance=%d order=%s tag_pairs=%v threshold=%.2f }", d, order, tagPairs, threshold)
	_, _ = fmt.Fprintf(w,
		`<div class="flash flash-ok">Saved. distance=%d order=%s tag pairs=%v (%.0f%%)%s</div>`,
		d, order, tagPairs, threshold*100, pruned)
}

// pruneRelationQueues reconciles every gallery's pair queue with the
// detector settings just saved. The [relations] block is app-wide while
// each gallery owns its queue, so a queue left unpruned would keep
// offering pairs the new settings reject until the operator switched to
// it. Returns the number of rows dropped.
func (s *Server) pruneRelationQueues(ctx context.Context, rc config.RelationsConfig) int {
	ctxs := s.allContexts()
	opts := relations.FindPairsOptions{
		Distance:         rc.DefaultDistance,
		TagPairs:         rc.TagPairs,
		TagPairThreshold: config.ClampTagPairThreshold(rc.TagPairThreshold),
	}
	total := 0
	for _, cx := range ctxs {
		if cx.DB == nil {
			continue
		}
		n, err := relations.PruneQueue(ctx, cx.DB, opts)
		if err != nil {
			logx.Warnf("prune pair queue %q: %v", cx.Name, err)
			continue
		}
		total += n
	}
	return total
}

// relationsCounts is the cheap rollup the Relations page header
// renders. Each query rides a covering index; the whole page header
// builds in well under a millisecond on a 1M-image library. The
// version / derivative counters are "chains" / "trees" rather than
// "edges" so the number matches the card list /relations/browse
// renders: the same `AnyTainted` whole-group filter that drops a card
// also drops a chain / tree from the counter.
type relationsCounts struct {
	PhashMissing int
	QueueOpen    int
	QueueSkipped int
	// QueueBySource splits the open queue by which detector filed each
	// pair. Nil when one detector produced every row - the total
	// already says everything there is to say.
	QueueBySource   []queueSourceCount
	DupGroups       int
	AltGroups       int
	VersionChains   int
	DerivativeTrees int
	NotRelatedPairs int
}

// queueSourceCount is one bucket of the hub's by-detector breakdown.
type queueSourceCount struct {
	Label string
	Count int
}

// queueSourceLabels names each stored source in operator language, in
// the order the hub lists them.
var queueSourceLabels = []struct{ source, label string }{
	{relations.SourcePhash, "image similarity"},
	{relations.SourceTags, "rare tag similarity"},
	{relations.SourceBoth, "both"},
	{relations.SourceReview, "reopened"},
}

// queueCounts runs the grouped scan behind the hub's three queue
// numbers and returns the open / skipped totals plus the open rows'
// non-empty by-detector buckets in label order. A single bucket means
// one detector filed everything, and the breakdown would just restate
// the total, so the split is nil. Errors degrade to zeroes - the page
// still renders the rest.
func queueCounts(cx *galleryCtx, query string, args ...any) (open, skipped int, bySource []queueSourceCount) {
	rows, err := cx.DB.Read.Query(query, args...)
	if err != nil {
		logx.Debugf("relations queue counts: %v", err)
		return 0, 0, nil
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var source string
		var isSkipped bool
		var n int
		if err := rows.Scan(&source, &isSkipped, &n); err != nil {
			return 0, 0, nil
		}
		if isSkipped {
			skipped += n
			continue
		}
		open += n
		counts[source] += n
	}
	if rows.Err() != nil {
		return 0, 0, nil
	}
	if len(counts) < 2 {
		return open, skipped, nil
	}
	bySource = make([]queueSourceCount, 0, len(counts))
	for _, s := range queueSourceLabels {
		if n := counts[s.source]; n > 0 {
			bySource = append(bySource, queueSourceCount{Label: s.label, Count: n})
		}
	}
	return open, skipped, bySource
}

// browseCard is one row of the unified /relations/browse page. Group
// kinds (dup, alt) lift n members in a thumb strip; the version kind
// lifts a chain of N images and the derivative kind lifts a tree of
// N images, each rooted at the chain / tree's earliest ancestor; the
// not_related kind rides a two-image pair plus a comparison table.
// The template branches on .Kind.
type browseCard struct {
	Kind     string  // "duplicate" | "alternate" | "version" | "derivative" | "not_related"
	GroupID  int64   // group id for group kinds; 0 for chain / tree / pair rows
	Members  []int64 // group members in id order; for version chains root-to-leaf order; for derivative trees BFS order; for not_related [a, b]
	Original int64   // dup-group original; 0 for the other kinds
	// CreatedAt is the group / chain / tree / pair declaration date,
	// formatted as "2006-01-02 15:04:05" to match the detail page. For
	// chains and trees this is the newest edge's created_at so the
	// card's "declared" time tracks the last extension.
	CreatedAt string
	// MemberIngestedAt is keyed by Members[i] and carries the same
	// formatted ingest date the detail page shows for each image. The
	// template renders it next to the image id.
	MemberIngestedAt map[int64]string
	// Generations groups Members by depth for the version (chain) kind
	// so the template paints a left-to-right row of thumbs with one
	// image per generation. Empty for kinds without a chain structure.
	Generations [][]int64
	// TreeRows is the DFS-ordered (root first, then each child's
	// subtree before its next sibling) flattening of the derivative
	// tree, each row carrying its depth so the template can indent
	// children under their parent. Empty for kinds without a tree.
	TreeRows []treeRow
}

// treeRow is one entry of a DFS-flattened derivative tree. Depth=0
// marks the root; Trunks carries one segment per indent column from
// the root toward this row, encoding the CSS class the template uses
// to paint the branch lines:
//   - "line"  : ancestor at this depth still has more siblings below
//   - "empty" : ancestor at this depth was the last child (no trunk)
//   - "tee"   : this row is not the last child of its parent (the
//     parent's vertical continues past this row)
//   - "elbow" : this row is the last child of its parent (the vertical
//     stops at the row centre)
//
// The connector (tee / elbow) sits at the last index; earlier indices
// are the ancestor trunks. Root rows carry no trunks.
type treeRow struct {
	ID     int64
	Depth  int
	Trunks []string
	// Source is the image this row hangs under, 0 on the root. The
	// browse card needs it to offer the edge for review; Members alone
	// is a flat DFS list with no parent link.
	Source int64
}

// relationsPage serves /relations: header counters and the per-section
// CTAs. Per-group cards live on /relations/browse.
func (s *Server) relationsPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	ceiling := resolveCeiling(r, cx)
	counts := loadRelationsCounts(cx, ceiling, "")
	s.renderTemplate(w, "relations.html", relationsPageData{
		baseData:      s.base(r, "relations", "Relations - "+s.booruName()),
		Counts:        counts,
		ActiveGallery: s.activeGallery(),
	})
}

type relationsPageData struct {
	baseData
	Counts        relationsCounts
	ActiveGallery string
}

// browseGroupsRedirect 301s the v1.8 /relations/browse-groups URL to
// the unified /relations/browse page. Keeps bookmarks alive without
// dragging the old route into the handler set.
func (s *Server) browseGroupsRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/relations/browse?kind=duplicate"
	switch r.URL.Query().Get("kind") {
	case "alt":
		target = "/relations/browse?kind=alternate"
	case "version":
		target = "/relations/browse?kind=version"
	case "derivative":
		target = "/relations/browse?kind=derivative"
	case "dup", "":
		target = "/relations/browse?kind=duplicate"
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// loadRelationsCounts runs the seven small count queries the header
// renders. Errors during the rollup degrade to a zero count on that
// row - the page still renders the rest. Every counter is ceiling-
// aware so the hub's numbers match what the operator can see and act
// on under their current cookie: group counters drop a group when
// any member is hidden, edge / pair counters drop a row when either
// side is hidden, PhashMissing skips hidden rows. This keeps the
// /relations hub consistent with /relations/browse, whose cards apply
// the same filters.
// skipKind names a relation kind whose count the caller will supply from
// its own card walk, so the counter can skip an in-Go walk of the same
// edge table. Empty counts everything.
func loadRelationsCounts(cx *galleryCtx, ceiling *Ceiling, skipKind string) relationsCounts {
	var c relationsCounts
	get := func(q string, dst *int, args ...any) {
		if err := cx.DB.Read.QueryRow(q, args...).Scan(dst); err != nil {
			logx.Debugf("relations counts %q: %v", q, err)
		}
	}
	if n, err := cx.PhashMissingUnder(ceiling); err == nil {
		c.PhashMissing = n
	}
	// One grouped scan yields the open and skipped totals plus the
	// by-detector split; the collection opt-out is the stored
	// collection_hidden flag, so no counter pays a per-row membership
	// probe. The numbers still match what a session will actually walk.
	queueQ := `SELECT p.source, p.skipped_at IS NOT NULL, COUNT(*)
		FROM potential_relation_pairs p WHERE ` + collectionPairExcl
	var queueArgs []any
	if rank, active := ceiling.RankCeiling(); active {
		queueQ += ` AND p.max_rating_rank <= ?`
		queueArgs = append(queueArgs, rank)
	}
	queueQ += ` GROUP BY p.source, p.skipped_at IS NOT NULL`
	c.QueueOpen, c.QueueSkipped, c.QueueBySource = queueCounts(cx, queueQ, queueArgs...)
	if where, args := ceiling.WhereGroupClean("dup_group_members", "dup_groups.id"); where != "" {
		get(`SELECT COUNT(*) FROM dup_groups WHERE `+where, &c.DupGroups, args...)
	} else {
		get(`SELECT COUNT(*) FROM dup_groups`, &c.DupGroups)
	}
	if where, args := ceiling.WhereGroupClean("alt_group_members", "alt_groups.id"); where != "" {
		get(`SELECT COUNT(*) FROM alt_groups WHERE `+where, &c.AltGroups, args...)
	} else {
		get(`SELECT COUNT(*) FROM alt_groups`, &c.AltGroups)
	}
	if skipKind != "version" {
		if _, total, err := loadVersionChainCards(cx, 0, ceiling); err == nil {
			c.VersionChains = total
		} else {
			logx.Debugf("relations counts version chains: %v", err)
		}
	}
	if skipKind != "derivative" {
		if _, total, err := loadDerivativeTreeCards(cx, 0, ceiling); err == nil {
			c.DerivativeTrees = total
		} else {
			logx.Debugf("relations counts derivative trees: %v", err)
		}
	}
	if where, args := ceiling.WhereTwo("a_image_id", "b_image_id"); where != "" {
		get(`SELECT COUNT(*) FROM not_related_pairs WHERE `+where, &c.NotRelatedPairs, args...)
	} else {
		get(`SELECT COUNT(*) FROM not_related_pairs`, &c.NotRelatedPairs)
	}
	return c
}

// loadBrowseCardsByKind collects up to `limit` cards of one relation
// kind in newest-first order. Group kinds (duplicate, alternate) lift
// the full member list per row; edge kinds (version, derivative) and
// the symmetric pair kind (not_related) emit a two-member slice in
// canonical order. Cards whose members fall above the operator's
// rating ceiling are filtered out via ceiling - an inactive ceiling
// disables the filter. The returned kindTotal is the post-ceiling
// count of chains / trees for version / derivative kinds (regardless
// of limit) so the caller can drive the matching hub counter without
// re-walking the edges; 0 for kinds the count helpers can query
// directly from SQL.
func loadBrowseCardsByKind(cx *galleryCtx, kind, sort string, limit, offset int, ceiling *Ceiling) ([]browseCard, int, error) {
	// groupCardsWhere wraps Ceiling.WhereGroupClean for the two
	// group-card scans: the underlying SQL is "AND <not-exists>" when
	// the predicate is non-empty, plain `1=1` filler otherwise so the
	// LIMIT placeholder ordering stays stable.
	groupCardsWhere := func(membersTable, groupCol string) (string, []any) {
		w, a := ceiling.WhereGroupClean(membersTable, groupCol)
		if w == "" {
			return "", nil
		}
		return " AND " + w, a
	}
	var cards []browseCard
	var walkedTotal int
	switch kind {
	case "duplicate", "alternate":
		g := browseGroupKinds[kind]
		where, args := groupCardsWhere(g.membersTable, g.groupTable+".id")
		rows, err := cx.DB.Read.Query(
			`SELECT `+g.cols+` FROM `+g.groupTable+` WHERE 1=1`+where+` `+g.sortClause(sort)+` LIMIT ? OFFSET ?`,
			append(args, limit, offset)...,
		)
		if err != nil {
			return nil, 0, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, original int64
			var createdAt string
			if err := rows.Scan(&id, &original, &createdAt); err != nil {
				return nil, 0, err
			}
			members, mErr := scanGroupMembers(cx, g.membersTable, id)
			if mErr != nil {
				return nil, 0, mErr
			}
			cards = append(cards, browseCard{Kind: kind, GroupID: id, Members: members, Original: original, CreatedAt: humanISOTime(createdAt)})
		}
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
	case "version":
		// loadVersionChainCards always returns the full sorted set
		// and its post-ceiling total. The annotate-then-sort order
		// matters when sort=newest_member - that pass reads each
		// member's ingested_at, populated by annotateBrowseCardIngestedAt.
		chains, total, cErr := loadVersionChainCards(cx, 0, ceiling)
		if cErr != nil {
			return nil, 0, cErr
		}
		if sort == "newest_member" {
			if aErr := annotateBrowseCardIngestedAt(cx, chains); aErr != nil {
				logx.Warnf("browse cards ingest-dates version: %v", aErr)
			}
		}
		sortVersionCards(chains, sort)
		cards = append(cards, sliceWindow(chains, offset, limit)...)
		walkedTotal = total
	case "derivative":
		trees, total, tErr := loadDerivativeTreeCards(cx, 0, ceiling)
		if tErr != nil {
			return nil, 0, tErr
		}
		sortDerivativeCards(trees, sort)
		cards = append(cards, sliceWindow(trees, offset, limit)...)
		walkedTotal = total
	case "not_related":
		where, args := ceiling.WhereTwo("a_image_id", "b_image_id")
		q := `SELECT a_image_id, b_image_id, created_at FROM not_related_pairs`
		if where != "" {
			q += ` WHERE ` + where
		}
		q += ` ORDER BY rowid DESC LIMIT ? OFFSET ?`
		nrRows, err := cx.DB.Read.Query(q, append(args, limit, offset)...)
		if err != nil {
			return nil, 0, err
		}
		defer func() { _ = nrRows.Close() }()
		for nrRows.Next() {
			var a, b int64
			var createdAt string
			if err := nrRows.Scan(&a, &b, &createdAt); err != nil {
				return nil, 0, err
			}
			cards = append(cards, browseCard{Kind: "not_related", Members: []int64{a, b}, CreatedAt: humanISOTime(createdAt)})
		}
		if err := nrRows.Err(); err != nil {
			return nil, 0, err
		}
	default:
		return nil, 0, nil
	}
	if err := annotateBrowseCardIngestedAt(cx, cards); err != nil {
		// Log but keep rendering: dates are nice-to-have, the rest of
		// the card carries the operator's primary signal.
		logx.Warnf("browse cards ingest-dates %s: %v", kind, err)
	}
	return cards, walkedTotal, nil
}

// dupSortClause maps the per-kind whitelist value to a static
// ORDER BY tail. The sort value is already resolved through
// resolveBrowseSort so it's safe to splice directly.
// browseGroupKinds describes the two group-card kinds, which differ only in
// their tables, their sort vocabulary and whether the group names an original.
// The alternate select pads a zero so both scan the same three columns.
var browseGroupKinds = map[string]struct {
	groupTable   string
	membersTable string
	cols         string
	sortClause   func(string) string
}{
	"duplicate": {"dup_groups", "dup_group_members", "dup_groups.id, dup_groups.original_image_id, dup_groups.created_at", dupSortClause},
	"alternate": {"alt_groups", "alt_group_members", "alt_groups.id, 0, alt_groups.created_at", altSortClause},
}

func dupSortClause(sort string) string {
	switch sort {
	case "size":
		return `ORDER BY (SELECT COUNT(*) FROM dup_group_members WHERE group_id = dup_groups.id) DESC, dup_groups.id DESC`
	case "original_added":
		return `ORDER BY (SELECT ingested_at FROM images WHERE id = dup_groups.original_image_id) DESC, dup_groups.id DESC`
	}
	return `ORDER BY dup_groups.id DESC`
}

func altSortClause(sort string) string {
	if sort == "size" {
		return `ORDER BY (SELECT COUNT(*) FROM alt_group_members WHERE group_id = alt_groups.id) DESC, alt_groups.id DESC`
	}
	return `ORDER BY alt_groups.id DESC`
}

func sortVersionCards(cards []browseCard, sortKey string) {
	switch sortKey {
	case "length":
		sort.SliceStable(cards, func(i, j int) bool {
			return len(cards[i].Members) > len(cards[j].Members)
		})
	case "newest_member":
		newest := func(c browseCard) string {
			var best string
			for _, m := range c.Members {
				if d, ok := c.MemberIngestedAt[m]; ok && d > best {
					best = d
				}
			}
			return best
		}
		sort.SliceStable(cards, func(i, j int) bool {
			return newest(cards[i]) > newest(cards[j])
		})
	}
	// "recent" is the loader's default order; nothing to do.
}

func sortDerivativeCards(cards []browseCard, sortKey string) {
	if sortKey == "size" {
		sort.SliceStable(cards, func(i, j int) bool {
			return len(cards[i].Members) > len(cards[j].Members)
		})
	}
}

func sliceWindow(cards []browseCard, offset, limit int) []browseCard {
	if offset >= len(cards) {
		return nil
	}
	end := offset + limit
	if limit <= 0 || end > len(cards) {
		end = len(cards)
	}
	return cards[offset:end]
}

// annotateBrowseCardIngestedAt populates the per-card MemberIngestedAt
// map with each member's images.ingested_at, formatted the same way
// the detail page formats its dates. One bulk SELECT keeps the load
// O(1) regardless of card / member count.
func annotateBrowseCardIngestedAt(cx *galleryCtx, cards []browseCard) error {
	if len(cards) == 0 {
		return nil
	}
	idSet := map[int64]struct{}{}
	for _, c := range cards {
		for _, m := range c.Members {
			idSet[m] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	placeholders, args := db.InPlaceholders(slices.Collect(maps.Keys(idSet)))
	rows, err := cx.DB.Read.Query(
		`SELECT id, ingested_at FROM images WHERE id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	dates := map[int64]string{}
	for rows.Next() {
		var id int64
		var ingested string
		if scanErr := rows.Scan(&id, &ingested); scanErr != nil {
			return scanErr
		}
		dates[id] = humanISOTime(ingested)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range cards {
		out := make(map[int64]string, len(cards[i].Members))
		for _, m := range cards[i].Members {
			if d, ok := dates[m]; ok {
				out[m] = d
			}
		}
		cards[i].MemberIngestedAt = out
	}
	return nil
}

// humanISOTime turns an ISO-8601 timestamp stored as TEXT in SQLite
// ("2026-05-17T08:00:00Z") into the matter-of-fact "2026-05-17 08:00:00"
// the rest of the app uses. Returns the input unchanged on a parse
// failure so a freshly added column never blanks a card body.
func humanISOTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.In(time.Local).Format("2006-01-02 15:04:05")
}

// humanISODate is humanISOTime's date-only sibling: the table-cell
// formatter on the comparison view drops the time so the cell stays
// short. Falls back to the substring split on a parse failure so the
// stored timestamp still renders something sensible.
func humanISODate(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if i := strings.IndexByte(s, 'T'); i > 0 {
			return s[:i]
		}
		return s
	}
	return t.In(time.Local).Format("2006-01-02")
}

// sortedRootsDesc returns the keys of rootSet sorted by id descending,
// so the newest root sorts first.
func sortedRootsDesc(rootSet map[int64]bool) []int64 {
	roots := slices.Sorted(maps.Keys(rootSet))
	slices.Reverse(roots)
	return roots
}

// finalizeCards orders cards by CreatedAt descending (newest first) and
// trims to limit when positive, returning the trimmed slice and the
// pre-trim total.
func finalizeCards(cards []browseCard, limit int) ([]browseCard, int) {
	total := len(cards)
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].CreatedAt > cards[j].CreatedAt
	})
	if limit > 0 && len(cards) > limit {
		cards = cards[:limit]
	}
	return cards, total
}

// loadVersionChainCards reads every version_edge into memory, walks
// each chain from its root (a parent with no incoming edge) down to
// its leaf, and emits one card per chain. The flat Members slice is
// root-to-leaf so per-member ingest dates key into it; Generations is
// the same image ids regrouped one-per-depth so the template can
// render each generation as a row separated by a down-arrow. Chains
// whose member set carries any tag above the ceiling are dropped
// whole so a ceiling-hidden image never surfaces a sibling. The
// returned total counts every surviving chain regardless of limit so
// the caller can drive the matching counter without re-walking the
// edges.
func loadVersionChainCards(cx *galleryCtx, limit int, ceiling *Ceiling) ([]browseCard, int, error) {
	rows, err := cx.DB.Read.Query(`SELECT child_image_id, parent_image_id, created_at FROM version_edges`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	type edgeMeta struct {
		parent    int64
		createdAt string
	}
	edges := map[int64]edgeMeta{} // child -> {parent, createdAt}
	childOf := map[int64]int64{}  // parent -> child (UNIQUE per schema)
	for rows.Next() {
		var c, p int64
		var ts string
		if scanErr := rows.Scan(&c, &p, &ts); scanErr != nil {
			return nil, 0, scanErr
		}
		edges[c] = edgeMeta{parent: p, createdAt: ts}
		childOf[p] = c
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rootSet := map[int64]bool{}
	for _, em := range edges {
		if _, hasParent := edges[em.parent]; !hasParent {
			rootSet[em.parent] = true
		}
	}
	roots := sortedRootsDesc(rootSet)
	cards := make([]browseCard, 0, len(roots))
	for _, root := range roots {
		members := []int64{root}
		generations := [][]int64{{root}}
		latestTS := ""
		cur := root
		for {
			next, ok := childOf[cur]
			if !ok {
				break
			}
			members = append(members, next)
			generations = append(generations, []int64{next})
			if em, ok := edges[next]; ok && em.createdAt > latestTS {
				latestTS = em.createdAt
			}
			cur = next
		}
		if ceiling.AnyTainted(members) {
			continue
		}
		cards = append(cards, browseCard{
			Kind:        "version",
			Members:     members,
			Generations: generations,
			CreatedAt:   humanISOTime(latestTS),
		})
	}
	// Order by the newest edge in each chain so freshly declared chains
	// land at the top.
	cards, total := finalizeCards(cards, limit)
	return cards, total, nil
}

// loadDerivativeTreeCards is the derivative-edge analogue of
// loadVersionChainCards. Each tree roots at a source that isn't itself
// a derivative; a depth-first walk from the root produces TreeRows
// (root first, then each subtree before its next sibling) so the
// template can indent each row by its depth and the branching is
// visible at a glance. Members keeps the same DFS order so per-member
// metadata maps key against it. Returns the total tree count (every
// surviving tree, regardless of limit) so the caller can drive the
// matching counter without re-walking the edges.
func loadDerivativeTreeCards(cx *galleryCtx, limit int, ceiling *Ceiling) ([]browseCard, int, error) {
	rows, err := cx.DB.Read.Query(`SELECT derivative_image_id, source_image_id, created_at FROM derivative_edges`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	derivativesOf := map[int64][]int64{} // source -> derivatives (sorted by id ASC)
	sourceOf := map[int64]int64{}        // derivative -> source
	derivCreated := map[int64]string{}   // derivative -> edge's created_at
	for rows.Next() {
		var d, src int64
		var ts string
		if scanErr := rows.Scan(&d, &src, &ts); scanErr != nil {
			return nil, 0, scanErr
		}
		derivativesOf[src] = append(derivativesOf[src], d)
		sourceOf[d] = src
		derivCreated[d] = ts
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for src := range derivativesOf {
		sort.Slice(derivativesOf[src], func(i, j int) bool {
			return derivativesOf[src][i] < derivativesOf[src][j]
		})
	}
	rootSet := map[int64]bool{}
	for src := range derivativesOf {
		if _, isDeriv := sourceOf[src]; !isDeriv {
			rootSet[src] = true
		}
	}
	roots := sortedRootsDesc(rootSet)
	cards := make([]browseCard, 0, len(roots))
	for _, root := range roots {
		var members []int64
		var treeRows []treeRow
		latestTS := ""
		dfsDerivativeTree(root, 0, 0, nil, true, derivativesOf, derivCreated, &members, &treeRows, &latestTS)
		if ceiling.AnyTainted(members) {
			continue
		}
		cards = append(cards, browseCard{
			Kind:      "derivative",
			Members:   members,
			TreeRows:  treeRows,
			CreatedAt: humanISOTime(latestTS),
		})
	}
	cards, total := finalizeCards(cards, limit)
	return cards, total, nil
}

// dfsDerivativeTree appends each tree node and its subtree to members
// and rows in depth-first order. Sibling order matches the caller's
// pre-sorted derivativesOf slice (ascending id) so the visual layout
// is stable across renders. ancestorTrunks is the prefix common to
// every descendant of this node (one entry per ancestor depth);
// isLast says whether this node is the last child of its parent so
// the connector is drawn as an elbow when true and a tee when false.
func dfsDerivativeTree(node, source int64, depth int, ancestorTrunks []string, isLast bool, derivativesOf map[int64][]int64, derivCreated map[int64]string, members *[]int64, rows *[]treeRow, latestTS *string) {
	*members = append(*members, node)
	*rows = append(*rows, treeRow{ID: node, Depth: depth, Trunks: rowTrunks(ancestorTrunks, depth, isLast), Source: source})
	if ts := derivCreated[node]; ts > *latestTS {
		*latestTS = ts
	}
	childAncestors := extendAncestorTrunks(ancestorTrunks, depth, isLast)
	children := derivativesOf[node]
	for i, child := range children {
		childIsLast := i == len(children)-1
		dfsDerivativeTree(child, node, depth+1, childAncestors, childIsLast, derivativesOf, derivCreated, members, rows, latestTS)
	}
}

// rowTrunks builds the per-row trunks slice the template renders. The
// root (depth 0) carries no trunks; every other row gets one entry per
// ancestor depth plus the tee / elbow connector.
func rowTrunks(ancestorTrunks []string, depth int, isLast bool) []string {
	return trunkSlice(ancestorTrunks, depth, isLast, "elbow", "tee")
}

// extendAncestorTrunks computes the ancestor-trunk slice each child of
// the current node sees: the existing ancestors plus a new segment at
// the current depth. The new segment is "line" when this node has more
// siblings below (its column continues past its children) and "empty"
// when this node is the last child (the column ends).
func extendAncestorTrunks(ancestorTrunks []string, depth int, isLast bool) []string {
	return trunkSlice(ancestorTrunks, depth, isLast, "empty", "line")
}

// trunkSlice appends one segment to the ancestor trunks: last when this
// node is its parent's final child, more otherwise. Depth 0 is the root,
// which carries no trunks at all.
func trunkSlice(ancestorTrunks []string, depth int, isLast bool, last, more string) []string {
	if depth == 0 {
		return nil
	}
	out := make([]string, 0, depth)
	out = append(out, ancestorTrunks...)
	if isLast {
		return append(out, last)
	}
	return append(out, more)
}

// scanGroupMembers returns every image_id belonging to the named
// group in id order. Reused across dup_group_members and
// alt_group_members.
func scanGroupMembers(cx *galleryCtx, table string, groupID int64) ([]int64, error) {
	return db.QueryIDs(cx.DB.Read, `SELECT image_id FROM `+table+` WHERE group_id = ? ORDER BY image_id`, groupID)
}

// validBrowseKinds is the closed vocabulary the /relations/browse page
// accepts as the ?kind= query parameter.
var validBrowseKinds = map[string]bool{
	"duplicate":   true,
	"alternate":   true,
	"version":     true,
	"derivative":  true,
	"not_related": true,
}

// browseSortsByKind whitelists the ?sort= values each kind tab accepts.
// The first entry in every slice is the default; anything off the list
// silently collapses to that default so a typo never executes against
// an interpolated SQL fragment.
var browseSortsByKind = map[string][]string{
	"duplicate":   {"recent", "size", "original_added"},
	"alternate":   {"recent", "size"},
	"version":     {"recent", "length", "newest_member"},
	"derivative":  {"recent", "size"},
	"not_related": {"recent"},
}

func resolveBrowseSort(kind, requested string) string {
	allowed := browseSortsByKind[kind]
	if len(allowed) == 0 {
		return "recent"
	}
	for _, s := range allowed {
		if s == requested {
			return s
		}
	}
	return allowed[0]
}

// browseRelationsPageSize caps each /relations/browse page; matches
// /tags's 100-row cap shape but tuned smaller because each card lifts
// a thumb strip whose vertical footprint is far denser.
const browseRelationsPageSize = 60

// browseRelationsPage renders /relations/browse with one tab per
// relation kind. The card layout adapts per kind: group cards lift a
// thumb strip plus dissolve / merge controls; edge cards render two
// thumbs with a directional arrow plus reverse / unlink; not-related
// rows render two thumbs plus unlink.
func (s *Server) browseRelationsPage(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	kind = cmp.Or(kind, "duplicate")
	if !validBrowseKinds[kind] {
		http.NotFound(w, r)
		return
	}
	page := 1
	requested := 0
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
		requested, page = p, p
	}
	if page < 1 {
		page = 1
	}
	sort := resolveBrowseSort(kind, r.URL.Query().Get("sort"))
	ceiling := resolveCeiling(r, cx)
	// The version and derivative loaders walk their whole edge table in
	// Go and hand back its post-ceiling total, so the counter skips the
	// active kind and takes the number from the card walk instead of
	// repeating it. That means the cards load before the page clamp; an
	// out-of-range page only pays a second slice on the htmx path, since
	// a full request redirects out of here first.
	counts := loadRelationsCounts(cx, ceiling, kind)
	offset := (page - 1) * browseRelationsPageSize
	cards, walkedTotal, err := loadBrowseCardsByKind(cx, kind, sort, browseRelationsPageSize, offset, ceiling)
	if err != nil {
		logx.Warnf("browse cards %s: %v", kind, err)
		http.Error(w, "load cards", http.StatusInternalServerError)
		return
	}
	switch kind {
	case "version":
		counts.VersionChains = walkedTotal
	case "derivative":
		counts.DerivativeTrees = walkedTotal
	}
	total := kindTotal(counts, kind)
	totalPages := 1
	if total > 0 {
		totalPages = (total + browseRelationsPageSize - 1) / browseRelationsPageSize
	}
	if page > totalPages {
		page = totalPages
	}
	// Sync the address bar when an out-of-range ?page= was clamped, mirroring
	// the gallery; otherwise a bookmarked deep page sticks in the URL.
	if requested != 0 && requested != page {
		clampedQ := r.URL.Query()
		clampedQ.Set("page", strconv.Itoa(page))
		clampedURL := "/relations/browse?" + clampedQ.Encode()
		if isHTMXRequest(r) {
			w.Header().Set("HX-Push-Url", clampedURL)
		} else {
			http.Redirect(w, r, clampedURL, http.StatusSeeOther)
			return
		}
	}
	if clamped := (page - 1) * browseRelationsPageSize; clamped != offset {
		cards, _, err = loadBrowseCardsByKind(cx, kind, sort, browseRelationsPageSize, clamped, ceiling)
		if err != nil {
			logx.Warnf("browse cards %s (clamped): %v", kind, err)
			http.Error(w, "load cards", http.StatusInternalServerError)
			return
		}
	}
	s.renderTemplate(w, "relations_browse.html", browseRelationsData{
		baseData:      s.base(r, "relations", "Browse relations - "+s.booruName()),
		ActiveGallery: s.activeGallery(),
		Kind:          kind,
		Cards:         cards,
		Counts:        counts,
		Page:          page,
		TotalPages:    totalPages,
		Sort:          sort,
		SortOptions:   browseSortsByKind[kind],
	})
}

// kindTotal returns the per-kind count from a relationsCounts bundle.
func kindTotal(c relationsCounts, kind string) int {
	switch kind {
	case "duplicate":
		return c.DupGroups
	case "alternate":
		return c.AltGroups
	case "version":
		return c.VersionChains
	case "derivative":
		return c.DerivativeTrees
	case "not_related":
		return c.NotRelatedPairs
	}
	return 0
}

type browseRelationsData struct {
	baseData
	ActiveGallery string
	Kind          string
	Cards         []browseCard
	Counts        relationsCounts
	Page          int
	TotalPages    int
	Sort          string
	SortOptions   []string
}
