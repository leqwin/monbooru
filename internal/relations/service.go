// Package relations manages the operator-declared graph between images:
// duplicate groups, alternate groups, directed version chains, directed
// derivative trees, and the "not related" rejection set.
//
// The Service is the only thing the rest of the codebase calls to mutate
// the graph; each mutation runs inside a single transaction so partial
// states never surface.
package relations

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/monbooru/monbooru/internal/db"
)

var (
	// ErrSelfRelation is returned when both ends of a relation point at
	// the same image.
	ErrSelfRelation = errors.New("relations: pair refers to a single image")

	// ErrRelationConflict is returned when a pair already carries a
	// relation of a different type. The operator must explicitly
	// remove the existing relation before declaring a new one.
	ErrRelationConflict = errors.New("relations: pair already has a different relation")

	// ErrIndirectRelation is returned when a pair sits on one
	// root-to-leaf path of a version chain or derivative tree without a
	// direct edge between the two: there is no edge to overwrite, so the
	// operator has to unlink the path instead.
	ErrIndirectRelation = errors.New("relations: pair is already related through another image")

	// ErrVersionExists is returned when adding a version edge would
	// give a child a second parent or a parent a second child. Strict
	// chains, not trees.
	ErrVersionExists = errors.New("relations: version edge already exists on one side")

	// ErrDerivativeExists is returned when adding a derivative edge
	// would assign a derivative a second source.
	ErrDerivativeExists = errors.New("relations: derivative already has a source")

	// ErrChainTooDeep is returned when adding a version or derivative
	// edge would make the chain / tree deeper than MaxVersionChainDepth,
	// the horizon every capped walker (dissolve, render) trusts.
	ErrChainTooDeep = errors.New("relations: chain would exceed the depth limit")

	// ErrNotInGroup is returned by PromoteToOriginal when the named
	// image isn't currently a member of the target dup group.
	ErrNotInGroup = errors.New("relations: image is not a member of the group")
)

// FriendlyError carries an operator-facing message and the HTTP status
// code a transport-layer error writer should surface for one of the
// Service's typed errors.
type FriendlyError struct {
	Status  int    // HTTP status the caller should write (400, 409, ...)
	Code    string // short identifier for JSON error envelopes
	Message string // the line the operator sees
}

// FriendlyErrorFor maps a Service error to the operator-facing message
// shared by every transport. Returns nil when err is not one of the
// recognised sentinels so the caller can fall back to a generic 500.
func FriendlyErrorFor(err error) *FriendlyError {
	switch {
	case errors.Is(err, ErrSelfRelation):
		return &FriendlyError{Status: 400, Code: "invalid_request", Message: "Cannot relate an image to itself."}
	case errors.Is(err, ErrRelationConflict):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "Pair already has a different relation; remove the existing one first."}
	case errors.Is(err, ErrIndirectRelation):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "These images are already related through another image."}
	case errors.Is(err, ErrVersionExists):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "One of the images already has a version edge; remove it first."}
	case errors.Is(err, ErrDerivativeExists):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "The chosen derivative already has a source; remove it first."}
	case errors.Is(err, ErrChainTooDeep):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "The chain is already at its maximum depth."}
	case errors.Is(err, ErrNotInGroup):
		return &FriendlyError{Status: 400, Code: "invalid_request", Message: "Image isn't a member of that group."}
	}
	return nil
}

// Service is the transactional boundary for relations mutations.
type Service struct {
	db *db.DB
}

// New returns a Service backed by the provided database.
func New(database *db.DB) *Service {
	return &Service{db: database}
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// canonicalPair returns (min, max) so symmetric relations
// (not_related, in particular) live as a single canonical row
// regardless of caller argument order.
func canonicalPair(a, b int64) (int64, int64) {
	if a < b {
		return a, b
	}
	return b, a
}

func (s *Service) inWriteTx(work func(*sql.Tx) error) error {
	return db.InWriteTx(s.db.Write, work)
}

// addGroupRelation enrols a and b in a group of the given kind in a
// single transaction. The other kind's group state is left untouched:
// §9.2 holds a pair to at most one relation type, so making a and b
// duplicates must not also enrol them as alternates of each other
// (which folding their alt groups together would do).
func (s *Service) addGroupRelation(a, b int64, label string, cfg groupMerge) error {
	if a == b {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if err := pairConflictTx(tx, a, b, label); err != nil {
			return err
		}
		if err := mergeIntoGroupTx(tx, a, b, cfg); err != nil {
			return err
		}
		return pruneQueueForGroupTx(tx, cfg.membersTbl, a)
	})
}

// AddDuplicate marks images a and b as duplicates.
func (s *Service) AddDuplicate(a, b int64) error {
	return s.addGroupRelation(a, b, "duplicate", dupGroupMerge)
}

// AddAlternate marks images a and b as alternates.
func (s *Service) AddAlternate(a, b int64) error {
	return s.addGroupRelation(a, b, "alternate", altGroupMerge)
}

// MaxVersionChainDepth is the maximum edge depth of a version chain or
// derivative tree, enforced by Add*Edge, and the walk budget of every
// depth-capped traversal here and in the web renderers. Enforcing it at
// add time is what lets those walkers trust that a capped walk covers
// the whole component. Mirrors the implications walker's depth budget.
const MaxVersionChainDepth = 16

// ChainPath walks table upward from start, reading selectCol via whereCol,
// and returns the nodes above it nearest-first (empty when start has none).
// Both edge tables make the read side a primary key, so every step is a
// point seek onto at most one row. Depth-capped so a malformed cycle in the
// data can't spin; q is a transaction or the read pool.
func ChainPath(q db.Querier, table, selectCol, whereCol string, start int64) ([]int64, error) {
	var path []int64
	cur := start
	for i := 0; i < MaxVersionChainDepth; i++ {
		next, err := db.QueryIDs(q, `SELECT `+selectCol+` FROM `+table+` WHERE `+whereCol+` = ?`, cur)
		if err != nil {
			return nil, err
		}
		if len(next) == 0 {
			return path, nil
		}
		path = append(path, next[0])
		cur = next[0]
	}
	return path, nil
}

// chainRoot is the top of the chain above start, or start itself when it
// has none.
func chainRoot(q db.Querier, table, parentCol, childCol string, start int64) (int64, error) {
	path, err := ChainPath(q, table, parentCol, childCol, start)
	if err != nil {
		return 0, err
	}
	if len(path) == 0 {
		return start, nil
	}
	return path[len(path)-1], nil
}

// chainReachesTx reports whether target sits anywhere above start.
func chainReachesTx(tx *sql.Tx, table, parentCol, childCol string, start, target int64) (bool, error) {
	path, err := ChainPath(tx, table, parentCol, childCol, start)
	if err != nil {
		return false, err
	}
	return slices.Contains(path, target), nil
}

// chainRelatesTx reports whether a and b sit on one root-to-leaf path of
// the table's edges, any number of steps apart. The pair carries no
// orientation, so both directions are walked.
func chainRelatesTx(tx *sql.Tx, table, parentCol, childCol string, a, b int64) (bool, error) {
	if up, err := chainReachesTx(tx, table, parentCol, childCol, a, b); err != nil || up {
		return up, err
	}
	return chainReachesTx(tx, table, parentCol, childCol, b, a)
}

// chainDepthTx counts the edges in table on one side of start: selecting
// parentCol via childCol counts ancestors, the reverse counts
// descendants. Capped at MaxVersionChainDepth like the other walks; the
// callers only need to know whether the joined chain would exceed it.
func chainDepthTx(tx *sql.Tx, table, selectCol, whereCol string, start int64) (int, error) {
	path, err := ChainPath(tx, table, selectCol, whereCol, start)
	if err != nil {
		return 0, err
	}
	return len(path), nil
}

// derivativeHeightTx returns the number of edge levels beneath start in
// the derivative tree, capped at MaxVersionChainDepth like the chain
// walks.
func derivativeHeightTx(tx *sql.Tx, start int64) (int, error) {
	_, levels, err := chainSpanTx(tx, "derivative_edges", "derivative_image_id", "source_image_id", start)
	return levels, err
}

// chainSpanTx collects start plus everything reachable from it through
// selectCol/whereCol: the single-parent chain when the columns read
// upward, the whole subtree when they read downward. Level-capped like
// the other walks.
func chainSpanTx(tx *sql.Tx, table, selectCol, whereCol string, start int64) ([]int64, int, error) {
	span := []int64{start}
	frontier := []int64{start}
	levels := 0
	for level := 0; level < MaxVersionChainDepth && len(frontier) > 0; level++ {
		var next []int64
		for _, id := range frontier {
			ids, err := db.QueryIDs(tx, `SELECT `+selectCol+` FROM `+table+` WHERE `+whereCol+` = ?`, id)
			if err != nil {
				return nil, 0, err
			}
			next = append(next, ids...)
		}
		if len(next) == 0 {
			break
		}
		levels++
		span = append(span, next...)
		frontier = next
	}
	return span, levels, nil
}

// walkToRootTx follows the single-parent chain in table upward from start
// and returns the root - start itself when it has no parent.
func walkToRootTx(tx *sql.Tx, table, parentCol, childCol string, start int64) (int64, error) {
	return chainRoot(tx, table, parentCol, childCol, start)
}

// AddVersionEdge declares child as the newer version of parent. The
// chain is strict: each image has at most one parent (PK on
// child_image_id) and at most one child (UNIQUE on parent_image_id), so
// the only forbidden configurations are (a) child already has a parent,
// (b) parent already has a child, or (c) adding the edge would close a
// loop with an existing ancestor chain.
func (s *Service) AddVersionEdge(parent, child int64) error {
	if parent == child {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if err := pairConflictTx(tx, parent, child, "version"); err != nil {
			return err
		}
		// Idempotent re-add: the same (parent, child) already on the
		// chain is a silent success so REST retries against a flaky
		// network don't have to distinguish "first call landed but
		// the response was lost" from a real cycle / direction
		// conflict. Still prunes: a queue row can predate the edge.
		var exact int
		if err := tx.QueryRow(
			`SELECT 1 FROM version_edges WHERE child_image_id = ? AND parent_image_id = ?`,
			child, parent,
		).Scan(&exact); err == nil {
			return pruneQueueForChainTx(tx, "version_edges", "parent_image_id", "child_image_id", parent, child)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM version_edges WHERE child_image_id = ? OR parent_image_id = ?`,
			child, parent,
		).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrVersionExists
		}
		// Walk parent's ancestors; if child is anywhere up that chain, the
		// new edge would close a cycle.
		if reaches, err := chainReachesTx(tx, "version_edges", "parent_image_id", "child_image_id", parent, child); err != nil {
			return err
		} else if reaches {
			return ErrVersionExists
		}
		// The new edge joins parent's chain above and child's chain below;
		// the combined depth must stay within the walkers' horizon.
		up, err := chainDepthTx(tx, "version_edges", "parent_image_id", "child_image_id", parent)
		if err != nil {
			return err
		}
		down, err := chainDepthTx(tx, "version_edges", "child_image_id", "parent_image_id", child)
		if err != nil {
			return err
		}
		if up+down+1 > MaxVersionChainDepth {
			return ErrChainTooDeep
		}
		if _, err := tx.Exec(
			`INSERT INTO version_edges (child_image_id, parent_image_id, created_at) VALUES (?, ?, ?)`,
			child, parent, nowISO(),
		); err != nil {
			return err
		}
		return pruneQueueForChainTx(tx, "version_edges", "parent_image_id", "child_image_id", parent, child)
	})
}

// AddDerivativeEdge declares derivative was made from source. A source
// can carry many derivatives (tree); each derivative has exactly one
// source. Refuses when the derivative already has a source or when
// adding the edge would close a cycle with an existing source chain.
func (s *Service) AddDerivativeEdge(source, derivative int64) error {
	if source == derivative {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if err := pairConflictTx(tx, source, derivative, "derivative"); err != nil {
			return err
		}
		// Idempotent re-add: the same (source, derivative) already
		// declared returns silent success so retries don't have to
		// distinguish a same-edge replay from a real source-conflict.
		// Still prunes: a queue row can predate the edge.
		var exact int
		if err := tx.QueryRow(
			`SELECT 1 FROM derivative_edges WHERE derivative_image_id = ? AND source_image_id = ?`,
			derivative, source,
		).Scan(&exact); err == nil {
			return pruneQueueForChainTx(tx, "derivative_edges", "source_image_id", "derivative_image_id", source, derivative)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM derivative_edges WHERE derivative_image_id = ?`, derivative,
		).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrDerivativeExists
		}
		// Walk source's source-chain; if derivative is anywhere up that
		// chain, the new edge would close a cycle. Same depth budget as
		// the version chain so a pathological tree can't loop indefinitely.
		if reaches, err := chainReachesTx(tx, "derivative_edges", "source_image_id", "derivative_image_id", source, derivative); err != nil {
			return err
		} else if reaches {
			return ErrDerivativeExists
		}
		// The new edge hangs derivative's subtree under source; the joined
		// depth must stay within the walkers' horizon.
		up, err := chainDepthTx(tx, "derivative_edges", "source_image_id", "derivative_image_id", source)
		if err != nil {
			return err
		}
		height, err := derivativeHeightTx(tx, derivative)
		if err != nil {
			return err
		}
		if up+height+1 > MaxVersionChainDepth {
			return ErrChainTooDeep
		}
		if _, err := tx.Exec(
			`INSERT INTO derivative_edges (derivative_image_id, source_image_id, created_at) VALUES (?, ?, ?)`,
			derivative, source, nowISO(),
		); err != nil {
			return err
		}
		return pruneQueueForChainTx(tx, "derivative_edges", "source_image_id", "derivative_image_id", source, derivative)
	})
}

// AddNotRelated records the canonicalised pair so it never surfaces in
// the find-pairs queue again.
func (s *Service) AddNotRelated(a, b int64) error {
	if a == b {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if err := pairConflictTx(tx, a, b, "not_related"); err != nil {
			return err
		}
		lo, hi := canonicalPair(a, b)
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO not_related_pairs (a_image_id, b_image_id, created_at) VALUES (?, ?, ?)`,
			lo, hi, nowISO(),
		); err != nil {
			return err
		}
		return pruneQueuePairTx(tx, a, b)
	})
}

// RemoveDupMember unlinks one image from its duplicate group. Idempotent:
// a no-op when the image isn't in a group. If the removal would leave
// the group with a single member, the group is dissolved. If the removed
// image was the original, the largest remaining member is promoted.
func (s *Service) RemoveDupMember(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return removeDupMemberTx(tx, imageID) })
}

// nextDupOriginalQuery picks a dup group's next original (largest file,
// then highest id) from the members that are not leaving. %s carries the
// placeholders for the leaving set - one for a single unlink, the whole
// chunk when a bulk delete decides the group once. Shared so the unlink
// preview and the promotion sites stay byte-identical.
const nextDupOriginalQuery = `SELECT m.image_id FROM dup_group_members m
	JOIN images i ON i.id = m.image_id
	WHERE m.group_id = ? AND m.image_id NOT IN (%s)
	ORDER BY i.file_size DESC, m.image_id DESC
	LIMIT 1`

// dissolveOrKeepGroupTx resolves imageID's group in memberTable and, when
// the member leaving would shrink it past viability (<= 2 members), drops
// the group row - the CASCADE or the caller's member DELETE clears the
// rest. keep is true when the group survives and gid names it; gid == 0
// with keep == false means no membership or a dissolved group.
func dissolveOrKeepGroupTx(tx *sql.Tx, memberTable, groupTable string, imageID int64) (gid int64, keep bool, err error) {
	g, err := lookupGroupIDTx(tx, memberTable, imageID)
	if err != nil || !g.Valid {
		return 0, false, err
	}
	var memberCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM `+memberTable+` WHERE group_id = ?`, g.Int64).Scan(&memberCount); err != nil {
		return 0, false, err
	}
	if memberCount <= 2 {
		_, err := tx.Exec(`DELETE FROM `+groupTable+` WHERE id = ?`, g.Int64)
		return 0, false, err
	}
	return g.Int64, true, nil
}

// promoteNextOriginalTx re-points a dup group's original at the next best
// member when leaverID currently holds it.
func promoteNextOriginalTx(tx *sql.Tx, gid, leaverID int64) error {
	var current int64
	if err := tx.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, gid).Scan(&current); err != nil {
		return err
	}
	if current != leaverID {
		return nil
	}
	var newOriginal int64
	if err := tx.QueryRow(fmt.Sprintf(nextDupOriginalQuery, "?"), gid, leaverID).Scan(&newOriginal); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, newOriginal, gid)
	return err
}

func removeDupMemberTx(tx *sql.Tx, imageID int64) error {
	gid, keep, err := dissolveOrKeepGroupTx(tx, "dup_group_members", "dup_groups", imageID)
	if err != nil || !keep {
		return err
	}
	if err := promoteNextOriginalTx(tx, gid, imageID); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM dup_group_members WHERE image_id = ?`, imageID)
	return err
}

// DissolveDupGroup drops the entire duplicate group. CASCADE clears
// every member row. Idempotent.
func (s *Service) DissolveDupGroup(groupID int64) error {
	_, err := s.db.Write.Exec(`DELETE FROM dup_groups WHERE id = ?`, groupID)
	return err
}

// NextOriginalIfRemoved returns the id removeDupMemberTx would promote
// to original if `removeID` left `groupID`. Same ORDER BY as the
// promotion itself so the preview the UI shows the operator matches
// what the unlink will commit. Returns (0, nil) when the group has
// fewer than three members - the group dissolves and there is no
// new original to name.
func (s *Service) NextOriginalIfRemoved(groupID, removeID int64) (int64, error) {
	var n int
	if err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ?`, groupID,
	).Scan(&n); err != nil {
		return 0, err
	}
	if n < 3 {
		return 0, nil
	}
	var nextID int64
	err := s.db.Read.QueryRow(fmt.Sprintf(nextDupOriginalQuery, "?"), groupID, removeID).Scan(&nextID)
	if err != nil {
		return 0, err
	}
	return nextID, nil
}

// PromoteToOriginal sets imageID as the original of groupID. Errors
// with ErrNotInGroup when imageID isn't a member of the group.
func (s *Service) PromoteToOriginal(groupID, imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ? AND image_id = ?`, groupID, imageID,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotInGroup
		}
		_, err := tx.Exec(`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, imageID, groupID)
		return err
	})
}

// RemoveAltMember unlinks one image from its alternate group.
// Idempotent. Dissolves the group when reduced to a singleton.
func (s *Service) RemoveAltMember(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return removeAltMemberTx(tx, imageID) })
}

func removeAltMemberTx(tx *sql.Tx, imageID int64) error {
	_, keep, err := dissolveOrKeepGroupTx(tx, "alt_group_members", "alt_groups", imageID)
	if err != nil || !keep {
		return err
	}
	_, err = tx.Exec(`DELETE FROM alt_group_members WHERE image_id = ?`, imageID)
	return err
}

// DissolveAltGroup drops the entire alternate group. CASCADE clears
// every member row. Idempotent.
func (s *Service) DissolveAltGroup(groupID int64) error {
	_, err := s.db.Write.Exec(`DELETE FROM alt_groups WHERE id = ?`, groupID)
	return err
}

// MergeAltGroups consolidates N alt groups into one. The lowest id is
// the survivor; every alt_group_members.group_id pointing at the
// others is repointed at the survivor; the now-empty alt_groups rows
// are deleted. Idempotent on a single-group input.
func (s *Service) MergeAltGroups(groupIDs []int64) error {
	groupIDs = dedupAndSortInt64(groupIDs)
	if len(groupIDs) <= 1 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error { return mergeAltGroupsTx(tx, groupIDs) })
}

// MergeDupGroups consolidates N dup groups into one. The lowest id is
// the survivor; member rows from the others are repointed; the
// survivor's original_image_id is taken from the group named by
// keepOriginalFrom. Pass 0 to keep the survivor's existing original.
// Idempotent on a single-group input.
func (s *Service) MergeDupGroups(groupIDs []int64, keepOriginalFrom int64) error {
	groupIDs = dedupAndSortInt64(groupIDs)
	if len(groupIDs) <= 1 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error { return mergeDupGroupsTx(tx, groupIDs, keepOriginalFrom) })
}

// dedupAndSortInt64 returns the input sorted ascending with duplicates
// removed. The merge primitives use this so the lowest id is always at
// index 0 (the survivor) regardless of caller argument order.
func dedupAndSortInt64(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// mergeAltGroupsTx implements MergeAltGroups inside an existing
// transaction. groupIDs must be deduplicated and sorted ascending.
func mergeAltGroupsTx(tx *sql.Tx, groupIDs []int64) error {
	if len(groupIDs) <= 1 {
		return nil
	}
	survivor := groupIDs[0]
	others := groupIDs[1:]
	for _, gid := range others {
		if _, err := tx.Exec(
			`UPDATE alt_group_members SET group_id = ? WHERE group_id = ?`, survivor, gid,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM alt_groups WHERE id = ?`, gid); err != nil {
			return err
		}
	}
	return nil
}

// mergeDupGroupsTx implements MergeDupGroups inside an existing
// transaction. groupIDs must be deduplicated and sorted ascending.
// keepOriginalFrom names which group's original_image_id is copied
// onto the survivor; 0 (or an id not in groupIDs) means "keep the
// survivor's existing original".
func mergeDupGroupsTx(tx *sql.Tx, groupIDs []int64, keepOriginalFrom int64) error {
	if len(groupIDs) <= 1 {
		return nil
	}
	survivor := groupIDs[0]
	others := groupIDs[1:]
	if keepOriginalFrom != 0 && keepOriginalFrom != survivor {
		// Caller asked to inherit a non-survivor's original. Copy it onto
		// the survivor row before we delete the source group.
		if slices.Contains(others, keepOriginalFrom) {
			var original int64
			if err := tx.QueryRow(
				`SELECT original_image_id FROM dup_groups WHERE id = ?`, keepOriginalFrom,
			).Scan(&original); err != nil {
				return err
			}
			if _, err := tx.Exec(
				`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, original, survivor,
			); err != nil {
				return err
			}
		}
	}
	for _, gid := range others {
		if _, err := tx.Exec(
			`UPDATE dup_group_members SET group_id = ? WHERE group_id = ?`, survivor, gid,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM dup_groups WHERE id = ?`, gid); err != nil {
			return err
		}
	}
	return nil
}

// RemoveVersionEdge deletes the edge between parent and child if one
// exists, regardless of which side is which. The schema stores a
// directed (parent, child) row but the operator-facing UI labels both
// "earlier" and "later" buttons with the same form, so a hand-crafted
// post that swaps the sides still drops the edge the operator clicked
// on. Idempotent on a missing edge.
func (s *Service) RemoveVersionEdge(a, b int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM version_edges
		 WHERE (parent_image_id = ? AND child_image_id = ?)
		    OR (parent_image_id = ? AND child_image_id = ?)`,
		a, b, b, a,
	)
	return err
}

// ReverseVersionEdge swaps the parent/child of the named edge in one
// transaction so the chain points the other way. Idempotent on a
// missing edge. The new (child=parent, parent=child) row must not
// collide with the per-side uniqueness of an adjacent chain entry; if
// it would (mid-chain reversal), the function returns ErrVersionExists
// so writeRelationError surfaces the operator-facing "remove the
// adjacent edge first" message rather than the raw SQLite constraint
// error.
func (s *Service) ReverseVersionEdge(parent, child int64) error {
	if parent == child {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`DELETE FROM version_edges WHERE parent_image_id = ? AND child_image_id = ?`, parent, child,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		// After the delete, the swapped row (parent, child) -> (child, parent)
		// must not clash with the schema's per-side UNIQUE constraints. Either
		// side already standing on the new role means an adjacent edge would
		// block the insert.
		var blocked int
		if err := tx.QueryRow(
			`SELECT EXISTS (
				SELECT 1 FROM version_edges WHERE child_image_id = ?
				UNION ALL
				SELECT 1 FROM version_edges WHERE parent_image_id = ?
			)`, parent, child,
		).Scan(&blocked); err != nil {
			return err
		}
		if blocked != 0 {
			return ErrVersionExists
		}
		_, err = tx.Exec(
			`INSERT INTO version_edges (child_image_id, parent_image_id, created_at) VALUES (?, ?, ?)`,
			parent, child, nowISO(),
		)
		return err
	})
}

// RemoveDerivativeEdge deletes the edge between the two images if one
// exists, regardless of which side is the source and which the
// derivative. A hand-crafted post that swaps the sides still drops the
// edge the operator clicked on. Idempotent on a missing edge.
func (s *Service) RemoveDerivativeEdge(a, b int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM derivative_edges
		 WHERE (source_image_id = ? AND derivative_image_id = ?)
		    OR (source_image_id = ? AND derivative_image_id = ?)`,
		a, b, b, a,
	)
	return err
}

// DissolveVersionChain drops every version_edge in the chain that
// contains anyMember. Walks up via child_image_id to the root, then
// down via parent_image_id collecting every member, then DELETEs in
// one statement using `parent_image_id IN (...) OR child_image_id IN
// (...)`. Idempotent on an image with no edges. Depth-capped at
// MaxVersionChainDepth on each side so a malformed cycle can't loop.
func (s *Service) DissolveVersionChain(anyMember int64) error {
	return s.dissolveEdges(anyMember, collectVersionChainMembersTx, "version_edges", "parent_image_id", "child_image_id")
}

// DissolveDerivativeTree drops every derivative_edge in the tree that
// contains anyMember. Walks up via derivative_image_id to the root,
// then DFSes down via source_image_id collecting every member, then
// DELETEs in one statement using `source_image_id IN (...) OR
// derivative_image_id IN (...)`. Idempotent on an image with no edges.
func (s *Service) DissolveDerivativeTree(anyMember int64) error {
	return s.dissolveEdges(anyMember, collectDerivativeTreeMembersTx, "derivative_edges", "source_image_id", "derivative_image_id")
}

// dissolveEdges is the body the two Dissolve methods share: collect the
// group anyMember belongs to, then drop every edge with a member on
// either end. Nothing to do when anyMember sits on no edge.
func (s *Service) dissolveEdges(
	anyMember int64,
	collect func(*sql.Tx, int64) ([]int64, error),
	table, parentCol, childCol string,
) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		members, err := collect(tx, anyMember)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		return deleteEdgesByEndpointsTx(tx, table, parentCol, childCol, members)
	})
}

// collectVersionChainMembersTx walks the chain containing anyMember
// and returns every member id, or nil when anyMember sits on no
// version edge. Up-walk and down-walk each run at most
// MaxVersionChainDepth steps so a malformed cycle in the data can't
// spin indefinitely.
func collectVersionChainMembersTx(tx *sql.Tx, anyMember int64) ([]int64, error) {
	var has int
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM version_edges WHERE parent_image_id = ? OR child_image_id = ?)`,
		anyMember, anyMember,
	).Scan(&has); err != nil {
		return nil, err
	}
	if has == 0 {
		return nil, nil
	}
	root, err := walkToRootTx(tx, "version_edges", "parent_image_id", "child_image_id", anyMember)
	if err != nil {
		return nil, err
	}
	// parent_image_id is UNIQUE, so the descent is the same point seek the
	// upward walk makes, one child per step.
	below, err := ChainPath(tx, "version_edges", "child_image_id", "parent_image_id", root)
	if err != nil {
		return nil, err
	}
	return append([]int64{root}, below...), nil
}

// collectDerivativeTreeMembersTx walks the derivative tree containing
// anyMember and returns every member id, or nil when anyMember sits on
// no derivative edge. Up-walk is depth-capped; the DFS down collects
// every descendant in arbitrary order.
func collectDerivativeTreeMembersTx(tx *sql.Tx, anyMember int64) ([]int64, error) {
	var has int
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM derivative_edges WHERE source_image_id = ? OR derivative_image_id = ?)`,
		anyMember, anyMember,
	).Scan(&has); err != nil {
		return nil, err
	}
	if has == 0 {
		return nil, nil
	}
	root, err := walkToRootTx(tx, "derivative_edges", "source_image_id", "derivative_image_id", anyMember)
	if err != nil {
		return nil, err
	}
	// BFS by tree level so MaxVersionChainDepth bounds genuine depth.
	// A previous DFS implementation incremented depth per stack pop, so
	// a wide tree (single source with >256 derivatives) silently
	// truncated at the 256th child; the cap should describe the tree's
	// vertical reach, not its fan-out.
	members, _, err := chainSpanTx(tx, "derivative_edges", "derivative_image_id", "source_image_id", root)
	return members, err
}

// deleteEdgesByEndpointsTx removes every row in `table` whose `colA` or
// `colB` is one of `ids`. Used by the version-chain and derivative-tree
// dissolve methods to drop every edge between any pair of chain
// members in one statement.
func deleteEdgesByEndpointsTx(tx *sql.Tx, table, colA, colB string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders, idArgs := db.InPlaceholders(ids)
	args := append(append([]any{}, idArgs...), idArgs...)
	q := fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s) OR %s IN (%s)`, table, colA, placeholders, colB, placeholders)
	_, err := tx.Exec(q, args...)
	return err
}

// ReverseDerivativeEdge swaps the source and derivative sides of the
// named edge in one transaction. Idempotent on a missing edge. If the
// would-be new derivative side already has another source, the
// function returns ErrDerivativeExists so writeRelationError surfaces
// the operator-facing message.
func (s *Service) ReverseDerivativeEdge(source, derivative int64) error {
	if source == derivative {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`DELETE FROM derivative_edges WHERE source_image_id = ? AND derivative_image_id = ?`, source, derivative,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		// The swapped row makes `source` the new derivative. PK on
		// derivative_image_id makes that collide with any existing edge
		// where source is already a derivative of another image.
		var blocked int
		if err := tx.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM derivative_edges WHERE derivative_image_id = ?)`, source,
		).Scan(&blocked); err != nil {
			return err
		}
		if blocked != 0 {
			return ErrDerivativeExists
		}
		_, err = tx.Exec(
			`INSERT INTO derivative_edges (derivative_image_id, source_image_id, created_at) VALUES (?, ?, ?)`,
			source, derivative, nowISO(),
		)
		return err
	})
}

// RemoveNotRelated forgets a previously-rejected pair so it becomes
// eligible to resurface in find-pairs again. Idempotent.
func (s *Service) RemoveNotRelated(a, b int64) error {
	lo, hi := canonicalPair(a, b)
	_, err := s.db.Write.Exec(`DELETE FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi)
	return err
}

// ClearVersionEdgeConflictsFor drops only the version_edge rows that
// would block an AddVersionEdge(parent, child) insert: the row where
// `child` is already a child (second parent for it) and the row where
// `parent` is already a parent (second child for it). Edges between
// either endpoint and a third image that don't violate the per-row
// uniqueness keep standing, so the operator's "Replace existing
// version edge" click only sacrifices the directly-conflicting links
// and the rest of the chain stays intact.
func (s *Service) ClearVersionEdgeConflictsFor(parent, child int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM version_edges WHERE child_image_id = ? OR parent_image_id = ?`,
		child, parent,
	)
	return err
}

// ClearDerivativeSourceOf drops the derivative_edge that names
// `derivative` as its derivative side. The schema allows only one
// source per derivative, so this is the single row that blocks a
// re-source. Used by the detail-page "Replace existing source"
// affordance.
func (s *Service) ClearDerivativeSourceOf(derivative int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM derivative_edges WHERE derivative_image_id = ?`,
		derivative,
	)
	return err
}

// ClearBetween drops every relation row that connects a and b, in one
// transaction. Group-shaped relations (duplicate, alternate) keep the
// rest of the group intact - only b's membership goes if the two
// shared a group. Used by the detail-page "Overwrite" affordance so a
// follow-up Add* succeeds without first asking the operator to unlink
// the previous relation by hand.
func (s *Service) ClearBetween(a, b int64) error {
	if a == b {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if share, err := pairShareGroupTx(tx, "dup_group_members", a, b); err != nil {
			return err
		} else if share {
			if err := removeDupMemberTx(tx, b); err != nil {
				return err
			}
		}
		if share, err := pairShareGroupTx(tx, "alt_group_members", a, b); err != nil {
			return err
		} else if share {
			if err := removeAltMemberTx(tx, b); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`DELETE FROM version_edges WHERE (child_image_id = ? AND parent_image_id = ?) OR (child_image_id = ? AND parent_image_id = ?)`,
			a, b, b, a,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`DELETE FROM derivative_edges WHERE (derivative_image_id = ? AND source_image_id = ?) OR (derivative_image_id = ? AND source_image_id = ?)`,
			a, b, b, a,
		); err != nil {
			return err
		}
		lo, hi := canonicalPair(a, b)
		_, err := tx.Exec(`DELETE FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi)
		return err
	})
}

// CopyTagsFromDuplicatesToOriginal inserts every image_tag carried by
// a non-original member of groupID onto the original. Rating tags are
// excluded (the rating system has highest-wins semantics, so a copy
// would silently bump the original's level). INSERT OR IGNORE makes
// the operation idempotent. Returns the count of newly added rows
// across the group. Runs in one transaction so the per-tag usage_count
// refresh stays consistent.
func (s *Service) CopyTagsFromDuplicatesToOriginal(groupID int64) (int, error) {
	var added int64
	err := s.inWriteTx(func(tx *sql.Tx) error {
		var original int64
		if err := tx.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, groupID).Scan(&original); err != nil {
			return err
		}
		// Find the rating category id once; we use it to exclude rating
		// tags from the copy (highest-wins semantics handles them already).
		var ratingCatID sql.NullInt64
		if err := tx.QueryRow(`SELECT id FROM tag_categories WHERE name = 'rating'`).Scan(&ratingCatID); err != nil && err != sql.ErrNoRows {
			return err
		}
		res, err := tx.Exec(`
			INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name, created_at)
			SELECT ?, it.tag_id, 0, 0, NULL, NULL, ?
			FROM image_tags it
			JOIN dup_group_members m ON m.image_id = it.image_id
			LEFT JOIN tags t ON t.id = it.tag_id
			WHERE m.group_id = ? AND m.image_id != ?
			  AND (? IS NULL OR t.category_id != ?)`,
			original, nowISO(), groupID, original, ratingCatID, ratingCatID,
		)
		if err != nil {
			return err
		}
		added, _ = res.RowsAffected()
		// Recount usage_count for the copied tags from non-missing images,
		// the same convention RecalcDB uses; the INSERT above doesn't touch it.
		if _, err := tx.Exec(`
			UPDATE tags SET usage_count = (
				SELECT COUNT(*) FROM image_tags it
				JOIN images i ON i.id = it.image_id
				WHERE it.tag_id = tags.id AND i.is_missing = 0
			) WHERE id IN (
				SELECT it.tag_id FROM image_tags it
				JOIN dup_group_members m ON m.image_id = it.image_id
				WHERE m.group_id = ? AND m.image_id != ?
			)`, groupID, original,
		); err != nil {
			return err
		}
		return nil
	})
	return int(added), err
}

// OnImageDeleteTx fixes up dup_groups.original_image_id (no FK CASCADE)
// and dissolves singleton groups so the caller's subsequent
// `DELETE FROM images WHERE id = ?` doesn't fail on the NOT NULL FK.
// Runs on a caller-held transaction so the image-delete path commits
// the graph fixups alongside the row delete; the FK CASCADE on the
// member tables takes care of the dependent rows. Also drops the image
// from this gallery's in-memory BK-tree (if one is built) so subsequent
// phash queries don't surface a stale id. That drop has no undo, but it
// is rebuildable and the row it describes is on its way out either way.
func (s *Service) OnImageDeleteTx(tx *sql.Tx, imageID int64) error {
	return s.OnImagesDeleteTx(tx, []int64{imageID})
}

// OnImagesDeleteTx is OnImageDeleteTx for a whole delete chunk. Looping
// the per-image form here would not do: with two members of a
// three-member group in the same chunk it can promote a member the
// chunk is about to remove, and the FK on dup_groups.original_image_id
// then fails the whole transaction. Each touched group is decided once
// against the membership that survives the chunk.
func (s *Service) OnImagesDeleteTx(tx *sql.Tx, imageIDs []int64) error {
	if len(imageIDs) == 0 {
		return nil
	}
	if err := groupsOnBatchDeleteTx(tx, "dup_group_members", "dup_groups", imageIDs, true); err != nil {
		return err
	}
	if err := groupsOnBatchDeleteTx(tx, "alt_group_members", "alt_groups", imageIDs, false); err != nil {
		return err
	}
	if tree := DefaultRegistry.Lookup(s.db); tree != nil && tree.Built() {
		for _, id := range imageIDs {
			tree.Remove(id)
		}
	}
	return nil
}

// groupsOnBatchDeleteTx drops every touched group the chunk would leave
// with fewer than two members and, for dup groups, re-points an original
// that is leaving at the best survivor. The membership rows themselves
// go with the caller's DELETE through the FK CASCADE.
func groupsOnBatchDeleteTx(tx *sql.Tx, memberTbl, groupTbl string, imageIDs []int64, promoteOriginal bool) error {
	placeholders, args := db.InPlaceholders(imageIDs)
	groupIDs, err := db.QueryIDs(tx,
		`SELECT DISTINCT group_id FROM `+memberTbl+` WHERE image_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	for _, gid := range groupIDs {
		gidArgs := append([]any{gid}, args...)
		var survivors int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM `+memberTbl+` WHERE group_id = ? AND image_id NOT IN (`+placeholders+`)`,
			gidArgs...,
		).Scan(&survivors); err != nil {
			return err
		}
		if survivors < 2 {
			if _, err := tx.Exec(`DELETE FROM `+groupTbl+` WHERE id = ?`, gid); err != nil {
				return err
			}
			continue
		}
		if !promoteOriginal {
			continue
		}
		var original int64
		if err := tx.QueryRow(`SELECT original_image_id FROM `+groupTbl+` WHERE id = ?`, gid).Scan(&original); err != nil {
			return err
		}
		if !slices.Contains(imageIDs, original) {
			continue
		}
		var newOriginal int64
		if err := tx.QueryRow(fmt.Sprintf(nextDupOriginalQuery, placeholders), gidArgs...).Scan(&newOriginal); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE `+groupTbl+` SET original_image_id = ? WHERE id = ?`, newOriginal, gid); err != nil {
			return err
		}
	}
	return nil
}

// lookupGroupIDTx returns the dup_group_members.group_id or
// alt_group_members.group_id for imageID. sql.NullInt64{Valid:false}
// when the image isn't currently in any group of that type.
func lookupGroupIDTx(tx *sql.Tx, table string, imageID int64) (sql.NullInt64, error) {
	var gid sql.NullInt64
	q := fmt.Sprintf(`SELECT group_id FROM %s WHERE image_id = ?`, table)
	err := tx.QueryRow(q, imageID).Scan(&gid)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}, nil
	}
	if err != nil {
		return sql.NullInt64{}, err
	}
	return gid, nil
}

// pairHasOtherRelationTx reports whether the pair already carries any
// declared relation outside of `ignore` ("duplicate", "alternate",
// "version", "derivative", "not_related"). Used by every Add* method
// to short-circuit before mutating - the spec demands at most one
// type per pair.
func pairHasOtherRelationTx(tx *sql.Tx, a, b int64, ignore string) (bool, error) {
	if ignore != "duplicate" {
		ok, err := pairShareGroupTx(tx, "dup_group_members", a, b)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	if ignore != "alternate" {
		ok, err := pairShareGroupTx(tx, "alt_group_members", a, b)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	if ignore != "version" {
		ok, err := pairEdgeExistsTx(tx, "version_edges", "child_image_id", "parent_image_id", a, b)
		if err != nil || ok {
			return ok, err
		}
	}
	if ignore != "derivative" {
		ok, err := pairEdgeExistsTx(tx, "derivative_edges", "derivative_image_id", "source_image_id", a, b)
		if err != nil || ok {
			return ok, err
		}
	}
	if ignore != "not_related" {
		lo, hi := canonicalPair(a, b)
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi,
		).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// pairEdgeExistsTx reports whether an edge table holds the pair, in either
// orientation. The table and both column names are compile-time constants,
// never input, and the OR covers both directions so which column is named
// first does not change the answer.
func pairEdgeExistsTx(tx *sql.Tx, table, colA, colB string, a, b int64) (bool, error) {
	var n int
	err := tx.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*) FROM %s WHERE (%s = ? AND %s = ?) OR (%s = ? AND %s = ?)`,
		table, colA, colB, colA, colB), a, b, b, a).Scan(&n)
	return n > 0, err
}

// pairChainRelatedTx reports whether a and b already sit on one
// root-to-leaf path of a version chain or a derivative tree. The edge
// tables hold single steps, so testing them for a direct edge alone
// reads two images three steps apart as strangers.
func pairChainRelatedTx(tx *sql.Tx, a, b int64, ignore string) (bool, error) {
	if ignore != "version" {
		ok, err := chainRelatesTx(tx, "version_edges", "parent_image_id", "child_image_id", a, b)
		if err != nil || ok {
			return ok, err
		}
	}
	if ignore == "derivative" {
		return false, nil
	}
	return chainRelatesTx(tx, "derivative_edges", "source_image_id", "derivative_image_id", a, b)
}

// pairConflictTx returns the error declaring `label` on the pair would
// violate, or nil when the pair is free. A direct relation is
// overwritable and reports ErrRelationConflict; a link through a third
// image leaves nothing between the two to drop, so it reports
// ErrIndirectRelation and the Overwrite affordance stays hidden.
func pairConflictTx(tx *sql.Tx, a, b int64, label string) error {
	if conflict, err := pairHasOtherRelationTx(tx, a, b, label); err != nil {
		return err
	} else if conflict {
		return ErrRelationConflict
	}
	if chained, err := pairChainRelatedTx(tx, a, b, label); err != nil {
		return err
	} else if chained {
		return ErrIndirectRelation
	}
	return nil
}

// pairSettledTx reports whether the pair already has an answer: a
// declared relation, a not-related mark, or a place on one root-to-leaf
// path. What the detectors check before queueing a candidate.
func pairSettledTx(tx *sql.Tx, a, b int64) (bool, error) {
	if got, err := pairHasOtherRelationTx(tx, a, b, ""); err != nil || got {
		return got, err
	}
	return pairChainRelatedTx(tx, a, b, "")
}

// pruneQueueForGroupTx deletes potential_relation_pairs rows whose
// endpoints are both members of the group that `anchor` now belongs
// to. Resolving one pair in a group can make other queue rows
// redundant (their endpoints land in the same group via the merge);
// this sweeps them so the session UI never asks the operator to
// re-decide a pair that is already inside a declared group. `anchor`
// being a non-member is a quiet no-op.
func pruneQueueForGroupTx(tx *sql.Tx, table string, anchor int64) error {
	gid, err := lookupGroupIDTx(tx, table, anchor)
	if err != nil {
		return err
	}
	if !gid.Valid {
		return nil
	}
	q := fmt.Sprintf(`
		DELETE FROM potential_relation_pairs
		WHERE a_image_id IN (SELECT image_id FROM %s WHERE group_id = ?)
		  AND b_image_id IN (SELECT image_id FROM %s WHERE group_id = ?)`, table, table)
	_, err = tx.Exec(q, gid.Int64, gid.Int64)
	return err
}

// pruneQueueForChainTx drops the queue rows a new edge answers: every
// pair joining one of parent's ancestors (parent included) to a node in
// child's subtree (child included), since those two sets now sit on one
// root-to-leaf path. Clearing only the edge's own pair left the rest
// queued, and the session went on asking about images the tree already
// related.
func pruneQueueForChainTx(tx *sql.Tx, table, parentCol, childCol string, parent, child int64) error {
	above, _, err := chainSpanTx(tx, table, parentCol, childCol, parent)
	if err != nil {
		return err
	}
	below, _, err := chainSpanTx(tx, table, childCol, parentCol, child)
	if err != nil {
		return err
	}
	aIn, aArgs := db.InPlaceholders(above)
	bIn, bArgs := db.InPlaceholders(below)
	q := fmt.Sprintf(`
		DELETE FROM potential_relation_pairs
		WHERE (a_image_id IN (%s) AND b_image_id IN (%s))
		   OR (a_image_id IN (%s) AND b_image_id IN (%s))`, aIn, bIn, bIn, aIn)
	_, err = tx.Exec(q, slices.Concat(aArgs, bArgs, bArgs, aArgs)...)
	return err
}

// pruneQueuePairTx drops the canonical queue row for a pair that just
// gained an edge relation. Used by the rejection, which relates nothing
// beyond the two images; the edge adds sweep their whole path through
// pruneQueueForChainTx and the group methods through
// pruneQueueForGroupTx.
func pruneQueuePairTx(tx *sql.Tx, a, b int64) error {
	lo, hi := canonicalPair(a, b)
	_, err := tx.Exec(
		`DELETE FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi,
	)
	return err
}

// pairShareGroupTx reports whether a and b sit in the same group of
// the given membership table (dup_group_members or alt_group_members).
func pairShareGroupTx(tx *sql.Tx, table string, a, b int64) (bool, error) {
	q := fmt.Sprintf(`
		SELECT COUNT(*) FROM %s m1
		JOIN %s m2 ON m1.group_id = m2.group_id
		WHERE m1.image_id = ? AND m2.image_id = ?`, table, table)
	var n int
	if err := tx.QueryRow(q, a, b).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// groupMerge names the per-kind pieces of the group merge: the
// membership table, how a fresh group row is created (dup_groups carries
// original_image_id, alt_groups does not), and how two existing groups
// are folded together.
type groupMerge struct {
	membersTbl  string
	insertGroup func(tx *sql.Tx, original int64) (int64, error)
	mergeGroups func(tx *sql.Tx, ids []int64) error
}

// dupGroupMerge folds duplicates. The caller has already decided which
// side is the original by passing it first; the session UI puts the
// bigger-filesize image in slot `a` by default. Existing-group cases
// preserve whichever original is already in place, and the operator can
// flip it from the browse-groups Merge dialog.
var dupGroupMerge = groupMerge{
	membersTbl: "dup_group_members",
	insertGroup: func(tx *sql.Tx, original int64) (int64, error) {
		var gid int64
		err := tx.QueryRow(
			`INSERT INTO dup_groups (original_image_id, created_at) VALUES (?, ?) RETURNING id`,
			original, nowISO(),
		).Scan(&gid)
		return gid, err
	},
	mergeGroups: func(tx *sql.Tx, ids []int64) error { return mergeDupGroupsTx(tx, ids, 0) },
}

// altGroupMerge folds variants. Alt groups carry no original.
var altGroupMerge = groupMerge{
	membersTbl: "alt_group_members",
	insertGroup: func(tx *sql.Tx, _ int64) (int64, error) {
		var gid int64
		err := tx.QueryRow(`INSERT INTO alt_groups (created_at) VALUES (?) RETURNING id`, nowISO()).Scan(&gid)
		return gid, err
	},
	mergeGroups: mergeAltGroupsTx,
}

// mergeIntoGroupTx is the five-case group merge from §6.4: both
// singletons, one existing member, the other existing member, same group
// already (idempotent no-op), and two different groups.
func mergeIntoGroupTx(tx *sql.Tx, a, b int64, cfg groupMerge) error {
	groupA, err := lookupGroupIDTx(tx, cfg.membersTbl, a)
	if err != nil {
		return err
	}
	groupB, err := lookupGroupIDTx(tx, cfg.membersTbl, b)
	if err != nil {
		return err
	}
	addMember := func(gid, imageID int64) error {
		_, err := tx.Exec(
			`INSERT INTO `+cfg.membersTbl+` (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			imageID, gid, nowISO(),
		)
		return err
	}
	switch {
	case !groupA.Valid && !groupB.Valid:
		gid, err := cfg.insertGroup(tx, a)
		if err != nil {
			return err
		}
		now := nowISO()
		if _, err := tx.Exec(
			`INSERT INTO `+cfg.membersTbl+` (image_id, group_id, created_at) VALUES (?, ?, ?), (?, ?, ?)`,
			a, gid, now, b, gid, now,
		); err != nil {
			return err
		}
	case groupA.Valid && !groupB.Valid:
		return addMember(groupA.Int64, b)
	case !groupA.Valid && groupB.Valid:
		return addMember(groupB.Int64, a)
	case groupA.Int64 == groupB.Int64:
		// Already in the same group.
	default:
		lo, hi := groupA.Int64, groupB.Int64
		if hi < lo {
			lo, hi = hi, lo
		}
		// The merge helpers require ascending ids so the lowest survives.
		return cfg.mergeGroups(tx, []int64{lo, hi})
	}
	return nil
}
