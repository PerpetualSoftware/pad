-- IDEA-1486: harden views.config to NOT NULL DEFAULT '{}'.
-- See also IDEA-1484 / PR #562 (collections.settings precedent at
-- 055_collections_settings_not_null.sql) and migration 056 in the same
-- PR for the parallel items.fields / items.tags hardening.
--
-- SQLite does not support ALTER COLUMN ... SET NOT NULL, so the table is
-- rebuilt via the standard SQLite recipe. The views table has no inbound
-- FKs, no indexes, and no FTS triggers — this is the smaller of the two
-- rebuilds in this PR.
--
-- foreign_keys is toggled OFF for the duration as the canonical pattern;
-- the IDEA-1485 migration runner lifts these PRAGMA bookends outside the
-- wrapping transaction so they actually take effect.

PRAGMA foreign_keys = OFF;

-- Backfill: repair any row whose config violates the post-migration
-- shape contract (NOT NULL + JSON object). See migration 056 for the
-- full rationale on the widened WHERE clause (codex R2 P1).
UPDATE views
SET config = '{}'
WHERE config IS NULL
   OR json_valid(config) = 0
   OR json_type(config) != 'object';

DROP TABLE IF EXISTS views_new;

CREATE TABLE views_new (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id),
    collection_id TEXT REFERENCES collections(id),
    name          TEXT NOT NULL,
    slug          TEXT NOT NULL,
    view_type     TEXT NOT NULL,
    config        TEXT NOT NULL DEFAULT '{}',
    sort_order    INTEGER DEFAULT 0,
    is_default    INTEGER DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE(workspace_id, slug)
);

INSERT INTO views_new (
    id, workspace_id, collection_id, name, slug, view_type, config,
    sort_order, is_default, created_at, updated_at
)
SELECT
    id, workspace_id, collection_id, name, slug, view_type,
    COALESCE(config, '{}'),
    sort_order, is_default, created_at, updated_at
FROM views;

DROP TABLE views;
ALTER TABLE views_new RENAME TO views;

PRAGMA foreign_keys = ON;
