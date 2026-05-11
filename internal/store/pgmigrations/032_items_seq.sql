-- Postgres mirror of migrations/053_items_seq.sql (TASK-1352).
-- See the SQLite migration for context.
--
-- The Postgres path also takes a workspace-scoped pg_advisory_xact_lock
-- at write time to serialize concurrent seq assignments — SQLite's
-- single-writer rule handles that implicitly, but Postgres needs an
-- explicit guard so two concurrent UPDATEs don't read the same MAX(seq)
-- and produce duplicate values. See `acquireWorkspaceSeqLock` in
-- internal/store/items.go.

ALTER TABLE items ADD COLUMN IF NOT EXISTS seq BIGINT NOT NULL DEFAULT 0;

UPDATE items
SET seq = sub.rn
FROM (
    SELECT id,
           ROW_NUMBER() OVER (PARTITION BY workspace_id ORDER BY updated_at, id) AS rn
    FROM items
) AS sub
WHERE items.id = sub.id;

CREATE INDEX IF NOT EXISTS idx_items_workspace_seq ON items(workspace_id, seq DESC);
