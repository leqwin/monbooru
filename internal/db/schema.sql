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
    ('meta',      '#ffaa00', 1);

CREATE TABLE IF NOT EXISTS tags (
    id               INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL,
    category_id      INTEGER NOT NULL REFERENCES tag_categories(id),
    usage_count      INTEGER NOT NULL DEFAULT 0,
    is_alias         INTEGER NOT NULL DEFAULT 0,
    canonical_tag_id INTEGER REFERENCES tags(id),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(name, category_id)
);

CREATE TABLE IF NOT EXISTS images (
    id             INTEGER PRIMARY KEY,
    sha256         TEXT    NOT NULL UNIQUE,
    canonical_path TEXT    NOT NULL,
    folder_path    TEXT    NOT NULL DEFAULT '',
    file_type      TEXT    NOT NULL,
    width          INTEGER,
    height         INTEGER,
    file_size      INTEGER NOT NULL,
    is_missing     INTEGER NOT NULL DEFAULT 0,
    is_favorited   INTEGER NOT NULL DEFAULT 0,
    auto_tagged_at TEXT,
    source_type    TEXT    NOT NULL DEFAULT 'none',
    ingested_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS image_paths (
    id           INTEGER PRIMARY KEY,
    image_id     INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL UNIQUE,
    is_canonical INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS image_tags (
    image_id    INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    tag_id      INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
    is_auto     INTEGER NOT NULL DEFAULT 0,
    confidence  REAL,
    tagger_name TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (image_id, tag_id)
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

CREATE TABLE IF NOT EXISTS saved_searches (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    query      TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_tags_name         ON tags(name);
CREATE INDEX IF NOT EXISTS idx_tags_category     ON tags(category_id);
CREATE INDEX IF NOT EXISTS idx_tags_usage        ON tags(usage_count DESC);
CREATE INDEX IF NOT EXISTS idx_tags_active_usage ON tags(usage_count DESC, name) WHERE is_alias = 0;
CREATE INDEX IF NOT EXISTS idx_image_tags_tag    ON image_tags(tag_id);
CREATE INDEX IF NOT EXISTS idx_image_tags_image  ON image_tags(image_id);
CREATE INDEX IF NOT EXISTS idx_image_tags_user_tag ON image_tags(tag_id) WHERE is_auto = 0;
CREATE INDEX IF NOT EXISTS idx_images_sha256     ON images(sha256);
CREATE INDEX IF NOT EXISTS idx_images_ingested   ON images(ingested_at DESC);
CREATE INDEX IF NOT EXISTS idx_images_favorited  ON images(is_favorited);
CREATE INDEX IF NOT EXISTS idx_images_source     ON images(source_type);
CREATE INDEX IF NOT EXISTS idx_images_missing    ON images(is_missing);
CREATE INDEX IF NOT EXISTS idx_images_folder     ON images(folder_path);
CREATE INDEX IF NOT EXISTS idx_images_folder_visible ON images(folder_path) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_image_paths_image ON image_paths(image_id);
