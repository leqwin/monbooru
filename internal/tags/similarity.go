package tags

import (
	"database/sql"
	"errors"
	"math"
	"sort"

	"github.com/monbooru/monbooru/internal/db"
)

// Tag-set similarity between two images: the metric behind the
// `similar:` search keyword.

// errVisibleCount surfaces a failed visible-count read, which leaves
// rarity undefined and so has no usable fallback.
var errVisibleCount = errors.New("tags: visible image count unavailable")

// SimilarMaxTagUsage bounds which tags can pull candidates into a
// scan: a tag sitting on a large share of the library would drag every
// one of those rows in.
const SimilarMaxTagUsage = relatedMaxTagUsage

// evidenceUsageCap is the usage_count past which a tag neither opens a
// candidate scan nor counts as something two images have in common. The
// absolute bound above is unreachable below a library ten times its
// size, which leaves a tag on half a small library reading as evidence,
// so the cap follows the library instead; the floor keeps it from
// rounding to nothing under thirty images.
func evidenceUsageCap(visible int64) int64 {
	return min(int64(SimilarMaxTagUsage), max(3, visible/10))
}

// categoryWeights scales a tag's rarity by how strongly its namespace
// identifies the subject. Artist is the one namespace worth a bump:
// character and copyright are constant across a cluster, so weighting
// them up rewards "same series" over "same picture". Categories absent
// here weigh 1; meta never reaches the map because it is excluded from
// the counted set.
var categoryWeights = map[string]float64{
	"artist": 3.0,
}

// SimilarityTag is one counted seed tag and what a candidate earns for
// carrying it. Seeds is false past evidenceUsageCap: the tag still
// scores and still counts toward both norms - dropping it would let a
// truncated tag set inflate the score - but it neither opens a candidate
// scan nor counts as evidence. Implied says the row came from another
// tag's fan-out rather than from a decision about this image, which is
// the same distinction on the evidence side.
//
// Field order and width are deliberate: the whole-library pass holds
// one of these per counted tag row, where the padding a wider id costs
// runs to hundreds of megabytes.
type SimilarityTag struct {
	Weight  float64
	TagID   int32
	Seeds   bool
	Implied bool
}

// SimilaritySeed carries the scoring inputs for one seed image: its
// counted tags with their weights and the norm the cosine divides by.
// An empty Tags slice means the seed has nothing to match on -
// untagged or meta-only - and every score against it is zero.
type SimilaritySeed struct {
	ImageID int64
	Tags    []SimilarityTag
	Norm    float64
}

// TagIDs returns every counted tag id.
func (s SimilaritySeed) TagIDs() []int64 {
	ids := make([]int64, len(s.Tags))
	for i, t := range s.Tags {
		ids[i] = int64(t.TagID)
	}
	return ids
}

// SimilarityScore is the one weighted-cosine formula every scoring path
// shares: shared weight over the geometric mean of the two sides'
// norms. It is the cosine between the two tag sets read as vectors of
// sqrt(weight), so Cauchy-Schwarz bounds it by 1 with equality exactly
// when the counted sets coincide, and the clamp only absorbs float
// rounding. Weights enter linearly rather than squared on purpose: a
// squared weight lets two or three rare shared tags carry almost the
// whole score, which reads as "identical" for images that merely share
// a character.
func SimilarityScore(shared, seedNorm, candNorm float64) float64 {
	if seedNorm <= 0 || candNorm <= 0 {
		return 0
	}
	return math.Min(1, shared/math.Sqrt(seedNorm*candNorm))
}

// LoadSimilaritySeed reads imageID's counted tags and weights each by
// rarity times its category multiplier. Rarity is ln(N / usage_count)
// against the visible image count, so a tag carried by (almost) every
// image weighs ~0 and drops out of both the numerator and the norm on
// its own.
func LoadSimilaritySeed(database *db.DB, imageID int64) (SimilaritySeed, error) {
	seed := SimilaritySeed{ImageID: imageID}
	n, ok := database.VisibleCount()
	if !ok {
		return seed, errVisibleCount
	}
	visible := int64(n)
	if visible <= 0 {
		return seed, nil
	}
	rows, err := database.Read.Query(
		`SELECT it.tag_id, t.usage_count, tc.name, it.is_implied
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE it.image_id = ? AND tc.name != 'meta'
		  ORDER BY it.tag_id`,
		imageID,
	)
	if err != nil {
		return seed, err
	}
	defer func() { _ = rows.Close() }()
	evidence := evidenceUsageCap(visible)
	for rows.Next() {
		var tagID, usage int64
		var category string
		var implied bool
		if err := rows.Scan(&tagID, &usage, &category, &implied); err != nil {
			return seed, err
		}
		w := tagWeight(visible, usage, category)
		if w <= 0 {
			continue
		}
		seed.Tags = append(seed.Tags, SimilarityTag{
			TagID: int32(tagID), Weight: w, Seeds: usage <= evidence, Implied: implied,
		})
		seed.Norm += w
	}
	return seed, rows.Err()
}

// SimilarityCorpusImage is one scorable image in a whole-library
// pass: the counted tags LoadSimilaritySeed would build for it,
// sorted by tag id, plus its norm and the cbz flag the type partition
// compares.
type SimilarityCorpusImage struct {
	ID   int64
	CBZ  bool
	Tags []SimilarityTag
	Norm float64
}

// LoadSimilarityCorpus reads every visible image carrying at least
// minTagCount tags in one pass, ordered by image id. Images whose
// counted set comes back empty are dropped: with nothing to share
// they can neither seed a match nor be one.
func LoadSimilarityCorpus(database *db.DB, minTagCount int) ([]SimilarityCorpusImage, error) {
	n, ok := database.VisibleCount()
	if !ok {
		return nil, errVisibleCount
	}
	visible := int64(n)
	if visible <= 0 {
		return nil, nil
	}
	// Weights and eligibility are read once each, then image_tags streams
	// on its own primary key. Joining tags and tag_categories per
	// membership row instead would price the same handful of global
	// lookups millions of times over.
	weights, countedRows, err := loadTagWeights(database, visible)
	if err != nil {
		return nil, err
	}
	eligible, err := loadScorableImages(database, minTagCount)
	if err != nil {
		return nil, err
	}
	rows, err := database.Read.Query(
		`SELECT image_id, tag_id, is_implied FROM image_tags ORDER BY image_id, tag_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	// Every image's tags land in one arena rather than a slice each: the
	// whole-library pass walks these lists tens of millions of times, and
	// scattered per-image allocations turn each walk into a chase through
	// the heap. Slices are handed out after the fill, since growing the
	// arena moves it - and sizing it from the counted rows keeps it from
	// growing at all, which on a large library would mean copying
	// hundreds of megabytes with both copies live.
	var corpus []SimilarityCorpusImage
	arena := make([]SimilarityTag, 0, countedRows)
	starts := make([]int, 0, len(eligible))
	var current int64 = -1
	keep := false
	for rows.Next() {
		var imageID, tagID int64
		var implied bool
		if err := rows.Scan(&imageID, &tagID, &implied); err != nil {
			return nil, err
		}
		if imageID != current {
			current = imageID
			cbz, ok := eligible[imageID]
			keep = ok
			if keep {
				corpus = append(corpus, SimilarityCorpusImage{ID: imageID, CBZ: cbz})
				starts = append(starts, len(arena))
			}
		}
		if !keep {
			continue
		}
		t, ok := weights[tagID]
		if !ok || t.Weight <= 0 {
			continue
		}
		t.Implied = implied
		arena = append(arena, t)
		corpus[len(corpus)-1].Norm += t.Weight
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range corpus {
		end := len(arena)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		corpus[i].Tags = arena[starts[i]:end:end]
	}
	return corpus, nil
}

// loadTagWeights resolves every tag's weight once, keyed by tag id,
// and totals their usage: since usage_count is the visible-image count
// per tag, the sum bounds how many rows the corpus arena can hold.
func loadTagWeights(database *db.DB, visible int64) (map[int64]SimilarityTag, int, error) {
	rows, err := database.Read.Query(
		`SELECT t.id, t.usage_count, tc.name FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE tc.name != 'meta'`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	weights := make(map[int64]SimilarityTag)
	evidence := evidenceUsageCap(visible)
	rowTotal := 0
	for rows.Next() {
		var tagID, usage int64
		var category string
		if err := rows.Scan(&tagID, &usage, &category); err != nil {
			return nil, 0, err
		}
		w := tagWeight(visible, usage, category)
		if w <= 0 {
			continue
		}
		weights[tagID] = SimilarityTag{TagID: int32(tagID), Weight: w, Seeds: usage <= evidence}
		rowTotal += int(usage)
	}
	return weights, rowTotal, rows.Err()
}

// loadScorableImages returns the visible images carrying at least
// minTagCount tags, mapped to whether they are archives.
func loadScorableImages(database *db.DB, minTagCount int) (map[int64]bool, error) {
	rows, err := database.Read.Query(
		`SELECT id, file_type FROM images WHERE is_missing = 0 AND tag_count >= ?`, minTagCount)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]bool)
	for rows.Next() {
		var id int64
		var fileType string
		if err := rows.Scan(&id, &fileType); err != nil {
			return nil, err
		}
		out[id] = fileType == "cbz"
	}
	return out, rows.Err()
}

// Tag overlap: the metric behind the `similar:` keyword. It asks how
// much of two images' tagging is the same, where the weighted score
// above asks whether what they share is unusual enough to mark the same
// work. Browsing wants the first question, the pair queue the second.

// OverlapSeed is the seed side of an overlap score: the tags a
// candidate can share with it.
type OverlapSeed struct {
	ImageID int64
	TagIDs  []int64
	// MaxUsage is the usage_count both sides are counted under, so the
	// candidate's own tally is taken over the same set of tags.
	MaxUsage int64
}

// LoadOverlapSeed reads the tags worth sharing: non-meta, under the
// usage cap, and short of the whole library, so boilerplate every image
// carries counts for nobody.
func LoadOverlapSeed(database *db.DB, imageID int64) (OverlapSeed, error) {
	seed := OverlapSeed{ImageID: imageID}
	n, ok := database.VisibleCount()
	if !ok {
		return seed, errVisibleCount
	}
	seed.MaxUsage = min(int64(SimilarMaxTagUsage), int64(n)-1)
	if seed.MaxUsage < 1 {
		return seed, nil
	}
	ids, err := db.QueryIDs(database.Read,
		`SELECT it.tag_id
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE it.image_id = ? AND tc.name != 'meta' AND t.usage_count <= ?
		  ORDER BY it.tag_id`,
		imageID, seed.MaxUsage)
	seed.TagIDs = ids
	return seed, err
}

// OverlapScore is the Dice coefficient over the two tag counts: twice
// what they share over what they carry between them. Normalising both
// ways is what stops a barely-tagged image and an exhaustively tagged
// one from winning on shape rather than on content.
func OverlapScore(shared, seedTags, candidateTags int) float64 {
	total := seedTags + candidateTags
	if total <= 0 {
		return 0
	}
	return 2 * float64(shared) / float64(total)
}

// countedJoin is the join and filter that define a candidate's
// counted tags, matching what LoadOverlapSeed kept for the seed.
func countedJoin(alias string) string {
	return " FROM image_tags " + alias +
		" JOIN tags t ON t.id = " + alias + ".tag_id" +
		" JOIN tag_categories tc ON tc.id = t.category_id" +
		" WHERE tc.name != 'meta' AND t.usage_count <= ?"
}

// MinShared returns the fewest of the seed's tags a candidate must
// carry to reach score. The score only falls as the candidate's own
// counted set grows, so 2n/(len+n) is its ceiling at n shared tags -
// computed with the same division the score does, so a rounding edge
// can only admit a candidate the score then rejects, never drop one.
func (s OverlapSeed) MinShared(score float64) int {
	for n := 0; n <= len(s.TagIDs); n++ {
		if 2*float64(n)/float64(len(s.TagIDs)+n) >= score {
			return n
		}
	}
	return len(s.TagIDs) + 1
}

// ScoreExpr renders the overlap score of the row named by imageCol,
// plus its bind args. One scan of the candidate's counted tags yields
// both how many it shares and how many it has. alias names the
// image_tags instance so the expression can nest under another scan.
func (s OverlapSeed) ScoreExpr(imageCol, alias string) (string, []any) {
	placeholders, args := db.InPlaceholders(s.TagIDs)
	expr := "(SELECT 2.0 * sum(CASE WHEN " + alias + ".tag_id IN (" + placeholders + ") THEN 1 ELSE 0 END)" +
		" / (? + count(*))" + countedJoin(alias) +
		" AND " + alias + ".image_id = " + imageCol + ")"
	return expr, append(args, len(s.TagIDs), s.MaxUsage)
}

// OverlapPercentsAgainst returns each candidate's overlap with the seed
// as a whole percent, keyed by image id and omitting anything that
// shares nothing. Scoped to the ids a page already holds, so the
// aggregate stays bounded by the page size rather than the library.
func OverlapPercentsAgainst(database *db.DB, seedID int64, ids []int64) (map[int64]int, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seed, err := LoadOverlapSeed(database, seedID)
	if err != nil || len(seed.TagIDs) == 0 {
		return nil, err
	}
	tagPlaceholders, tagArgs := db.InPlaceholders(seed.TagIDs)
	idPlaceholders, idArgs := db.InPlaceholders(ids)
	args := append(tagArgs, seed.MaxUsage)
	args = append(args, idArgs...)
	rows, err := database.Read.Query(
		`SELECT it.image_id,
		        sum(CASE WHEN it.tag_id IN (`+tagPlaceholders+`) THEN 1 ELSE 0 END),
		        count(*)`+countedJoin("it")+`
		    AND it.image_id IN (`+idPlaceholders+`)
		  GROUP BY it.image_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]int, len(ids))
	for rows.Next() {
		var id int64
		var shared, counted int
		if err := rows.Scan(&id, &shared, &counted); err != nil {
			return nil, err
		}
		if pct := int(math.Round(OverlapScore(shared, len(seed.TagIDs), counted) * 100)); pct > 0 {
			out[id] = pct
		}
	}
	return out, rows.Err()
}

// SharedTag is one tag both images carry and the weight it contributed
// to their score. Category and Color let a caller render it the way the
// tag reads everywhere else.
type SharedTag struct {
	Name     string
	Category string
	Color    string
	Weight   float64
}

// SharedTags returns what two images have in common, heaviest first,
// capped at limit, plus the total number of shared counted tags. This
// is the evidence behind a tag-similarity score: a pair can share
// forty tags and look nothing alike, so which tags drove the match is
// what makes the score judgeable.
func SharedTags(database *db.DB, a, b int64, limit int) ([]SharedTag, int, error) {
	seed, err := LoadSimilaritySeed(database, a)
	if err != nil || len(seed.Tags) == 0 {
		return nil, 0, err
	}
	weights := make(map[int64]float64, len(seed.Tags))
	for _, t := range seed.Tags {
		weights[int64(t.TagID)] = t.Weight
	}
	placeholders, args := db.InPlaceholders(seed.TagIDs())
	shared, err := db.QueryAll(database.Read, func(rows *sql.Rows) (SharedTag, error) {
		var tagID int64
		var name, category, color string
		if err := rows.Scan(&tagID, &name, &category, &color); err != nil {
			return SharedTag{}, err
		}
		return SharedTag{
			Name:     name,
			Category: category,
			Color:    SafeCategoryColor(color),
			Weight:   weights[tagID],
		}, nil
	},
		`SELECT it.tag_id, t.name, COALESCE(tc.name, ''), COALESCE(tc.color, '')
		   FROM image_tags it
		   JOIN tags t ON t.id = it.tag_id
		   LEFT JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE it.image_id = ? AND it.tag_id IN (`+placeholders+`)`,
		append([]any{b}, args...)...)
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(shared, func(i, j int) bool {
		if shared[i].Weight != shared[j].Weight {
			return shared[i].Weight > shared[j].Weight
		}
		return shared[i].Name < shared[j].Name
	})
	total := len(shared)
	if limit > 0 && total > limit {
		shared = shared[:limit]
	}
	return shared, total, nil
}

// tagWeight is one tag's contribution. A tag on at least as many
// images as the library holds visible says nothing about the subject,
// so it weighs nothing.
func tagWeight(visible, usage int64, category string) float64 {
	if usage <= 0 || usage >= visible {
		return 0
	}
	w := math.Log(float64(visible) / float64(usage))
	if m, ok := categoryWeights[category]; ok {
		w *= m
	}
	return w
}
