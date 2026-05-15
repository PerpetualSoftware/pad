-- IDEA-1484: harden collections.settings to NOT NULL DEFAULT '{}'.
-- See also BUG-1482 / PR #561 (squash 714da48): the defensive `sql.NullString`
-- scans in collections.go and export.go remain in place and will be reverted
-- in a separate follow-up PR after this migration has rolled out everywhere.
--
-- SQLite does not support ALTER COLUMN ... SET NOT NULL, so the table is
-- rebuilt via the standard SQLite recipe. Existing FKs from items, views,
-- collection_access, and grants point at collections(id); since we preserve
-- the same primary-key values during the copy, those references remain valid.
-- foreign_keys is toggled OFF for the duration to avoid the constraint
-- checker tripping on the transient DROP TABLE.

PRAGMA foreign_keys = OFF;

-- Backfill any NULL settings before applying the constraint.
UPDATE collections SET settings = '{}' WHERE settings IS NULL;

DROP TABLE IF EXISTS collections_new;

CREATE TABLE collections_new (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL,
    icon         TEXT DEFAULT '',
    description  TEXT DEFAULT '',
    schema       TEXT NOT NULL DEFAULT '{"fields":[]}',
    settings     TEXT NOT NULL DEFAULT '{}',
    sort_order   INTEGER DEFAULT 0,
    is_default   INTEGER DEFAULT 0,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    deleted_at   TEXT,
    prefix       TEXT NOT NULL DEFAULT '',
    is_system    INTEGER NOT NULL DEFAULT 0,
    UNIQUE(workspace_id, slug)
);

INSERT INTO collections_new (
    id, workspace_id, name, slug, icon, description, schema, settings,
    sort_order, is_default, created_at, updated_at, deleted_at,
    prefix, is_system
)
SELECT
    id, workspace_id, name, slug, icon, description, schema,
    COALESCE(settings, '{}'),
    sort_order, is_default, created_at, updated_at, deleted_at,
    prefix, is_system
FROM collections;

DROP TABLE collections;
ALTER TABLE collections_new RENAME TO collections;

-- Recreate indexes (originally from 032_permission_indexes.sql).
CREATE INDEX IF NOT EXISTS idx_collections_system ON collections(workspace_id, is_system) WHERE is_system = 1 AND deleted_at IS NULL;

PRAGMA foreign_keys = ON;
