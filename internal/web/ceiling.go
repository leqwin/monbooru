package web

import (
	"net/http"
	"sync"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tags"
)

// ratingCeilingCookieName is the single point of truth for the cookie
// name. The handler at POST /internal/rating-ceiling writes it; the
// resolver below reads it; nowhere else should reference the literal.
const ratingCeilingCookieName = "monbooru_rating_ceiling"

// Ceiling carries the resolved rating-ceiling state for one request.
// Construct via resolveCeiling so callers share the per-request cache of
// excluded tag ids.
//
// A Ceiling is safe to share across goroutines for one request. The
// sidebar handlers fan out worker goroutines that all consult the same
// *Ceiling; the mutex below guards the lazy cache against the resulting
// concurrent first-access. The hot path (cache hit) reads the cached
// slice directly under a single sync.Mutex acquire.
type Ceiling struct {
	level string
	cx    *galleryCtx

	mu             sync.Mutex
	excludedIDs    []int64
	excludedLoaded bool
}

// duplicatePathsFrom is the FROM clause both file-duplicate surfaces
// count and list against, with the rating ceiling folded in. Shared
// because the two must agree on what a duplicate file is and on hiding
// the same rows: one prints paths the other's counter would deny.
func duplicatePathsFrom(r *http.Request, cx *galleryCtx) (string, []any) {
	from := ` FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0`
	args := []any{}
	if where, wargs := resolveCeiling(r, cx).WhereOne("i.id"); where != "" {
		from += ` WHERE ` + where
		args = append(args, wargs...)
	}
	return from, args
}

// resolveCeiling reads the cookie and returns a Ceiling bound to cx.
// cx may be nil (no active gallery) - the resolver still works for AST
// shapes that don't need the tag-id resolution. ExcludedTagIDs returns
// nil and AnyTainted reports false when cx is nil.
func resolveCeiling(r *http.Request, cx *galleryCtx) *Ceiling {
	return &Ceiling{level: readRatingCookie(r), cx: cx}
}

// readRatingCookie parses the cookie value. Empty string and "explicit"
// both mean "no ceiling"; anything outside the closed enum is dropped to
// "" so a stale or hand-crafted cookie can't inject arbitrary AST values.
func readRatingCookie(r *http.Request) string {
	c, err := r.Cookie(ratingCeilingCookieName)
	if err != nil {
		return ""
	}
	switch c.Value {
	case "general", "sensitive", "questionable", "explicit":
		return c.Value
	}
	return ""
}

// writeRatingCookie sets or clears the cookie. level=explicit (or any
// out-of-enum value) clears it so the empty-storage steady state means
// "no ceiling".
func writeRatingCookie(w http.ResponseWriter, level string) {
	switch level {
	case "general", "sensitive", "questionable":
		http.SetCookie(w, &http.Cookie{
			Name:     ratingCeilingCookieName,
			Value:    level,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   31_536_000,
			SameSite: http.SameSiteLaxMode,
		})
	default:
		http.SetCookie(w, &http.Cookie{
			Name:   ratingCeilingCookieName,
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
	}
}

// Level returns the raw cookie value. Empty string and "explicit" both
// mean "no ceiling"; callers that care about that distinction should use
// IsActive instead.
func (c *Ceiling) Level() string {
	if c == nil {
		return ""
	}
	return c.level
}

// IsActive reports whether a ceiling will actually filter anything. The
// no-cookie state and explicit-cookie state are both inactive: an
// explicit cookie is a no-op in the same way the empty cookie is, since
// the rating vocabulary tops out at explicit.
func (c *Ceiling) IsActive() bool {
	if c == nil {
		return false
	}
	return c.level != "" && c.level != "explicit"
}

// Apply AND-chains a NotExpr per rating level above the ceiling onto
// userExpr. The emitted AST shape is the contract fastCountCeiling
// recognises - keep this function as the sole producer. An empty or
// "explicit" ceiling returns userExpr unchanged.
func (c *Ceiling) Apply(userExpr search.Expr) search.Expr {
	if c == nil || !c.IsActive() {
		return userExpr
	}
	rank := tags.RatingRank(c.level)
	if rank < 0 || rank >= len(tags.RatingLevels)-1 {
		return userExpr
	}
	var ce search.Expr
	for i := rank + 1; i < len(tags.RatingLevels); i++ {
		not := search.NotExpr{Expr: search.FilterExpr{Key: "rating", Val: tags.RatingLevels[i]}}
		if ce == nil {
			ce = not
		} else {
			ce = search.AndExpr{Left: ce, Right: not}
		}
	}
	if userExpr == nil {
		return ce
	}
	return search.AndExpr{Left: userExpr, Right: ce}
}

// ExcludedTagIDs returns the tag ids whose rating rank is strictly above
// the ceiling. Memoised per Ceiling so multiple call sites in one
// request pay the SELECT once. Returns nil when the ceiling is inactive
// or the active gallery isn't available.
func (c *Ceiling) ExcludedTagIDs() []int64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.excludedLoaded {
		return c.excludedIDs
	}
	if !c.IsActive() {
		return nil
	}
	c.excludedLoaded = true
	if c.cx == nil || c.cx.TagSvc == nil {
		return nil
	}
	c.excludedIDs = c.cx.TagSvc.RatingTagIDsAbove(c.level)
	return c.excludedIDs
}

// notExistsRatingPredicate gates col on the absence of any over-ceiling rating
// tag; in is the pre-built placeholder list for the excluded ids.
func notExistsRatingPredicate(col, in string) string {
	return `NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = ` + col + ` AND it.tag_id IN (` + in + `))`
}

// WhereOne returns a NOT EXISTS predicate gating col on the absence of
// any rating tag above the ceiling. Returns ("", nil) when the ceiling
// is inactive so the caller can omit the WHERE entirely and keep the
// covering scan.
func (c *Ceiling) WhereOne(col string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	in, args := db.InPlaceholders(ids)
	return notExistsRatingPredicate(col, in), args
}

// WhereTwo returns a pair of NOT EXISTS predicates ANDed together that
// gate each side of a paired query on the absence of any rating tag
// above the ceiling. Returns ("", nil) when the ceiling is inactive.
func (c *Ceiling) WhereTwo(leftCol, rightCol string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	in, a := db.InPlaceholders(ids)
	args := append(append([]any{}, a...), a...)
	return notExistsRatingPredicate(leftCol, in) + " AND " + notExistsRatingPredicate(rightCol, in), args
}

// WhereGroupClean returns a NOT EXISTS predicate that drops a group
// row when any of its members carries a rating above the ceiling. The
// predicate is meant to be ANDed onto a `... FROM <groups_table> g`
// scan: callers wire `groupCol` to the group-id column (e.g.
// `dup_groups.id`) and `membersTable` to the per-member join table
// (`dup_group_members`).
//
// Returns ("", nil) when the ceiling is inactive so the caller can
// skip the predicate entirely and keep the covering scan.
func (c *Ceiling) WhereGroupClean(membersTable, groupCol string) (string, []any) {
	ids := c.ExcludedTagIDs()
	if len(ids) == 0 {
		return "", nil
	}
	in, args := db.InPlaceholders(ids)
	return `NOT EXISTS (
		SELECT 1 FROM ` + membersTable + ` mr
		JOIN image_tags it ON it.image_id = mr.image_id
		WHERE mr.group_id = ` + groupCol + ` AND it.tag_id IN (` + in + `)
	)`, args
}

// RankCeiling returns the numeric images.rating_rank ceiling for
// queries that read a stored rank column, and whether a ceiling is
// active at all. Rows pass with `rank_col <= rank`; the -1 unrated
// sentinel passes every level.
func (c *Ceiling) RankCeiling() (int, bool) {
	if c == nil || !c.IsActive() {
		return 0, false
	}
	return tags.RatingRank(c.level), true
}

// AnyTainted reports whether any id in ids carries a rating above the
// ceiling. One bounded EXISTS over the stored rating_rank per call -
// relation chains and trees hold a handful of members, so probing the
// ids at hand beats preloading every over-ceiling image in the
// library. An inactive ceiling is a no-op so call sites can treat the
// helper as policy-aware without an extra IsActive guard; a read
// error keeps the row visible rather than failing the page.
func (c *Ceiling) AnyTainted(ids []int64) bool {
	rank, active := c.RankCeiling()
	if !active || len(ids) == 0 || c.cx == nil || c.cx.DB == nil {
		return false
	}
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		in, args := db.InPlaceholders(ids[start:end])
		var tainted int
		err := c.cx.DB.Read.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM images WHERE id IN (`+in+`) AND rating_rank > ?)`,
			append(args, rank)...,
		).Scan(&tainted)
		if err != nil {
			logx.Debugf("ceiling tainted probe: %v", err)
			return false
		}
		if tainted != 0 {
			return true
		}
	}
	return false
}
