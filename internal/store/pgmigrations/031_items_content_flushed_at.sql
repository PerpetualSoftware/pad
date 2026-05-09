-- Postgres mirror of migrations/052_items_content_flushed_at.sql
-- (PLAN-1248 TASK-1309). See the SQLite migration for full context.

ALTER TABLE items ADD COLUMN IF NOT EXISTS content_flushed_at TEXT;

-- See SQLite migration for the watermark column rationale (Codex
-- review of TASK-1309 round 4 [P1]: timestamp comparison is unsafe
-- at second granularity).
ALTER TABLE items ADD COLUMN IF NOT EXISTS content_flushed_op_log_id BIGINT;

-- See SQLite migration for why items with existing op-log rows stay
-- at NULL (Codex review of TASK-1309 round 3 [P1]).
UPDATE items
SET content_flushed_at = updated_at,
    content_flushed_op_log_id = 0
WHERE content != ''
  AND NOT EXISTS (
    SELECT 1 FROM item_yjs_updates u WHERE u.item_id = items.id
  );
