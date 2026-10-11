-- Migration 109 (Postgres): the edits of every content-anchored patch (PLAN-3578 step 3).
-- Postgres counterpart to migrations/137_item_patches.sql; the rationale is there.
CREATE TABLE IF NOT EXISTS item_patches (
    id           TEXT PRIMARY KEY,
    item_id      TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    edits        TEXT NOT NULL,
    version_id   TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_item_patches_item ON item_patches(item_id);
