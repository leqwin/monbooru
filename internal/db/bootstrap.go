package db

import (
	"fmt"

	_ "embed"
)

// Bootstrap runs the embedded schema.sql on the write pool, then applies
// idempotent column-add migrations for databases that predate a column.
// SQLite has no ADD COLUMN IF NOT EXISTS, so each migration gates itself
// on pragma_table_info.
func Bootstrap(db *DB) error {
	b := &bootstrapper{db: db}
	b.exec("bootstrapping schema", schemaSQL)
	b.ensureColumn("images", "origin", `ALTER TABLE images ADD COLUMN origin TEXT NOT NULL DEFAULT 'ingest'`)
	b.ensureColumn("image_tags", "is_implied", `ALTER TABLE image_tags ADD COLUMN is_implied INTEGER NOT NULL DEFAULT 0`)
	// is_inbox: pre-feature libraries upgrade as fully curated. The column
	// default is 1 (new ingests land in the inbox), but existing rows added
	// before the column existed would all flip to "needs triage" without
	// this one-shot - which would dump the operator's whole library into
	// the inbox view on first boot. The pre-count gate in
	// backfillIfFreshColumn detects the just-added case so the UPDATE
	// only runs then.
	b.backfillIfFreshColumn("images", "is_inbox",
		`ALTER TABLE images ADD COLUMN is_inbox INTEGER NOT NULL DEFAULT 1`,
		`UPDATE images SET is_inbox = 0`,
		"backfill is_inbox=0 on upgrade")
	// Partial seek index for the inbox-count cache and the inbox: filter's
	// fastCountInbox path. Created here rather than in schema.sql because
	// the column is added by ensureColumn above on existing libraries; an
	// index in schema.sql would run before the ALTER and reference a
	// missing column.
	b.exec("create idx_images_inbox_visible", `CREATE INDEX IF NOT EXISTS idx_images_inbox_visible ON images(is_inbox) WHERE is_missing = 0`)
	// idx_image_tags_tag(tag_id) is superseded by
	// idx_image_tags_tag_image(tag_id, image_id) - same leading column,
	// same seek selectivity, plus image_id is now covering. Drop on
	// upgrade so existing libraries don't pay disk and write overhead
	// on a redundant index.
	b.exec("drop superseded idx_image_tags_tag", `DROP INDEX IF EXISTS idx_image_tags_tag`)
	b.ensureColumn("images", "source", `ALTER TABLE images ADD COLUMN source TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("images", "url", `ALTER TABLE images ADD COLUMN url TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("images", "note", `ALTER TABLE images ADD COLUMN note TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("images", "original_source", `ALTER TABLE images ADD COLUMN original_source TEXT NOT NULL DEFAULT ''`)
	// The historical idx_images_source pointed at images(source_type); the
	// name now belongs to the new images(source) column. Drop the old
	// shape unconditionally and let schema.sql / the recreate below
	// rebuild it under both names.
	b.exec("drop legacy idx_images_source", `DROP INDEX IF EXISTS idx_images_source`)
	b.exec("create idx_images_source_type", `CREATE INDEX IF NOT EXISTS idx_images_source_type ON images(source_type)`)
	b.exec("create idx_images_source", `CREATE INDEX IF NOT EXISTS idx_images_source ON images(source)`)
	// Partial visible source index for the source: filter. The NOCASE-
	// collated variant is what the source: filter equality
	// (`source = ? COLLATE NOCASE`) seeks against; the BINARY-collated
	// index stays for the source autocomplete's prefix-range query that
	// needs binary ordering. Both partials are gated only on
	// `is_missing = 0` so SQLite's planner can match the partial WHERE
	// against any source: filter query (a `source != ''` clause in the
	// partial would force the query to also include it, which the
	// executor doesn't emit).
	b.exec("create idx_images_source_visible", `CREATE INDEX IF NOT EXISTS idx_images_source_visible ON images(source) WHERE is_missing = 0`)
	b.exec("create idx_images_source_nocase_visible", `CREATE INDEX IF NOT EXISTS idx_images_source_nocase_visible ON images(source COLLATE NOCASE) WHERE is_missing = 0`)
	// Seed image_sources from the legacy scalar source/url on the first boot
	// after the table appears; the NOT EXISTS guard makes it a no-op once any
	// origin row exists so later boots never re-seed edited rows. The scalars
	// stay as the primary-origin mirror (gallery.SourcesForImage et al.).
	b.exec("seed image_sources from scalar",
		`INSERT OR IGNORE INTO image_sources (image_id, site, post_id, url)
		 SELECT id, source, '', url FROM images
		 WHERE (source != '' OR url != '') AND NOT EXISTS (SELECT 1 FROM image_sources)`)
	b.ensureColumn("image_sources", "commentary", `ALTER TABLE image_sources ADD COLUMN commentary TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("image_sources", "original", `ALTER TABLE image_sources ADD COLUMN original TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("image_sources", "similarity", `ALTER TABLE image_sources ADD COLUMN similarity REAL NOT NULL DEFAULT 0`)
	b.ensureColumn("image_sources", "parent_url", `ALTER TABLE image_sources ADD COLUMN parent_url TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("image_sources", "md5_match", `ALTER TABLE image_sources ADD COLUMN md5_match TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("image_sources", "upgrade_kept", `ALTER TABLE image_sources ADD COLUMN upgrade_kept INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("image_sources", "post_width", `ALTER TABLE image_sources ADD COLUMN post_width INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("image_sources", "post_height", `ALTER TABLE image_sources ADD COLUMN post_height INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("image_sources", "post_size", `ALTER TABLE image_sources ADD COLUMN post_size INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("image_sources", "post_ext", `ALTER TABLE image_sources ADD COLUMN post_ext TEXT NOT NULL DEFAULT ''`)
	// Created here rather than in schema.sql so the ALTER above runs first on
	// libraries that predate the column. Covers the child-side probe of the
	// derivative-edge linking; partial since most origins declare no parent.
	b.exec("create idx_image_sources_parent_url", `CREATE INDEX IF NOT EXISTS idx_image_sources_parent_url ON image_sources(parent_url) WHERE parent_url != ''`)
	// The upgrade gate (internal/upgrade.CandidateWhere), as a partial index
	// so it holds candidate origins only - a handful of rows next to one per
	// origin. The sidebar count and the `upgrade:` filter both seek it.
	b.exec("create idx_image_sources_upgradable", `CREATE INDEX IF NOT EXISTS idx_image_sources_upgradable ON image_sources(image_id)
		WHERE url <> '' AND upgrade_kept = 0 AND (md5_match = 'differ' OR (similarity > 0 AND md5_match = ''))`)
	b.ensureColumn("image_annotations", "manual", `ALTER TABLE image_annotations ADD COLUMN manual INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("images", "page_count", `ALTER TABLE images ADD COLUMN page_count INTEGER`)
	b.ensureColumn("images", "last_read_page", `ALTER TABLE images ADD COLUMN last_read_page INTEGER`)
	b.ensureColumn("images", "duration_seconds", `ALTER TABLE images ADD COLUMN duration_seconds REAL`)
	// Partial visible duration index for the duration: filter. Excludes
	// NULL so non-video rows don't carry an entry.
	b.exec("create idx_images_duration_visible", `CREATE INDEX IF NOT EXISTS idx_images_duration_visible ON images(duration_seconds) WHERE is_missing = 0 AND duration_seconds IS NOT NULL`)
	b.ensureColumn("images", "md5", `ALTER TABLE images ADD COLUMN md5 TEXT NOT NULL DEFAULT ''`)
	// Partial so the rows still waiting on a backfill carry no entry. Not
	// UNIQUE: md5 collisions are cheap to construct, so the constraint
	// would let a prepared pair of files abort an ingest.
	b.exec("create idx_images_md5", `CREATE INDEX IF NOT EXISTS idx_images_md5 ON images(md5) WHERE md5 != ''`)
	// md5_match is the claim compared against the local read. Triggers rather
	// than the write sites: either digest lands from ingest, sync, replace,
	// the lazy fill, the backfill job, every push and enrich and the bulk
	// importer, and one writer forgetting leaves a candidate invisible. Both
	// sides hold their peace while a digest is empty, so a row waiting on its
	// local md5 keeps whatever verdict a fetch recorded. images.md5 is
	// lowercase by construction; the claim is stored as the source sent it.
	b.exec("create trg_image_sources_verdict_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_sources_verdict_ai
		AFTER INSERT ON image_sources
		WHEN NEW.md5 != '' AND (SELECT md5 FROM images WHERE id = NEW.image_id) != ''
		BEGIN
			UPDATE image_sources SET md5_match = CASE
				WHEN lower(NEW.md5) = (SELECT md5 FROM images WHERE id = NEW.image_id) THEN 'match' ELSE 'differ' END
			 WHERE image_id = NEW.image_id AND site = NEW.site AND post_id = NEW.post_id;
		END`)
	// A claim the post never made before is a new question, so the keep
	// ruling lapses with it even when no verdict can be written.
	b.exec("create trg_image_sources_verdict_au", `CREATE TRIGGER IF NOT EXISTS trg_image_sources_verdict_au
		AFTER UPDATE OF md5 ON image_sources
		WHEN NEW.md5 != ''
		BEGIN
			UPDATE image_sources SET upgrade_kept = 0
			 WHERE image_id = NEW.image_id AND site = NEW.site AND post_id = NEW.post_id
			   AND lower(NEW.md5) != lower(OLD.md5);
			UPDATE image_sources SET md5_match = CASE
				WHEN lower(NEW.md5) = (SELECT md5 FROM images WHERE id = NEW.image_id) THEN 'match' ELSE 'differ' END
			 WHERE image_id = NEW.image_id AND site = NEW.site AND post_id = NEW.post_id
			   AND (SELECT md5 FROM images WHERE id = NEW.image_id) != '';
		END`)
	// Only a digest that moved re-opens the question: the lazy cell fill
	// rewrites the same value on every hit, and re-deriving there would read
	// the claim a refused refetch deliberately left behind.
	b.exec("create trg_images_verdict_au", `CREATE TRIGGER IF NOT EXISTS trg_images_verdict_au
		AFTER UPDATE OF md5 ON images
		WHEN NEW.md5 != '' AND NEW.md5 IS NOT OLD.md5
		BEGIN
			UPDATE image_sources SET md5_match = CASE
				WHEN lower(md5) = NEW.md5 THEN 'match' ELSE 'differ' END
			 WHERE image_id = NEW.id AND md5 != '';
		END`)
	// VIRTUAL generated column over the lowercased filename basename so
	// the name: filter and the system:name autocomplete seek a single
	// indexed string instead of running lower(basename(canonical_path))
	// per row of a full canonical_path scan. STORED isn't reachable via
	// ALTER TABLE; VIRTUAL keeps the value computed on read but lets the
	// matching index materialise it once per row at index-write time, so
	// the seek is the same cost as a real column.
	b.ensureColumn("images", "basename_lower",
		`ALTER TABLE images ADD COLUMN basename_lower TEXT GENERATED ALWAYS AS (lower(basename(canonical_path))) VIRTUAL`)
	b.exec("create idx_images_basename_lower_visible", `CREATE INDEX IF NOT EXISTS idx_images_basename_lower_visible ON images(basename_lower) WHERE is_missing = 0 AND basename_lower != ''`)
	// Mirror of images.basename_lower for alias paths so the name:
	// filter's EXISTS over image_paths reads `ip.basename_lower`
	// directly instead of running lower(basename(ip.path)) per alias
	// row. VIRTUAL so the value is computed on read; the EXISTS
	// subquery rides idx_image_paths_aliases (image_id WHERE
	// is_canonical = 0) to skip every canonical row, which is the
	// other half of the per-row cost.
	b.ensureColumn("image_paths", "basename_lower",
		`ALTER TABLE image_paths ADD COLUMN basename_lower TEXT GENERATED ALWAYS AS (lower(basename(path))) VIRTUAL`)
	b.ensureColumn("images", "series", `ALTER TABLE images ADD COLUMN series TEXT NOT NULL DEFAULT ''`)
	// Operator-edited per-image position within its series. NULL means
	// "no specific order" - the search executor sorts those after rows
	// with a numeric position when a series: filter pins the result set.
	b.ensureColumn("images", "series_order", `ALTER TABLE images ADD COLUMN series_order INTEGER`)
	b.exec("create idx_images_series", `CREATE INDEX IF NOT EXISTS idx_images_series ON images(series) WHERE series != ''`)
	// NOCASE-collated companion for the collection: filter equality
	// (`series = ? COLLATE NOCASE`); the BINARY index above stays for
	// the collection-autocomplete prefix-range query that needs binary
	// ordering. The NOCASE partial is gated only on the visibility
	// filter the executor emits (no `series != ''` clause), so SQLite
	// can match the partial WHERE against any collection: query.
	b.exec("create idx_images_series_nocase", `CREATE INDEX IF NOT EXISTS idx_images_series_nocase ON images(series COLLATE NOCASE)`)
	// Seed image_collections from the legacy single-collection columns on
	// the first boot after the table appears. The NOT EXISTS guard makes
	// it a no-op once any membership row exists, so later boots never
	// re-seed rows the operator has since edited.
	b.exec("seed image_collections from series",
		`INSERT OR IGNORE INTO image_collections (image_id, name, position)
		 SELECT id, series, series_order FROM images
		 WHERE series != '' AND NOT EXISTS (SELECT 1 FROM image_collections)`)
	// NOCASE-collated companion for folder: equality - same shape as
	// idx_images_folder_visible (already partial WHERE is_missing = 0)
	// but with the COLLATE NOCASE that the folder: filter uses, so the
	// equality leg of the (path = ? COLLATE NOCASE OR path LIKE ...)
	// composite predicate can ride an indexed seek instead of falling
	// back to idx_images_missing + TEMP B-TREE FOR ORDER BY.
	b.exec("create idx_images_folder_nocase_visible", `CREATE INDEX IF NOT EXISTS idx_images_folder_nocase_visible ON images(folder_path COLLATE NOCASE) WHERE is_missing = 0`)
	// Saved-search reproduces the URL the operator was looking at; the
	// seed bit lets a `random` save reopen at the same shuffle. `sort_order`
	// is the URL's `order` value - column name is suffixed because `order`
	// is a SQLite reserved word that breaks plain UPDATE/INSERT statements
	// even with quoting in some driver paths.
	b.ensureColumn("saved_searches", "sort", `ALTER TABLE saved_searches ADD COLUMN sort TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("saved_searches", "sort_order", `ALTER TABLE saved_searches ADD COLUMN sort_order TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("saved_searches", "seed", `ALTER TABLE saved_searches ADD COLUMN seed TEXT NOT NULL DEFAULT ''`)
	b.ensureColumn("image_paths", "mtime_unix", `ALTER TABLE image_paths ADD COLUMN mtime_unix INTEGER NOT NULL DEFAULT 0`)
	// The same stamp at full resolution. Seconds cannot tell an edit that
	// landed in the same second the file was last observed, and 0 keeps a
	// row written before this column on the second-grained comparison
	// instead of re-hashing every file in the library once on upgrade.
	b.ensureColumn("image_paths", "mtime_nsec", `ALTER TABLE image_paths ADD COLUMN mtime_nsec INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("images", "phash", `ALTER TABLE images ADD COLUMN phash INTEGER`)
	// Which detector queued a pair, and the tag score behind it. The
	// default keeps every row an existing library already holds on the
	// only source that could have created it.
	b.ensureColumn("potential_relation_pairs", "source", `ALTER TABLE potential_relation_pairs ADD COLUMN source TEXT NOT NULL DEFAULT 'phash'`)
	b.ensureColumn("potential_relation_pairs", "score", `ALTER TABLE potential_relation_pairs ADD COLUMN score REAL`)
	// The stored collection opt-out verdict (see schema.sql). Backfilled
	// once on upgrade; the triggers below keep it current after that.
	b.backfillIfFreshColumn("potential_relation_pairs", "collection_hidden",
		`ALTER TABLE potential_relation_pairs ADD COLUMN collection_hidden INTEGER NOT NULL DEFAULT 0`,
		`UPDATE potential_relation_pairs SET collection_hidden = 1 WHERE `+pairHiddenProbe("a_image_id", "b_image_id"),
		"backfill potential_relation_pairs.collection_hidden")
	b.ensureColumn("relation_session", "detector", `ALTER TABLE relation_session ADD COLUMN detector TEXT NOT NULL DEFAULT 'both'`)
	// A group below two members is not a group. Bulk deletes that predate
	// the relations hook cascaded the member rows away without dissolving
	// the group row, and an import replays whatever the export carried.
	b.exec("dissolve degenerate dup groups",
		`DELETE FROM dup_groups WHERE (SELECT COUNT(*) FROM dup_group_members m WHERE m.group_id = dup_groups.id) < 2`)
	b.exec("dissolve degenerate alt groups",
		`DELETE FROM alt_groups WHERE (SELECT COUNT(*) FROM alt_group_members m WHERE m.group_id = alt_groups.id) < 2`)
	// Per-upload batch token stamped on web-UI uploads; NULL elsewhere. No
	// index - it is only read for the page of rows the inbox cluster view
	// already loaded, never filtered or sorted on.
	b.ensureColumn("images", "upload_batch", `ALTER TABLE images ADD COLUMN upload_batch INTEGER`)
	// The operator's per-image opt-out from the scheduled lookup, one
	// column per backend so the per-image choice matches the per-phase one
	// in Settings. 1 for existing rows: opting out is the exception, and
	// nothing runs until a Schedule checkbox is on anyway. The PTR column
	// is seeded from the single flag it splits off, so an opt-out made
	// before the split keeps covering both backends.
	b.ensureColumn("images", "scheduled_lookup", `ALTER TABLE images ADD COLUMN scheduled_lookup INTEGER NOT NULL DEFAULT 1`)
	b.backfillIfFreshColumn("images", "scheduled_lookup_ptr",
		`ALTER TABLE images ADD COLUMN scheduled_lookup_ptr INTEGER NOT NULL DEFAULT 1`,
		`UPDATE images SET scheduled_lookup_ptr = scheduled_lookup`,
		"backfill images.scheduled_lookup_ptr")
	// Partial phash index: drives `phash:<hex>` exact-match seeks and
	// the cold-path SELECT that loads the BK-tree at first relations
	// query. Skips NULL rows (the BK-tree only carries computed phashes)
	// so a half-backfilled library doesn't pay the storage for unhashed
	// entries.
	b.exec("create idx_images_phash", `CREATE INDEX IF NOT EXISTS idx_images_phash ON images(phash) WHERE phash IS NOT NULL`)
	// Stored tag_count column maintained by triggers on image_tags so
	// the tagcount: filter rides an indexed range seek instead of a
	// correlated `SELECT COUNT(*) FROM image_tags WHERE image_id = i.id`
	// per visible row. Backfilled once on first boot after the column
	// is added; the triggers below keep it in lockstep with every
	// image_tags insert/delete.
	b.backfillIfFreshColumn("images", "tag_count",
		`ALTER TABLE images ADD COLUMN tag_count INTEGER NOT NULL DEFAULT 0`,
		`UPDATE images SET tag_count = (SELECT COUNT(*) FROM image_tags WHERE image_id = images.id)`,
		"backfill images.tag_count")
	b.exec("create idx_images_tag_count_visible", `CREATE INDEX IF NOT EXISTS idx_images_tag_count_visible ON images(tag_count) WHERE is_missing = 0`)
	// Partial covering index for `mime:` / `type:` so the planner can
	// seek `i.file_type IN (...)` instead of scanning every visible row.
	// The bucket is small (jpeg, png, webp, gif, mp4, webm, cbz) so the
	// index stays compact even at large scale.
	b.exec("create idx_images_file_type_visible", `CREATE INDEX IF NOT EXISTS idx_images_file_type_visible ON images(file_type) WHERE is_missing = 0`)
	// Maintain images.tag_count with row-level triggers. MAX(0, ...) on
	// the delete trigger guards against the impossible-but-cheap case of
	// a negative count from a torn upgrade. The triggers are FOR EACH
	// ROW (SQLite's only mode) so a batch INSERT/DELETE on image_tags
	// fires one UPDATE per affected image; the per-row cost is a primary-
	// key seek on images plus an indexed update.
	b.exec("create trg_image_tags_count_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_count_ai
		AFTER INSERT ON image_tags
		BEGIN
			UPDATE images SET tag_count = tag_count + 1 WHERE id = NEW.image_id;
		END`)
	b.exec("create trg_image_tags_count_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_count_ad
		AFTER DELETE ON image_tags
		BEGIN
			UPDATE images SET tag_count = MAX(0, tag_count - 1) WHERE id = OLD.image_id;
		END`)
	// Per-label visible-member counts maintained by triggers, so the
	// collections listing and counts read one row per label instead of
	// scanning every membership with a per-row visibility probe. Rows
	// decremented to 0 stay (filtered by visible_count > 0 on read);
	// the label population is small so they never add up to anything.
	b.backfillIfFreshTable("collection_counts",
		`CREATE TABLE IF NOT EXISTS collection_counts (
			name          TEXT PRIMARY KEY COLLATE NOCASE,
			visible_count INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO collection_counts (name, visible_count)
		 SELECT c.name, SUM(EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0))
		 FROM image_collections c GROUP BY c.name`,
		"backfill collection_counts")
	// Membership-side triggers cover insert / delete / relabel; the WHEN
	// visibility probe keeps memberships of missing images out of the
	// count, matching the listing's is_missing = 0 filter.
	b.exec("create trg_image_collections_count_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_count_ai
		AFTER INSERT ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = NEW.image_id AND i.is_missing = 0)
		BEGIN
			INSERT INTO collection_counts (name, visible_count) VALUES (NEW.name, 1)
			ON CONFLICT(name) DO UPDATE SET visible_count = visible_count + 1;
		END`)
	b.exec("create trg_image_collections_count_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_count_ad
		AFTER DELETE ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = OLD.image_id AND i.is_missing = 0)
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1) WHERE name = OLD.name;
		END`)
	b.exec("create trg_image_collections_count_au", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_count_au
		AFTER UPDATE OF name ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = NEW.image_id AND i.is_missing = 0)
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1) WHERE name = OLD.name;
			INSERT INTO collection_counts (name, visible_count) VALUES (NEW.name, 1)
			ON CONFLICT(name) DO UPDATE SET visible_count = visible_count + 1;
		END`)
	// Image-side triggers cover visibility flips and deletes. The delete
	// trigger must run BEFORE so the memberships are still readable; the
	// FK cascade then removes them with the parent row already gone, so
	// the membership delete trigger's WHEN probe fails and the pair
	// can't double-decrement.
	b.exec("create trg_images_collection_count_hide", `CREATE TRIGGER IF NOT EXISTS trg_images_collection_count_hide
		AFTER UPDATE OF is_missing ON images
		WHEN OLD.is_missing = 0 AND NEW.is_missing != 0
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1)
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = NEW.id);
		END`)
	b.exec("create trg_images_collection_count_show", `CREATE TRIGGER IF NOT EXISTS trg_images_collection_count_show
		AFTER UPDATE OF is_missing ON images
		WHEN OLD.is_missing != 0 AND NEW.is_missing = 0
		BEGIN
			UPDATE collection_counts SET visible_count = visible_count + 1
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = NEW.id);
		END`)
	b.exec("create trg_images_collection_count_bd", `CREATE TRIGGER IF NOT EXISTS trg_images_collection_count_bd
		BEFORE DELETE ON images
		WHEN OLD.is_missing = 0
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1)
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = OLD.id);
		END`)
	// Maintain potential_relation_pairs.collection_hidden. The insert
	// trigger stamps new rows (WHEN keeps the common visible case to a
	// probe with no write); the membership and opt-in triggers resweep
	// the affected images' rows, one indexed UPDATE per pair side. The
	// tag-pass admission probe applies the same rule before queueing
	// (internal/relations/job_tagpairs.go).
	b.exec("create trg_potential_pairs_hidden_ai", `CREATE TRIGGER IF NOT EXISTS trg_potential_pairs_hidden_ai
		AFTER INSERT ON potential_relation_pairs
		WHEN `+pairHiddenProbe("NEW.a_image_id", "NEW.b_image_id")+`
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = 1
			WHERE a_image_id = NEW.a_image_id AND b_image_id = NEW.b_image_id;
		END`)
	b.exec("create trg_image_collections_pairs_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_pairs_ai
		AFTER INSERT ON image_collections
		BEGIN
			`+pairsResweepBody("a_image_id = NEW.image_id", "b_image_id = NEW.image_id")+`
		END`)
	b.exec("create trg_image_collections_pairs_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_pairs_ad
		AFTER DELETE ON image_collections
		BEGIN
			`+pairsResweepBody("a_image_id = OLD.image_id", "b_image_id = OLD.image_id")+`
		END`)
	b.exec("create trg_image_collections_pairs_au", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_pairs_au
		AFTER UPDATE OF name ON image_collections
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = `+pairHiddenProbe("a_image_id", "b_image_id")+`
			WHERE a_image_id = NEW.image_id;
			UPDATE potential_relation_pairs SET collection_hidden = `+pairHiddenProbe("a_image_id", "b_image_id")+`
			WHERE b_image_id = NEW.image_id;
		END`)
	b.exec("create trg_collection_find_relations_pairs_ai", `CREATE TRIGGER IF NOT EXISTS trg_collection_find_relations_pairs_ai
		AFTER INSERT ON collection_find_relations
		BEGIN
			`+pairsResweepBody("a_image_id IN (SELECT image_id FROM image_collections WHERE name = NEW.name)", "b_image_id IN (SELECT image_id FROM image_collections WHERE name = NEW.name)")+`
		END`)
	b.exec("create trg_collection_find_relations_pairs_ad", `CREATE TRIGGER IF NOT EXISTS trg_collection_find_relations_pairs_ad
		AFTER DELETE ON collection_find_relations
		BEGIN
			`+pairsResweepBody("a_image_id IN (SELECT image_id FROM image_collections WHERE name = OLD.name)", "b_image_id IN (SELECT image_id FROM image_collections WHERE name = OLD.name)")+`
		END`)
	// Reading-order index for per-collection member walks (preview
	// samples, the reorder dialog). The `position IS NULL` expression
	// key mirrors the ORDER BY exactly, so a LIMIT stops after the
	// first visible members instead of sorting the whole label.
	b.exec("create idx_image_collections_reading", `CREATE INDEX IF NOT EXISTS idx_image_collections_reading
		ON image_collections(name, position IS NULL, position, image_id)`)
	// Stored rating_rank column maintained by triggers on image_tags. The
	// SFW cookie ceiling AND-chains three NOT EXISTS subqueries (one per
	// excluded rating tag) onto every search expression, which the deep-
	// gallery cursor walks per visible row; reading a single integer
	// column with a covering partial index collapses that chain to one
	// indexed range. -1 sentinel for "no rating tag" lets the ceiling
	// predicate stay `rating_rank <= ?` without an OR-NULL clause:
	// unrated rows pass every ceiling because -1 is below every
	// documented level (general=0, sensitive=1, questionable=2,
	// explicit=3). PruneLowerRatingsTx and the autotagger uphold
	// at-most-one-rating-per-image, so the column carries the single
	// rank that survived; pre-existing rows with multiple ratings get
	// the MAX during backfill, matching the search engine's "highest-
	// wins" semantics.
	b.backfillIfFreshColumn("images", "rating_rank",
		`ALTER TABLE images ADD COLUMN rating_rank INTEGER NOT NULL DEFAULT -1`,
		`UPDATE images SET rating_rank = `+ratingRankExpr("images.id")+``,
		"backfill images.rating_rank")
	// Tag provenance: origin is stamped once at creation with the label
	// of whatever created the row (user, a booru site, ptr, an
	// auto-tagger, an import label); last_used_at tracks the most recent
	// application to an image. The backfills label pre-existing rows
	// from their image_tags attribution: the exact tagger_name when
	// every row agrees on one non-empty label, else the coarse bucket
	// the derived origin badge used to report, and the newest row's
	// created_at for last_used_at. Zero-usage rows (aliases included)
	// stay '' - there is nothing to derive from.
	b.backfillIfFreshColumn("tags", "origin",
		`ALTER TABLE tags ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
		`UPDATE tags SET origin = COALESCE((
			SELECT CASE
				WHEN COUNT(*) = 0 THEN NULL
				WHEN COUNT(DISTINCT COALESCE(it.tagger_name, '')) = 1
				     AND MAX(COALESCE(it.tagger_name, '')) <> '' THEN MAX(it.tagger_name)
				WHEN SUM(it.is_auto = 0) = 0 THEN 'auto'
				WHEN SUM(it.is_auto = 0 AND COALESCE(it.tagger_name, '') = '') = 0 THEN 'api'
				ELSE 'user'
			END
			FROM image_tags it WHERE it.tag_id = tags.id), '')`,
		"backfill tags.origin")
	b.backfillIfFreshColumn("tags", "last_used_at",
		`ALTER TABLE tags ADD COLUMN last_used_at TEXT`,
		`UPDATE tags SET last_used_at = (SELECT MAX(it.created_at) FROM image_tags it WHERE it.tag_id = tags.id)`,
		"backfill tags.last_used_at")
	b.ensureColumn("tag_implications", "origin",
		`ALTER TABLE tag_implications ADD COLUMN origin TEXT NOT NULL DEFAULT ''`)
	// stale: the attributed source's latest refresh no longer carried the
	// row; it stays until the operator acts, rendered in a "stale" group.
	b.ensureColumn("image_tags", "stale",
		`ALTER TABLE image_tags ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("tags", "stale",
		`ALTER TABLE tags ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("tag_implications", "stale",
		`ALTER TABLE tag_implications ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`)
	// Partial indexes over the small stale-row slice. The tag-leading one
	// serves the /tags per-row stale counts and stale:<tag>; the image one
	// serves stale:any / stale:none.
	b.exec("create idx_image_tags_stale_tag",
		`CREATE INDEX IF NOT EXISTS idx_image_tags_stale_tag ON image_tags(tag_id, image_id) WHERE stale = 1`)
	b.exec("create idx_image_tags_stale_image",
		`CREATE INDEX IF NOT EXISTS idx_image_tags_stale_image ON image_tags(image_id) WHERE stale = 1`)
	// Two /tags sidebar counts scan the whole catalog on every render.
	// idx_tags_active_name is covering for the conflicts scan - it carries
	// name alone, which is why ConflictsCount counts rows rather than
	// distinct categories - and idx_tags_origin covers both the origin
	// histogram and the origin= filter.
	b.exec("create idx_tags_active_name",
		`CREATE INDEX IF NOT EXISTS idx_tags_active_name ON tags(name) WHERE is_alias = 0`)
	b.exec("create idx_tags_origin",
		`CREATE INDEX IF NOT EXISTS idx_tags_origin ON tags(origin)`)
	// The created_at sort has no other key to seek on, so without this
	// index its listing temp-sorts the catalog. last_used_at gets none
	// deliberately: every tag application rewrites it, and the sort is
	// cheap enough without one.
	b.exec("create idx_tags_active_created",
		`CREATE INDEX IF NOT EXISTS idx_tags_active_created ON tags(created_at DESC) WHERE is_alias = 0`)
	// Per-tag source ledger: one row per (image, tag, source) so a tag
	// several sources agree on shows every confirmation, not just the
	// first-wins image_tags.tagger_name. Write paths record through
	// tags.RecordTagSourceTx (including re-confirmations of an existing
	// row); implied fan-out rows record nothing - their provenance is
	// the implication edge. The backfill derives the single source each
	// existing row can attest ('user' when tagger_name is empty).
	b.backfillIfFreshTable("image_tag_sources",
		`CREATE TABLE IF NOT EXISTS image_tag_sources (
			image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
			tag_id     INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
			source     TEXT    NOT NULL,
			created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
			PRIMARY KEY (image_id, tag_id, source)
		)`,
		`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source, created_at)
		 SELECT it.image_id, it.tag_id, COALESCE(NULLIF(it.tagger_name, ''), 'user'), it.created_at
		 FROM image_tags it
		 JOIN images i ON i.id = it.image_id
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.is_implied = 0`,
		"backfill image_tag_sources from tagger_name")
	// The PK leads on image_id, so every read keyed by source scans the
	// table. This one lets the /tags Used-by surfaces ride index seeks:
	// the label list off a skip-ahead DISTINCT, the per-row chips off one
	// EXISTS probe per (tag, label), the filter off a covering lookup.
	b.exec("create idx_image_tag_sources_source",
		`CREATE INDEX IF NOT EXISTS idx_image_tag_sources_source ON image_tag_sources(source, tag_id)`)
	// Removing a tag from an image drops its ledger rows. A trigger
	// instead of per-call cleanup because image_tags rows die through
	// many shapes (single remove, implied sweep, batch strip, tag
	// delete, FK cascade) and the ledger must never outlive its row.
	b.exec("create trg_image_tags_sources_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_sources_ad
		AFTER DELETE ON image_tags
		BEGIN
			DELETE FROM image_tag_sources WHERE image_id = OLD.image_id AND tag_id = OLD.tag_id;
		END`)
	// Partial covering index for `fav:true` searches. The bare
	// idx_images_favorited (CREATE in schema.sql) covers both polarities
	// but isn't partial; the planner under c=5 sometimes prefers
	// idx_images_missing and pays a TEMP B-TREE for the cursor sort.
	// The favorited subset is small (low single-percent on a typical
	// library) so the composite (ingested_at, id) tail lets the data
	// SELECT walk favorited matches in sort order with no temp sort.
	b.exec("create idx_images_favorited_visible", `CREATE INDEX IF NOT EXISTS idx_images_favorited_visible ON images(ingested_at DESC, id DESC) WHERE is_missing = 0 AND is_favorited = 1`)
	// Covering partial sort indexes that include rating_rank as a tail
	// key. The ceiling chain rewrite emits `i.rating_rank <= ?` as the
	// only non-cursor predicate; with rating_rank inlined in the index
	// the deep-gallery cursor walks (ingested_at, id) in order and
	// filters rating_rank without dropping back to the table. The
	// matching filesize-sort companion covers `back_sort=filesize` on
	// the same ceiling path. Both are partial WHERE is_missing = 0 so
	// the entries match the gallery's visible-only filter.
	b.exec("create idx_images_ingested_rating_visible", `CREATE INDEX IF NOT EXISTS idx_images_ingested_rating_visible ON images(ingested_at DESC, id DESC, rating_rank) WHERE is_missing = 0`)
	b.exec("create idx_images_filesize_rating_visible", `CREATE INDEX IF NOT EXISTS idx_images_filesize_rating_visible ON images(file_size DESC, id DESC, rating_rank) WHERE is_missing = 0`)
	// Standalone covering partial index on the effective rating rank so
	// the unfiltered-ceiling count rides a `rating_rank <= ?` range seek
	// instead of scanning every visible row. The sort indexes above carry
	// rating_rank only as a trailing key, which the planner can't seek on
	// for a bare rank predicate.
	b.exec("create idx_images_rating_rank_visible", `CREATE INDEX IF NOT EXISTS idx_images_rating_rank_visible ON images(rating_rank) WHERE is_missing = 0`)
	// Maintain images.rating_rank with row-level triggers. The WHEN
	// subquery short-circuits the trigger to rating-category writes only
	// so per-image_tags inserts and deletes for non-rating tags stay free.
	// Each fire recomputes MAX over the image's remaining rating tags;
	// on the insert side that includes the just-added row, on the delete
	// side it excludes the just-deleted row, mirroring the highest-wins
	// invariant PruneLowerRatingsTx enforces at write time.
	b.exec("create trg_image_tags_rating_rank_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_rating_rank_ai
		AFTER INSERT ON image_tags
		WHEN NEW.tag_id IN (SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating')
		BEGIN
			UPDATE images SET rating_rank = `+ratingRankExpr("NEW.image_id")+` WHERE id = NEW.image_id;
		END`)
	b.exec("create trg_image_tags_rating_rank_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_rating_rank_ad
		AFTER DELETE ON image_tags
		WHEN OLD.tag_id IN (SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating')
		BEGIN
			UPDATE images SET rating_rank = `+ratingRankExpr("OLD.image_id")+` WHERE id = OLD.image_id;
		END`)
	// potential_relation_pairs.max_rating_rank mirrors the higher of
	// the two members' rating_rank so the ceiling-aware queue reads
	// compare one stored integer instead of probing image_tags twice
	// per row. Stamped on insert; when a rating edit moves a member's
	// rating_rank, the image_tags triggers above cascade into the
	// images-side trigger below. Sits after the rating_rank block so a
	// from-scratch upgrade has the column before these reference it.
	b.backfillIfFreshColumn("potential_relation_pairs", "max_rating_rank",
		`ALTER TABLE potential_relation_pairs ADD COLUMN max_rating_rank INTEGER NOT NULL DEFAULT -1`,
		`UPDATE potential_relation_pairs SET max_rating_rank = max(
			(SELECT rating_rank FROM images WHERE id = a_image_id),
			(SELECT rating_rank FROM images WHERE id = b_image_id))`,
		"backfill potential_relation_pairs.max_rating_rank")
	b.exec("create trg_potential_pairs_rank_ai", `CREATE TRIGGER IF NOT EXISTS trg_potential_pairs_rank_ai
		AFTER INSERT ON potential_relation_pairs
		BEGIN
			UPDATE potential_relation_pairs SET max_rating_rank = max(
				(SELECT rating_rank FROM images WHERE id = NEW.a_image_id),
				(SELECT rating_rank FROM images WHERE id = NEW.b_image_id))
			WHERE a_image_id = NEW.a_image_id AND b_image_id = NEW.b_image_id;
		END`)
	b.exec("create trg_images_pairs_rank_au", `CREATE TRIGGER IF NOT EXISTS trg_images_pairs_rank_au
		AFTER UPDATE OF rating_rank ON images
		BEGIN
			UPDATE potential_relation_pairs SET max_rating_rank = max(
				NEW.rating_rank,
				(SELECT rating_rank FROM images WHERE id = b_image_id))
			WHERE a_image_id = NEW.id;
			UPDATE potential_relation_pairs SET max_rating_rank = max(
				NEW.rating_rank,
				(SELECT rating_rank FROM images WHERE id = a_image_id))
			WHERE b_image_id = NEW.id;
		END`)
	// FTS5 trigram virtual tables for the name: filter. The outer leg's
	// `i.basename_lower LIKE '%val%'` cannot ride the existing
	// idx_images_basename_lower_visible index because of the leading
	// wildcard, so the planner walks every visible row. With the
	// trigram tokenizer the LIKE-substring search becomes an index
	// lookup (rowid = the canonical or alias path id). Queries shorter
	// than 3 characters still fall back to the LIKE shape because the
	// trigram tokenizer needs at least 3 characters of overlap to seek.
	b.exec("create image_basename_canonical_fts", `CREATE VIRTUAL TABLE IF NOT EXISTS image_basename_canonical_fts USING fts5(basename, tokenize='trigram', content='', contentless_delete=1)`)
	b.exec("create image_basename_alias_fts", `CREATE VIRTUAL TABLE IF NOT EXISTS image_basename_alias_fts USING fts5(basename, image_id UNINDEXED, tokenize='trigram', content='', contentless_delete=1)`)
	// Backfill on the same version-gate as ANALYZE so a partial backfill
	// from a torn upgrade gets retried until the marker advances. The
	// trigram FTS is contentless so re-inserting a (rowid, basename)
	// pair after a DELETE FROM stays cheap; clear stale entries first.
	// Resolve b.err before reading user_version so a failed earlier
	// exec doesn't query against a half-bootstrapped DB.
	if b.err != nil {
		return b.err
	}
	var ratingRankUserVersion int
	if err := db.Write.QueryRow(`PRAGMA user_version`).Scan(&ratingRankUserVersion); err != nil {
		return fmt.Errorf("read user_version (fts5 backfill): %w", err)
	}
	// Pinned to the version that introduced them so a later marker bump
	// can't re-run them over pairs find-pairs has produced since.
	if ratingRankUserVersion < 13 {
		// The triggers only see writes from here on, so a library whose two
		// digests are already stored gets its verdicts in one pass. Rows a
		// fetch already ruled on are left alone - see trg_images_verdict_au.
		b.exec("backfill source md5 verdicts", `UPDATE image_sources SET md5_match = CASE
			WHEN lower(md5) = (SELECT md5 FROM images WHERE id = image_id) THEN 'match' ELSE 'differ' END
		 WHERE md5_match = '' AND md5 != '' AND (SELECT md5 FROM images WHERE id = image_id) != ''`)
	}
	if ratingRankUserVersion < 12 {
		// Tag rows an earlier metric admitted cannot be re-thresholded into
		// the current one; drop them and let the next find-pairs run
		// requeue. Both-detector rows keep their pixel evidence.
		b.exec("drop stale tag-scored pairs", `DELETE FROM potential_relation_pairs WHERE source = 'tags'`)
		b.exec("demote stale both-detector pairs", `UPDATE potential_relation_pairs SET source = 'phash', score = NULL WHERE source = 'both'`)
	}
	if ratingRankUserVersion < 11 {
		// basename() splits on both separators now, so keys materialised
		// under the old body are stale - and the backfill below reads
		// basename_lower straight back out.
		b.exec("reindex basename_lower", `REINDEX idx_images_basename_lower_visible`)
		b.exec("clear image_basename_canonical_fts", `DELETE FROM image_basename_canonical_fts`)
		b.exec("backfill image_basename_canonical_fts", `INSERT INTO image_basename_canonical_fts (rowid, basename)
			SELECT id, basename_lower FROM images WHERE basename_lower != ''`)
		b.exec("clear image_basename_alias_fts", `DELETE FROM image_basename_alias_fts`)
		b.exec("backfill image_basename_alias_fts", `INSERT INTO image_basename_alias_fts (rowid, basename, image_id)
			SELECT id, basename_lower, image_id FROM image_paths WHERE is_canonical = 0 AND basename_lower != ''`)
		b.exec("normalize windows folder_path", NormalizeWindowsFolderPathSQL)
	}
	// Triggers maintain the FTS5 tables in lockstep with the source rows.
	// canonical_path's basename_lower is a VIRTUAL generated column;
	// SQLite resolves NEW.basename_lower / OLD.basename_lower per trigger
	// fire. The rowid alignment (canonical = images.id, alias =
	// image_paths.id) keeps every UPDATE / DELETE O(1) on the FTS rowid.
	// `INSERT OR REPLACE` collapses the (rowid) primary-key conflict on
	// ALTER-style updates that don't change the column but do retrigger
	// the OF clause (rare; defensive).
	b.exec("create trg_image_basename_canonical_fts_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_ai
		AFTER INSERT ON images
		WHEN NEW.basename_lower != ''
		BEGIN
			INSERT OR REPLACE INTO image_basename_canonical_fts (rowid, basename) VALUES (NEW.id, NEW.basename_lower);
		END`)
	b.exec("create trg_image_basename_canonical_fts_au", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_au
		AFTER UPDATE OF canonical_path ON images
		BEGIN
			DELETE FROM image_basename_canonical_fts WHERE rowid = OLD.id;
			INSERT INTO image_basename_canonical_fts (rowid, basename) SELECT NEW.id, NEW.basename_lower WHERE NEW.basename_lower != '';
		END`)
	b.exec("create trg_image_basename_canonical_fts_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_ad
		AFTER DELETE ON images
		BEGIN
			DELETE FROM image_basename_canonical_fts WHERE rowid = OLD.id;
		END`)
	b.exec("create trg_image_basename_alias_fts_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_ai
		AFTER INSERT ON image_paths
		WHEN NEW.is_canonical = 0 AND NEW.basename_lower != ''
		BEGIN
			INSERT OR REPLACE INTO image_basename_alias_fts (rowid, basename, image_id) VALUES (NEW.id, NEW.basename_lower, NEW.image_id);
		END`)
	b.exec("create trg_image_basename_alias_fts_au", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_au
		AFTER UPDATE OF path, is_canonical ON image_paths
		BEGIN
			DELETE FROM image_basename_alias_fts WHERE rowid = OLD.id;
			INSERT INTO image_basename_alias_fts (rowid, basename, image_id)
				SELECT NEW.id, NEW.basename_lower, NEW.image_id
				WHERE NEW.is_canonical = 0 AND NEW.basename_lower != '';
		END`)
	b.exec("create trg_image_basename_alias_fts_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_ad
		AFTER DELETE ON image_paths
		BEGIN
			DELETE FROM image_basename_alias_fts WHERE rowid = OLD.id;
		END`)
	// ANALYZE only when the schema marker says this version's migrations
	// haven't been analyzed yet. PRAGMA optimize then handles row-count
	// drift on steady-state restarts. Without the gate, ANALYZE on the
	// large image_tags indexes costs ~30 s on a cold OS page cache every
	// boot and blows the coldstart budget; the new partial indexes that
	// pragma optimize misses (idx_images_inbox_visible / idx_images_source
	// / idx_images_series and the rebuilt idx_image_tags_tag_image)
	// instead get their sqlite_stat1 entries once, on the upgrade boot.
	// analysis_limit=400 is the SQLite-recommended sample cap; the
	// resulting stats are accurate enough for plan choice and keep the
	// one-time pass under a second when the DB is warm.
	if b.err != nil {
		return b.err
	}
	var userVersion int
	if err := db.Write.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if userVersion < bootstrapSchemaVersion {
		b.exec("set analysis_limit", `PRAGMA analysis_limit = 400`)
		b.exec("analyze images", `ANALYZE images`)
		b.exec("analyze image_tags", `ANALYZE image_tags`)
		b.exec("set user_version", fmt.Sprintf(`PRAGMA user_version = %d`, bootstrapSchemaVersion))
	}
	b.exec("pragma optimize", `PRAGMA optimize`)
	return b.err
}

// bootstrapper threads the first migration error through a long
// sequence of calls so each migration step lands as one statement.
// Same shape as jsonWriter in internal/galleryio/io.go.
type bootstrapper struct {
	db  *DB
	err error
}

func (b *bootstrapper) exec(label, sql string) {
	if b.err != nil {
		return
	}
	if _, err := b.db.Write.Exec(sql); err != nil {
		b.err = fmt.Errorf("%s: %w", label, err)
	}
}

// ratingRankExpr is the highest-wins rating rank of one image, read off
// its rating-category tags. imageIDCol names the row under test: the
// backfill correlates to images.id, the triggers to NEW / OLD.
func ratingRankExpr(imageIDCol string) string {
	return `COALESCE((
				SELECT MAX(CASE t.name
					WHEN 'general' THEN 0
					WHEN 'sensitive' THEN 1
					WHEN 'questionable' THEN 2
					WHEN 'explicit' THEN 3
					ELSE -1 END)
				FROM image_tags it
				JOIN tags t ON t.id = it.tag_id
				JOIN tag_categories tc ON tc.id = t.category_id
				WHERE it.image_id = ` + imageIDCol + ` AND tc.name = 'rating'
			), -1)`
}

// pairsResweepBody re-derives collection_hidden for every queue pair
// the changed collection row can reach, once from each side of the
// pair. predA / predB select those rows.
func pairsResweepBody(predA, predB string) string {
	return `UPDATE potential_relation_pairs SET collection_hidden = ` + pairHiddenProbe("a_image_id", "b_image_id") + `
			WHERE ` + predA + `;
			UPDATE potential_relation_pairs SET collection_hidden = ` + pairHiddenProbe("a_image_id", "b_image_id") + `
			WHERE ` + predB + `;`
}

// pairHiddenProbe returns the EXISTS clause deciding a queue pair's
// collection_hidden flag: true when the two images share a collection
// that has not opted into relation finding. aCol / bCol name the pair
// columns of the row under test; unqualified names correlate to the
// UPDATE target inside a trigger body.
func pairHiddenProbe(aCol, bCol string) string {
	return `EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = ` + bCol + `
		WHERE ca.image_id = ` + aCol + `
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))`
}

func (b *bootstrapper) ensureColumn(table, column, alterSQL string) {
	if b.err != nil {
		return
	}
	var count int
	if err := b.db.Write.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_xinfo(?) WHERE name = ?`, table, column,
	).Scan(&count); err != nil {
		b.err = fmt.Errorf("inspect %s.%s: %w", table, column, err)
		return
	}
	if count > 0 {
		return
	}
	if _, err := b.db.Write.Exec(alterSQL); err != nil {
		b.err = fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
}

// backfillIfFresh runs the one-time seed the two probes above share: only
// when the probe found nothing before the DDL ran, and only while the
// bootstrap is still healthy.
func (b *bootstrapper) backfillIfFresh(pre int, backfillSQL, backfillLabel string) {
	if b.err != nil || pre > 0 {
		return
	}
	if _, err := b.db.Write.Exec(backfillSQL); err != nil {
		b.err = fmt.Errorf("%s: %w", backfillLabel, err)
	}
}

// backfillIfFreshColumn runs backfillSQL only when the ALTER actually
// added the column - re-bootstraps of an in-use library must not
// overwrite values the triggers have since maintained. The pre-check
// reads table_info rather than xinfo: a generated column is never
// backfilled, so only a real stored column counts as fresh.
func (b *bootstrapper) backfillIfFreshColumn(table, column, alterSQL, backfillSQL, backfillLabel string) {
	if b.err != nil {
		return
	}
	var pre int
	if err := b.db.Write.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&pre); err != nil {
		b.err = fmt.Errorf("inspect %s.%s: %w", table, column, err)
		return
	}
	b.ensureColumn(table, column, alterSQL)
	b.backfillIfFresh(pre, backfillSQL, backfillLabel)
}

// backfillIfFreshTable runs backfillSQL only when the CREATE actually
// added the table - re-bootstraps of an in-use library must not
// overwrite values the triggers have since maintained. A DB that never
// had the table (older export, older binary) gets the one-time seed on
// its first boot here.
func (b *bootstrapper) backfillIfFreshTable(table, createSQL, backfillSQL, backfillLabel string) {
	if b.err != nil {
		return
	}
	var pre int
	if err := b.db.Write.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&pre); err != nil {
		b.err = fmt.Errorf("inspect table %s: %w", table, err)
		return
	}
	b.exec("create "+table, createSQL)
	b.backfillIfFresh(pre, backfillSQL, backfillLabel)
}
