-- IDEA-1486: harden items.fields and items.tags to NOT NULL DEFAULT.
-- See also IDEA-1484 / PR #562 (collections.settings precedent at
-- 055_collections_settings_not_null.sql).
--
-- SQLite does not support ALTER COLUMN ... SET NOT NULL, so the table is
-- rebuilt via the standard SQLite recipe. All inbound FKs to items(id)
-- (item_links.source_id/target_id, item_versions.item_id, comments.item_id,
-- item_stars.item_id, grants.item_id, item_yjs_updates.item_id, and the
-- items.parent_id self-reference) point at the primary key; since we
-- preserve every `id` value during the copy, those references remain
-- valid after the rename.
--
-- foreign_keys is toggled OFF for the duration to avoid the constraint
-- checker tripping on the transient DROP TABLE. The IDEA-1485 migration
-- runner (store.go:applySQLiteMigration) lifts these PRAGMA bookends out
-- of the wrapping transaction so they actually take effect on SQLite.
--
-- This migration also drops + recreates the three items_fts triggers
-- (auto-dropped with the items table) and issues a `'rebuild'` on the
-- FTS5 virtual table. The items_fts virtual table itself stays in place:
-- its `content='items'` link rebinds to the renamed items table by name,
-- and `'rebuild'` repopulates the internal index against the post-
-- rebuild rowids. Trigger bodies are copied verbatim from migration 005.

PRAGMA foreign_keys = OFF;

-- Backfill: repair any row whose fields / tags violates the post-
-- migration shape contract (NOT NULL + object-or-array JSON). The
-- WHERE clauses widen the NULL-only filter to cover three pre-
-- existing failure modes:
--
--   1. fields IS NULL                 — the original IDEA-1486 case.
--   2. json_valid(fields) = 0         — empty-string, "not json", or
--                                       any other non-JSON text the
--                                       NOT-NULL-DEFAULT never blocked.
--   3. json_type(fields) != 'object'  — JSON-but-wrong-shape (e.g.
--                                       'null', '[]', '"a string"',
--                                       a number).
--
-- Same trio for tags with 'array' as the expected json_type. Per
-- codex R2 P1: the original NULL-only filter would have left every
-- malformed row in place. The partial UNIQUE index on
-- json_extract(fields, '$.invocation_slug') (recreated below) errors
-- on rows whose fields fails json_valid, so a single bad row would
-- break the CREATE INDEX mid-migration. Repair has to happen before
-- the rebuild so both the INSERT…SELECT and the post-rename CREATE
-- INDEX see only well-shaped rows.
UPDATE items
SET fields = '{}'
WHERE fields IS NULL
   OR json_valid(fields) = 0
   OR json_type(fields) != 'object';

UPDATE items
SET tags = '[]'
WHERE tags IS NULL
   OR json_valid(tags) = 0
   OR json_type(tags) != 'array';

DROP TABLE IF EXISTS items_new;

CREATE TABLE items_new (
    id                          TEXT PRIMARY KEY,
    workspace_id                TEXT NOT NULL REFERENCES workspaces(id),
    collection_id               TEXT NOT NULL REFERENCES collections(id),
    title                       TEXT NOT NULL,
    slug                        TEXT NOT NULL,
    content                     TEXT DEFAULT '',
    fields                      TEXT NOT NULL DEFAULT '{}',
    tags                        TEXT NOT NULL DEFAULT '[]',
    pinned                      INTEGER DEFAULT 0,
    sort_order                  INTEGER DEFAULT 0,
    parent_id                   TEXT REFERENCES items(id),
    created_by                  TEXT DEFAULT 'user',
    last_modified_by            TEXT DEFAULT 'user',
    source                      TEXT DEFAULT 'web',
    created_at                  TEXT NOT NULL,
    updated_at                  TEXT NOT NULL,
    deleted_at                  TEXT,
    item_number                 INTEGER,
    created_by_user_id          TEXT REFERENCES users(id),
    last_modified_by_user_id    TEXT REFERENCES users(id),
    assigned_user_id            TEXT REFERENCES users(id) ON DELETE SET NULL,
    agent_role_id               TEXT REFERENCES agent_roles(id) ON DELETE SET NULL,
    role_sort_order             INTEGER NOT NULL DEFAULT 0,
    content_flushed_at          TEXT,
    content_flushed_op_log_id   INTEGER,
    seq                         INTEGER NOT NULL DEFAULT 0,
    UNIQUE(workspace_id, slug)
);

INSERT INTO items_new (
    id, workspace_id, collection_id, title, slug, content, fields, tags,
    pinned, sort_order, parent_id, created_by, last_modified_by, source,
    created_at, updated_at, deleted_at, item_number,
    created_by_user_id, last_modified_by_user_id,
    assigned_user_id, agent_role_id, role_sort_order,
    content_flushed_at, content_flushed_op_log_id, seq
)
SELECT
    id, workspace_id, collection_id, title, slug, content,
    COALESCE(fields, '{}'),
    COALESCE(tags, '[]'),
    pinned, sort_order, parent_id, created_by, last_modified_by, source,
    created_at, updated_at, deleted_at, item_number,
    created_by_user_id, last_modified_by_user_id,
    assigned_user_id, agent_role_id, role_sort_order,
    content_flushed_at, content_flushed_op_log_id, seq
FROM items;

DROP TABLE items;
ALTER TABLE items_new RENAME TO items;

-- Recreate all 8 indexes (originally from 005, 017, 053, 054). Every
-- index is attached to the dropped items table and does NOT survive
-- DROP TABLE items; each must be re-issued explicitly.
CREATE INDEX IF NOT EXISTS idx_items_collection ON items(collection_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_workspace ON items(workspace_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_updated ON items(updated_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_assigned_user ON items(assigned_user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_agent_role ON items(agent_role_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_workspace_seq ON items(workspace_id, seq DESC);

-- Playbook invocation_slug uniqueness guard (from migration 054). The
-- body matches 054 verbatim. This is the DB-level TOCTOU protection
-- that the application-layer checkUniqueFields in handlers_items.go
-- relies on — dropping it during the rebuild and forgetting to
-- recreate it would silently lose the uniqueness invariant.
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_invocation_slug_per_collection
    ON items(collection_id, json_extract(fields, '$.invocation_slug'))
    WHERE json_extract(fields, '$.invocation_slug') IS NOT NULL
      AND json_extract(fields, '$.invocation_slug') != ''
      AND deleted_at IS NULL;

-- Recreate items_fts triggers (auto-dropped with the items table).
-- Bodies match 005_collections.sql:96-107 exactly. The IF EXISTS guards
-- mirror 046_restore_documents_fts_triggers.sql as an idempotency belt;
-- they are harmless no-ops on the success path because DROP TABLE items
-- already removed them.
DROP TRIGGER IF EXISTS items_fts_insert;
DROP TRIGGER IF EXISTS items_fts_update;
DROP TRIGGER IF EXISTS items_fts_delete;

CREATE TRIGGER items_fts_insert AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(rowid, title, content, tags) VALUES (NEW.rowid, NEW.title, NEW.content, NEW.tags);
END;

CREATE TRIGGER items_fts_update AFTER UPDATE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, content, tags) VALUES('delete', OLD.rowid, OLD.title, OLD.content, OLD.tags);
  INSERT INTO items_fts(rowid, title, content, tags) VALUES (NEW.rowid, NEW.title, NEW.content, NEW.tags);
END;

CREATE TRIGGER items_fts_delete AFTER DELETE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, content, tags) VALUES('delete', OLD.rowid, OLD.title, OLD.content, OLD.tags);
END;

-- Rebuild the FTS5 internal index from the post-rebuild items table.
-- The contentless-content link binds by table name, so items_fts re-
-- attaches to the renamed items table automatically; 'rebuild' clears
-- and repopulates the internal index against the current rowids.
INSERT INTO items_fts(items_fts) VALUES ('rebuild');

PRAGMA foreign_keys = ON;
