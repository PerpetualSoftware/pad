-- Migration 051: composite index on item_yjs_updates(item_id, created_at)
-- for the periodic op-log prune sweeper (TASK-1309).
--
-- The candidate query for the sweeper is:
--   SELECT item_id FROM item_yjs_updates
--   GROUP BY item_id HAVING MAX(created_at) < ?
--
-- Without this index, that query is a full table scan: the existing
-- (item_id, id) index from migration 050 covers the live-replay path
-- (`WHERE item_id = ? AND id > ? ORDER BY id ASC`) but is useless for
-- aggregating MAX(created_at). The sweeper is the only feature that
-- aggregates by created_at across all items, and it'll be running on
-- exactly the table that grows fastest under collab load.
--
-- (item_id, created_at) — vs. just (created_at) — also speeds up the
-- per-item prune that follows the candidate query:
--   DELETE FROM item_yjs_updates WHERE item_id = ?
--     AND NOT EXISTS (SELECT 1 ... WHERE item_id = ? AND created_at >= ?)
--
-- Per Codex review of TASK-1309 [P2].

CREATE INDEX IF NOT EXISTS idx_yjs_updates_item_id_created_at
    ON item_yjs_updates(item_id, created_at);
