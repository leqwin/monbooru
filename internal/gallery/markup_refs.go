package gallery

import (
	"database/sql"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

// TagRefTarget is a markup tag reference resolved against the catalog.
type TagRefTarget struct {
	Color string
	Found bool
}

type tagRefRow struct {
	name     string
	category string
	color    string
	usage    int
}

// ResolveTagRefs looks up the tag references a page's markup carries, in one
// pass over the whole page. A reference may carry the `category:name`
// qualifier the search bar takes; an unqualified name that lives in several
// categories takes the most used one, and an alias takes its canonical row's
// colour so the swatch matches where the link lands.
func ResolveTagRefs(database *db.DB, refs []string) map[string]TagRefTarget {
	out := make(map[string]TagRefTarget, len(refs))
	var names []string
	for _, ref := range refs {
		out[ref] = TagRefTarget{}
		names = append(names, tags.NormalizeTagName(ref))
		if cat, name, ok := strings.Cut(ref, ":"); ok && cat != "" && name != "" {
			names = append(names, tags.NormalizeTagName(name))
		}
	}
	placeholders, args := db.InPlaceholders(names)
	if placeholders == "" {
		return out
	}
	rows, err := db.QueryAll(database.Read, func(rows *sql.Rows) (tagRefRow, error) {
		var r tagRefRow
		err := rows.Scan(&r.name, &r.category, &r.color, &r.usage)
		return r, err
	}, `SELECT t.name, tc.name, COALESCE(cc.color, tc.color), COALESCE(ct.usage_count, t.usage_count)
	    FROM tags t
	    JOIN tag_categories tc ON tc.id = t.category_id
	    LEFT JOIN tags ct ON ct.id = t.canonical_tag_id
	    LEFT JOIN tag_categories cc ON cc.id = ct.category_id
	    WHERE t.name IN (`+placeholders+`)`, args...)
	if err != nil {
		return out
	}
	for _, ref := range refs {
		cat, name, qualified := strings.Cut(ref, ":")
		if best, ok := pickTagRef(rows, tags.NormalizeTagName(ref), ""); ok {
			out[ref] = best
		} else if qualified && cat != "" && name != "" {
			if best, ok := pickTagRef(rows, tags.NormalizeTagName(name), cat); ok {
				out[ref] = best
			}
		}
	}
	return out
}

// pickTagRef takes the most used row carrying name, restricted to one category
// when the reference named one.
func pickTagRef(rows []tagRefRow, name, category string) (TagRefTarget, bool) {
	best, found := tagRefRow{usage: -1}, false
	for _, r := range rows {
		if r.name != name || (category != "" && r.category != category) {
			continue
		}
		if r.usage > best.usage {
			best, found = r, true
		}
	}
	return TagRefTarget{Color: best.color, Found: found}, found
}

// ExistingImageIDs reports which of ids the gallery still holds, so a markup
// reference to a deleted image renders as text instead of a dead link.
func ExistingImageIDs(database *db.DB, ids []int64) map[int64]bool {
	out := map[int64]bool{}
	placeholders, args := db.InPlaceholders(ids)
	if placeholders == "" {
		return out
	}
	found, err := db.QueryIDs(database.Read, `SELECT id FROM images WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return out
	}
	for _, id := range found {
		out[id] = true
	}
	return out
}

// ImageIDsBySourceURL is the batch form of ImageIDBySourceURL: it turns the
// off-site links a page's markup carries into the local images whose origins
// serve them, so a note linking a booru post links this gallery's copy.
func ImageIDsBySourceURL(database *db.DB, urls []string) map[string]int64 {
	out := map[string]int64{}
	placeholders, args := db.InPlaceholders(urls)
	if placeholders == "" {
		return out
	}
	type hit struct {
		url string
		id  int64
	}
	hits, err := db.QueryAll(database.Read, func(rows *sql.Rows) (hit, error) {
		var h hit
		err := rows.Scan(&h.url, &h.id)
		return h, err
	}, `SELECT url, MIN(image_id) FROM image_sources WHERE url IN (`+placeholders+`) GROUP BY url`, args...)
	if err != nil {
		return out
	}
	for _, h := range hits {
		out[h.url] = h.id
	}
	return out
}
