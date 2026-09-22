-- Migration 068: mark op-log rows that cannot change the document (BUG-3124).
-- Postgres twin of SQLite migration 091; see that file for the rationale.
ALTER TABLE item_yjs_updates ADD COLUMN IF NOT EXISTS content_bearing BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE item_yjs_updates ADD COLUMN IF NOT EXISTS content_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_yjs_updates_item_hash
    ON item_yjs_updates(item_id, content_hash);
