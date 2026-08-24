// Package counts owns the whole-library tallies more than one caller
// divides by, and caches them per gallery. They live here rather than in
// internal/db because "visible" and "untagged" are domain predicates, not
// driver concerns - and rather than in internal/tags or internal/search
// because both of those read them and neither owns "the number of images
// in the library".
//
// The per-pool state hangs off a registry keyed by *db.DB, the shape
// relations.DefaultRegistry already uses. Entries are created on first
// read, so a caller never has to register one; Release drops a gallery's
// entry when its context closes.
package counts

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/db"
)

// cache is one gallery's tallies.
type cache struct {
	db *db.DB
	// untaggedVisible / autoUntaggedVisible cache the count subtrahends
	// behind tagged:true / autotagged:true partition reads. The
	// underlying NOT EXISTS walk over image_tags is multi-second on a
	// million-row library; Invalidate drops both on every image_tags
	// membership write.
	untaggedVisible     atomic.Pointer[int]
	autoUntaggedVisible atomic.Pointer[int]
	// visibleCount caches the non-missing image total. Cheap on its own
	// - an index scan of idx_images_missing - but it is the divisor
	// behind every tag-similarity weight, so the tag-pairs pass would
	// otherwise re-run it once per image in the library.
	visibleCount atomic.Pointer[int]
	// countedTags caches the per-image counted-tag totals the overlap
	// score divides by. Dropped alongside the counts above.
	countedTags atomic.Pointer[CountedTags]
}

var (
	mu     sync.Mutex
	caches = map[*db.DB]*cache{}
)

func forDB(database *db.DB) *cache {
	mu.Lock()
	defer mu.Unlock()
	c, ok := caches[database]
	if !ok {
		c = &cache{db: database}
		caches[database] = c
	}
	return c
}

// Release drops a gallery's tallies. Called when its context is
// destroyed (gallery removal, server shutdown), beside the BK-tree's own
// unregister; missing it leaks one small struct, not correctness.
func Release(database *db.DB) {
	mu.Lock()
	delete(caches, database)
	mu.Unlock()
}

// cachedCount returns the cached value or runs sql once. Errors return
// (0, false) so the fastCount* callers can fall back to the slow path
// without per-call error handling.
func (c *cache) cachedCount(slot *atomic.Pointer[int], sql string) (int, bool) {
	if p := slot.Load(); p != nil {
		return *p, true
	}
	var n int
	if err := c.db.Read.QueryRow(sql).Scan(&n); err != nil {
		return 0, false
	}
	slot.Store(&n)
	return n, true
}

// UntaggedVisibleCount returns the cached count of visible images that
// carry no image_tags row, or queries it on demand. fastCountTagged
// subtracts this from the visible total to derive an exact tagged:true
// partition without re-walking image_tags on every search.
func UntaggedVisibleCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.untaggedVisible,
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id)`)
}

// VisibleCount returns the cached count of non-missing images, or
// queries it on demand.
func VisibleCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.visibleCount, `SELECT COUNT(*) FROM images WHERE is_missing = 0`)
}

// AutoUntaggedVisibleCount is UntaggedVisibleCount restricted to
// image_tags rows carrying is_auto = 1 - the subtrahend behind
// autotagged:true. There is no covering (image_id, is_auto) index, so
// the NOT-EXISTS walk is heavier than the bare-untagged one above.
func AutoUntaggedVisibleCount(database *db.DB) (int, bool) {
	c := forDB(database)
	return c.cachedCount(&c.autoUntaggedVisible,
		`SELECT COUNT(*) FROM images i
		 WHERE is_missing = 0
		   AND NOT EXISTS (
		         SELECT 1 FROM image_tags it
		         WHERE it.image_id = i.id AND it.is_auto = 1
		       )`)
}

// CountedTags holds every image's counted-tag total - its non-meta
// tags at or under maxUsage - in image-id order. Parallel slices
// rather than a map: the tally is read once per candidate during a
// similarity ranking and a million-image library costs 12 MB here
// against four times that in map buckets.
type CountedTags struct {
	maxUsage int64
	ids      []int64
	totals   []int32
	// used stamps the last read so the reclaim loop can drop tallies
	// nothing is ranking against.
	used atomic.Int64
}

// Total returns id's counted-tag total; images carrying none are
// absent from the walk and answer 0.
func (c *CountedTags) Total(id int64) int32 {
	if i, ok := slices.BinarySearch(c.ids, id); ok {
		return c.totals[i]
	}
	return 0
}

// CountedTagTotals returns the tallies the tag-overlap score divides
// by, walking image_tags once on first use. Deriving them per query
// instead means scanning every candidate's tag rows through both tag
// joins, which is seconds on a large library; here the ranking pays a
// lookup per candidate. Rebuilt when maxUsage moves with the visible
// count, and dropped by Invalidate.
func CountedTagTotals(ctx context.Context, database *db.DB, maxUsage int64) (*CountedTags, error) {
	c := forDB(database)
	if t := c.countedTags.Load(); t != nil && t.maxUsage == maxUsage {
		t.used.Store(time.Now().UnixNano())
		return t, nil
	}
	rows, err := database.Read.QueryContext(ctx,
		`SELECT it.image_id, count(*)
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE tc.name != 'meta' AND t.usage_count <= ?
		  GROUP BY it.image_id`, maxUsage)
	if err != nil {
		return nil, fmt.Errorf("counted tag totals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	totals := &CountedTags{maxUsage: maxUsage}
	for rows.Next() {
		var id int64
		var total int32
		if err := rows.Scan(&id, &total); err != nil {
			return nil, err
		}
		totals.ids = append(totals.ids, id)
		totals.totals = append(totals.totals, total)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	totals.used.Store(time.Now().UnixNano())
	c.countedTags.Store(totals)
	return totals, nil
}

// ReleaseIdleCountedTags drops the tallies when nothing has read them
// for at least `after`, returning whether it did. The next reader walks
// image_tags again; the point is that an idle gallery shouldn't hold an
// index only a similarity browse needs.
func ReleaseIdleCountedTags(database *db.DB, after time.Duration) bool {
	c := forDB(database)
	t := c.countedTags.Load()
	if t == nil || time.Since(time.Unix(0, t.used.Load())) < after {
		return false
	}
	return c.countedTags.CompareAndSwap(t, nil)
}

// Invalidate drops every cached tally for one gallery. Call after a
// write that changes image_tags membership (tag add/remove, batch tag,
// implication propagation, autotag ingest, image delete) so the next
// reader recomputes the slow subtrahends from current state. Cheap to
// call - just a few atomic stores - so over-invalidating costs nothing.
func Invalidate(database *db.DB) {
	c := forDB(database)
	c.untaggedVisible.Store(nil)
	c.autoUntaggedVisible.Store(nil)
	c.visibleCount.Store(nil)
	c.countedTags.Store(nil)
}
