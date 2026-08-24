-- Monbooru Schema
-- All statements use IF NOT EXISTS / INSERT OR IGNORE for idempotency.

CREATE TABLE IF NOT EXISTS tag_categories (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    color      TEXT    NOT NULL DEFAULT '#888888',
    is_builtin INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO tag_categories (name, color, is_builtin) VALUES
    ('general',   '#3d90e3', 1),
    ('character', '#00aa00', 1),
    ('artist',    '#cc0000', 1),
    ('copyright', '#aa00aa', 1),
    ('meta',      '#ffaa00', 1),
    ('rating',    '#996666', 1),
    ('medium',    '#7d4fbf', 1),
    ('person',    '#b85c9e', 1),
    ('year',      '#4a8fa8', 1),
    ('species',   '#ed5d1f', 1);

-- Promote any pre-existing user-created medium/person/year/species category
-- to built-in so a library that already had one of these as a custom row
-- stops being deletable once the seed catches up.
UPDATE tag_categories SET is_builtin = 1 WHERE name IN ('medium', 'person', 'year', 'species');

CREATE TABLE IF NOT EXISTS tags (
    id               INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL,
    category_id      INTEGER NOT NULL REFERENCES tag_categories(id),
    usage_count      INTEGER NOT NULL DEFAULT 0,
    is_alias         INTEGER NOT NULL DEFAULT 0,
    canonical_tag_id INTEGER REFERENCES tags(id),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Creation provenance: 'user', a booru site, 'ptr', an auto-tagger
    -- name, an import label. Stamped once at insert, never overwritten.
    origin           TEXT    NOT NULL DEFAULT '',
    -- Most recent application to an image; NULL = never applied.
    last_used_at     TEXT,
    -- On PTR alias rows: the latest refresh no longer listed this
    -- spelling. The row stays until the operator acts.
    stale            INTEGER NOT NULL DEFAULT 0,
    UNIQUE(name, category_id)
);

-- Canonical rating tags. The category accepts only these four names; the
-- tagger routes WD14 rating labels here, search uses the IDs directly via
-- a fixed-name SELECT, and the GetOrCreateTag guard refuses anything else
-- in this category.
INSERT OR IGNORE INTO tags (name, category_id) VALUES
    ('general',      (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('sensitive',    (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('questionable', (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('explicit',     (SELECT id FROM tag_categories WHERE name = 'rating'));

CREATE TABLE IF NOT EXISTS images (
    id             INTEGER PRIMARY KEY,
    sha256         TEXT    NOT NULL UNIQUE,
    -- Digest of the same bytes sha256 addresses, kept because boorus key
    -- their posts on it. '' until computed; never a dedup key, and never
    -- written from a source's claimed md5 (image_sources.md5), which is
    -- allowed to disagree with the file.
    md5            TEXT    NOT NULL DEFAULT '',
    canonical_path TEXT    NOT NULL,
    folder_path    TEXT    NOT NULL DEFAULT '',
    file_type      TEXT    NOT NULL,
    width          INTEGER,
    height         INTEGER,
    file_size      INTEGER NOT NULL,
    is_missing     INTEGER NOT NULL DEFAULT 0,
    is_favorited   INTEGER NOT NULL DEFAULT 0,
    -- New ingests land in the inbox (1) for triage; archived rows sit at 0.
    -- Matching idx_images_inbox_visible is created in db.go Bootstrap so the
    -- migration's ALTER TABLE on existing libraries runs before the index
    -- references the new column.
    is_inbox       INTEGER NOT NULL DEFAULT 1,
    auto_tagged_at TEXT,
    source_type    TEXT    NOT NULL DEFAULT 'none',
    origin         TEXT    NOT NULL DEFAULT 'ingest',
    source         TEXT    NOT NULL DEFAULT '',
    url            TEXT    NOT NULL DEFAULT '',
    -- Operator's freeform note. The full-JSON export carries it; the
    -- merge importer leaves it alone.
    note           TEXT    NOT NULL DEFAULT '',
    -- Operator's image-level original source (where the artist first posted
    -- it), one URL; distinct from the per-origin image_sources.original a
    -- booru pull fills. Carried by the full-JSON export since v8; the merge
    -- importer leaves it alone.
    original_source TEXT   NOT NULL DEFAULT '',
    -- Video duration in seconds (REAL so short clips and sub-second
    -- precision survive). NULL for non-video rows and for video rows
    -- that pre-date the column or whose ffprobe call failed; the
    -- search and detail surfaces treat NULL as "unknown" rather than
    -- "zero". Backfilled on re-extract metadata.
    duration_seconds REAL,
    -- 64-bit canonical perceptual hash (DCT-based pHash, mirror-
    -- canonicalised). NULL until backfilled or when the file has no
    -- visual surface. Added by ensureColumn on existing libraries;
    -- the matching idx_images_phash is created in db.Bootstrap.
    phash          INTEGER,
    ingested_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Upload-batch token (UnixNano) shared by every row from one web-UI
    -- upload POST; NULL for watcher / sync / API rows. Lets the inbox
    -- cluster view group a single drop as one batch regardless of the
    -- 15-minute time-gap rule. Added by ensureColumn on existing libraries.
    upload_batch   INTEGER
);

CREATE TABLE IF NOT EXISTS image_paths (
    id           INTEGER PRIMARY KEY,
    image_id     INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL UNIQUE,
    is_canonical INTEGER NOT NULL DEFAULT 0,
    -- File mtime at the time the row was last touched, in Unix seconds and
    -- again as Unix nanoseconds. Sync's unchanged-shortcut requires (size,
    -- mtime) parity so a same-size in-place edit is still re-hashed, and it
    -- reads mtime_nsec when the row has one: whole seconds cannot tell an
    -- edit that landed in the same second the file was last observed. 0
    -- marks rows that predate each column on upgraded libraries - a row
    -- with no nsec keeps the second-grained comparison rather than costing
    -- the library a full re-hash on upgrade.
    mtime_unix   INTEGER NOT NULL DEFAULT 0,
    mtime_nsec   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS image_tags (
    image_id    INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    tag_id      INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
    is_auto     INTEGER NOT NULL DEFAULT 0,
    is_implied  INTEGER NOT NULL DEFAULT 0,
    confidence  REAL,
    tagger_name TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- The attributed source's latest fetch no longer carried this tag.
    stale       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (image_id, tag_id)
);

-- Per-image collection membership. An image can belong to several
-- collections, each with its own position. images.series / series_order
-- mirror one "home" membership so the global order-sort and the
-- adjacency cursor keep riding the scalar columns.
CREATE TABLE IF NOT EXISTS image_collections (
    image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    name     TEXT    NOT NULL COLLATE NOCASE,
    position INTEGER,
    PRIMARY KEY (image_id, name)
);

-- Collections opted in to find-relations. A row lets the relations
-- session surface pairs whose two images share that collection; absence
-- (the default) hides them, since membership already relates the images.
CREATE TABLE IF NOT EXISTS collection_find_relations (
    name TEXT PRIMARY KEY COLLATE NOCASE
);

-- Per-image origin (provenance). An image can carry several sources; each
-- is a site label plus the post URL it came from. images.source / images.url
-- mirror the "primary" (first) origin so existing readers keep riding the
-- scalar columns, the same way images.series mirrors a home collection.
CREATE TABLE IF NOT EXISTS image_sources (
    image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    site       TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    post_id    TEXT    NOT NULL DEFAULT '',
    url        TEXT    NOT NULL DEFAULT '',
    md5        TEXT    NOT NULL DEFAULT '', -- md5 the source last claimed on a push/enrich; audit trail, never a dedup key
    commentary TEXT    NOT NULL DEFAULT '', -- artist commentary from this source; operator-editable, overwritten by a re-pull
    original   TEXT    NOT NULL DEFAULT '', -- upstream artist source the booru post declared (usually a URL, newline-joined when several); operator-editable, overwritten by a re-pull
    similarity REAL    NOT NULL DEFAULT 0,  -- best similarity-service score (0-100) a lookup matched this origin with; 0 = exact or manual. A matched origin's file differs by design, so refetches skip the md5 verify
    md5_match  TEXT    NOT NULL DEFAULT '', -- claimed-md5 vs local-file verdict: '' unknown, 'match', 'differ'. Maintained by the trg_*_verdict triggers off the two stored digests; gates the [upgrade] action
    parent_url TEXT    NOT NULL DEFAULT '', -- canonical URL of the post this booru post declared as its parent; drives derivative-edge linking once both sides are in the gallery
    upgrade_kept INTEGER NOT NULL DEFAULT 0, -- operator kept the local file; hides the upgrade offer until the post claims a different md5
    post_width  INTEGER NOT NULL DEFAULT 0,  -- what the post says the file it serves is; 0 / '' where the source published nothing. Never measured here, unlike images.width
    post_height INTEGER NOT NULL DEFAULT 0,
    post_size   INTEGER NOT NULL DEFAULT 0,
    post_ext    TEXT    NOT NULL DEFAULT '',
    fetched_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (image_id, site, post_id)
);

-- Positional note boxes overlaid on an image (Danbooru "notes"), in original-
-- image pixel coordinates. Pulled per source; the whole set a source
-- contributed is replaced on a re-pull. Body is plain text.
CREATE TABLE IF NOT EXISTS image_annotations (
    id         INTEGER PRIMARY KEY,
    image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    site       TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    post_id    TEXT    NOT NULL DEFAULT '',
    x          INTEGER NOT NULL,
    y          INTEGER NOT NULL,
    w          INTEGER NOT NULL,
    h          INTEGER NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    -- 1 for an operator-drawn box (site/post_id empty), 0 for a source-pulled
    -- one. Source-keyed deletes/replaces gate on manual = 0 so an operator box
    -- survives a source edit, removal or re-pull.
    manual     INTEGER NOT NULL DEFAULT 0,
    fetched_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_image_annotations_image ON image_annotations(image_id);

CREATE TABLE IF NOT EXISTS tag_implications (
    parent_tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    implied_tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Same vocabulary as tags.origin; stamped when the edge is created.
    origin         TEXT    NOT NULL DEFAULT '',
    -- On PTR edges: the latest refresh no longer carried the edge.
    stale          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (parent_tag_id, implied_tag_id)
);

CREATE TABLE IF NOT EXISTS sd_metadata (
    image_id        INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    prompt          TEXT,
    negative_prompt TEXT,
    model           TEXT,
    seed            INTEGER,
    sampler         TEXT,
    steps           INTEGER,
    cfg_scale       REAL,
    raw_params      TEXT,
    generation_hash TEXT
);

CREATE TABLE IF NOT EXISTS comfyui_metadata (
    image_id         INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    prompt           TEXT,
    model_checkpoint TEXT,
    seed             INTEGER,
    sampler          TEXT,
    steps            INTEGER,
    cfg_scale        REAL,
    raw_workflow     TEXT,
    generation_hash  TEXT
);

CREATE TABLE IF NOT EXISTS manga_metadata (
    image_id         INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    title            TEXT,
    series           TEXT,
    number           TEXT,
    volume           TEXT,
    count            INTEGER,
    summary          TEXT,
    notes            TEXT,
    year             INTEGER,
    month            INTEGER,
    day              INTEGER,
    writer           TEXT,
    penciller        TEXT,
    inker            TEXT,
    colorist         TEXT,
    letterer         TEXT,
    cover_artist     TEXT,
    editor           TEXT,
    publisher        TEXT,
    imprint          TEXT,
    genre            TEXT,
    web              TEXT,
    language_iso     TEXT,
    format           TEXT,
    manga            TEXT,
    age_rating       TEXT,
    community_rating REAL,
    xml_page_count   INTEGER,
    raw_xml          TEXT
);

-- Duplicate group: a set of images representing the same source content
-- in different quality / format. One member is the "original" (the best
-- representative). original_image_id is NOT NULL and has no ON DELETE
-- cascade so the parent image_delete path is forced to fix the original
-- (or dissolve the group) before the image row goes away - prevents the
-- group from outliving its anchor as a dangling reference.
CREATE TABLE IF NOT EXISTS dup_groups (
    id                INTEGER PRIMARY KEY,
    original_image_id INTEGER NOT NULL REFERENCES images(id),
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS dup_group_members (
    image_id   INTEGER PRIMARY KEY REFERENCES images(id)     ON DELETE CASCADE,
    group_id   INTEGER NOT NULL    REFERENCES dup_groups(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS alt_groups (
    id         INTEGER PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS alt_group_members (
    image_id   INTEGER PRIMARY KEY REFERENCES images(id)     ON DELETE CASCADE,
    group_id   INTEGER NOT NULL    REFERENCES alt_groups(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Directed version edge. child_image_id is PK (each image has at most
-- one parent); parent_image_id is UNIQUE (each parent has at most one
-- child). Together this enforces a strict chain - branching is a
-- derivative relationship.
CREATE TABLE IF NOT EXISTS version_edges (
    child_image_id  INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    parent_image_id INTEGER NOT NULL UNIQUE REFERENCES images(id) ON DELETE CASCADE,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Directed derivative edge. derivative_image_id is PK (a derivative has
-- exactly one source); source_image_id is unconstrained so a source can
-- carry many derivatives (tree).
CREATE TABLE IF NOT EXISTS derivative_edges (
    derivative_image_id INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    source_image_id     INTEGER NOT NULL    REFERENCES images(id) ON DELETE CASCADE,
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Canonicalised "not related" pair (a < b). Recorded so a rejected pair
-- never resurfaces in the find-pairs queue at any distance.
CREATE TABLE IF NOT EXISTS not_related_pairs (
    a_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    b_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (a_image_id, b_image_id)
);

-- Singleton holding the active session's order mode plus the
-- started-at timestamp. id is constrained to 1 so there is at most
-- one row regardless of how the upserts go.
CREATE TABLE IF NOT EXISTS relation_session (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    order_mode TEXT NOT NULL DEFAULT 'smallest_distance_first',
    detector   TEXT NOT NULL DEFAULT 'both',
    started_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    paused_at  TEXT
);

-- Candidate pairs surfaced by the find-pairs background job; the
-- session UI iterates these and either commits a relation (deletes
-- the row), rejects the pair (deletes the row + writes
-- not_related_pairs), or skips (sets skipped_at so the row sorts to
-- the back of the queue). Canonicalised a < b matches the rest of the
-- symmetric tables.
-- collection_hidden stores the collection opt-out verdict per row: 1
-- when both images share a collection absent from
-- collection_find_relations. max_rating_rank mirrors the higher of
-- the two members' images.rating_rank. Both are stamped by the
-- bootstrap triggers on insert and resweeped when the underlying
-- state changes, so the session walk and the hub counters read
-- stored values instead of probing memberships and ratings per row.
CREATE TABLE IF NOT EXISTS potential_relation_pairs (
    a_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    b_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    distance   INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    skipped_at TEXT,
    source     TEXT NOT NULL DEFAULT 'phash',
    score      REAL,
    collection_hidden INTEGER NOT NULL DEFAULT 0,
    max_rating_rank   INTEGER NOT NULL DEFAULT -1,
    PRIMARY KEY (a_image_id, b_image_id)
);

CREATE TABLE IF NOT EXISTS saved_searches (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    query      TEXT NOT NULL,
    sort       TEXT NOT NULL DEFAULT '',
    sort_order TEXT NOT NULL DEFAULT '',
    seed       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Folded-duplicate pairs: old_id (a pre-widening fold) -> new_id (the richer
-- spelling that superseded it). Recomputed by the Find-folded-duplicates scan;
-- ambiguous = 1 when old_id has more than one candidate new_id.
CREATE TABLE IF NOT EXISTS folded_tag_pairs (
    old_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    new_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    category_id INTEGER NOT NULL,
    ambiguous   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (old_id, new_id)
);

-- One row per (image, lookup backend), written the moment an attempt is
-- enqueued: an image nobody ever looked up costs no row. attempts counts
-- consecutive concluded misses and drives the retry ladder; queued_at
-- non-NULL is the in-flight state (there is no pending literal to leak into
-- last_result) and job_id is monloader's id for it, which is what makes an
-- attempt whose callback went missing reconcilable at all. next_due_at NULL
-- means nothing is scheduled, for one of three reasons last_result and
-- images.scheduled_lookup tell apart: a hit, an exhausted ladder, or the
-- operator's opt-out. ptr_cursor is monloader's index position at the miss,
-- so a PTR retry can skip an index that has not moved.
CREATE TABLE IF NOT EXISTS image_lookups (
    image_id    INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    backend     TEXT    NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    queued_at   TEXT,
    job_id      INTEGER,
    last_at     TEXT,
    last_result TEXT    NOT NULL DEFAULT '',
    next_due_at TEXT,
    ptr_cursor  INTEGER,
    PRIMARY KEY (image_id, backend)
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_tags_name         ON tags(name);
CREATE INDEX IF NOT EXISTS idx_tags_category     ON tags(category_id);
CREATE INDEX IF NOT EXISTS idx_tags_usage        ON tags(usage_count DESC);
CREATE INDEX IF NOT EXISTS idx_tags_active_usage ON tags(usage_count DESC, name) WHERE is_alias = 0;
CREATE INDEX IF NOT EXISTS idx_tags_alias_canonical ON tags(canonical_tag_id, name) WHERE is_alias = 1;
-- Composite covering index: a `tag_id = ?` lookup gets `image_id`
-- straight from the index entry, so the multi-leg INTERSECT in the
-- AND-driver doesn't pay one row-fetch per matched image. The
-- `image_id` suffix also makes `tag_id = ? AND image_id >= ?` a real
-- range seek for the recent-id-bounded INTERSECT shape. This index
-- supersedes the older single-column `idx_image_tags_tag(tag_id)`;
-- Bootstrap drops that one explicitly when upgrading.
CREATE INDEX IF NOT EXISTS idx_image_tags_tag_image ON image_tags(tag_id, image_id);
CREATE INDEX IF NOT EXISTS idx_image_tags_image  ON image_tags(image_id);
-- Covers the collection: filter (name -> image_id semi-join), the
-- sidebar collection counts (GROUP BY name), and the pinned-order
-- position lookup. The PRIMARY KEY (image_id, name) covers the inverse
-- "collections of one image" read.
CREATE INDEX IF NOT EXISTS idx_image_collections_name ON image_collections(name, image_id, position);
-- Covers the source: filter (site -> image_id semi-join) and the sidebar
-- source-label counts (GROUP BY site), mirroring idx_image_collections_name.
CREATE INDEX IF NOT EXISTS idx_image_sources_site ON image_sources(site, image_id);
-- Covers the parent-side probe of the derivative-edge linking (find the
-- image holding a pushed post's declared parent URL).
CREATE INDEX IF NOT EXISTS idx_image_sources_url ON image_sources(url) WHERE url != '';
CREATE INDEX IF NOT EXISTS idx_tag_implications_implied ON tag_implications(implied_tag_id);
CREATE INDEX IF NOT EXISTS idx_image_tags_user_tag ON image_tags(tag_id) WHERE is_auto = 0;
CREATE INDEX IF NOT EXISTS idx_image_tags_auto_tagger ON image_tags(tagger_name)
    WHERE is_auto = 1 AND tagger_name IS NOT NULL AND tagger_name != '';
CREATE INDEX IF NOT EXISTS idx_images_sha256     ON images(sha256);
CREATE INDEX IF NOT EXISTS idx_images_ingested   ON images(ingested_at DESC);
CREATE INDEX IF NOT EXISTS idx_images_favorited  ON images(is_favorited);
CREATE INDEX IF NOT EXISTS idx_images_source_type ON images(source_type);
-- idx_images_source(source) is created in db.Bootstrap so the migration's
-- ALTER TABLE ADD COLUMN source on libraries that predate the column
-- runs before this index references it (schema.sql is executed before
-- ensureColumn).
CREATE INDEX IF NOT EXISTS idx_images_missing    ON images(is_missing);
CREATE INDEX IF NOT EXISTS idx_images_folder     ON images(folder_path);
CREATE INDEX IF NOT EXISTS idx_images_folder_visible ON images(folder_path) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_filesize_visible ON images(file_size DESC, id DESC) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_ingested_visible ON images(ingested_at DESC, id DESC) WHERE is_missing = 0;
-- Partial visible indexes over columns the original schema already
-- carries (file_type, source_type) so mime: / type: / ai: filters
-- seek the visibility-bounded set instead of falling back on
-- idx_images_missing. idx_images_source_visible and
-- idx_images_duration_visible reference columns added by ensureColumn
-- migrations (source, duration_seconds) and live in db.Bootstrap
-- below the matching ensureColumn call - adding them here would
-- error on libraries that predate the columns.
CREATE INDEX IF NOT EXISTS idx_images_file_type_visible   ON images(file_type)   WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_source_type_visible ON images(source_type) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_image_paths_image ON image_paths(image_id);
-- Partial index over the non-canonical alias rows so the sha256 / file-
-- duplicates walkers and the name: filter's alias-paths EXISTS ride a
-- covering seek instead of scanning every image_paths row to filter
-- for is_canonical = 0. A canonical image_paths row sits on every
-- image; the non-canonical rows are typically a small subset (renames,
-- sha-collisions, sync moves), so the partial index stays cheap.
CREATE INDEX IF NOT EXISTS idx_image_paths_aliases ON image_paths(image_id) WHERE is_canonical = 0;
CREATE INDEX IF NOT EXISTS idx_sd_metadata_genhash      ON sd_metadata(generation_hash)      WHERE generation_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_comfyui_metadata_genhash ON comfyui_metadata(generation_hash) WHERE generation_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sd_metadata_seed         ON sd_metadata(seed)                 WHERE seed IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_comfyui_metadata_seed    ON comfyui_metadata(seed)            WHERE seed IS NOT NULL;
-- Relations covering indexes. The PRIMARY KEY on dup_group_members.image_id
-- and alt_group_members.image_id already covers the per-image group lookup;
-- the (group_id, image_id) shape below covers the inverse - listing a
-- group's members ordered. version_edges and derivative_edges get inverse
-- indexes from the parent / source side. not_related_pairs picks up an
-- index on (b, a) so pair-existence checks ride a covering seek regardless
-- of which side the caller passed first.
CREATE INDEX IF NOT EXISTS idx_dup_group_members_group ON dup_group_members(group_id, image_id);
CREATE INDEX IF NOT EXISTS idx_alt_group_members_group ON alt_group_members(group_id, image_id);
CREATE INDEX IF NOT EXISTS idx_derivative_edges_source ON derivative_edges(source_image_id);
CREATE INDEX IF NOT EXISTS idx_not_related_b           ON not_related_pairs(b_image_id, a_image_id);
CREATE INDEX IF NOT EXISTS idx_potential_pairs_distance ON potential_relation_pairs(skipped_at, distance, a_image_id);
-- b-side seek for the collection_hidden resweep triggers; the a side
-- rides the primary key prefix.
CREATE INDEX IF NOT EXISTS idx_potential_pairs_b        ON potential_relation_pairs(b_image_id);
CREATE INDEX IF NOT EXISTS idx_image_lookups_due        ON image_lookups(backend, next_due_at);
-- The partial in-flight index is what keeps the reconcile sweep proportional
-- to the handful of rows actually waiting rather than to the table.
CREATE INDEX IF NOT EXISTS idx_image_lookups_inflight   ON image_lookups(queued_at) WHERE queued_at IS NOT NULL;
