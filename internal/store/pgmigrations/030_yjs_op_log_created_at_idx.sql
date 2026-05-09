-- Postgres mirror of migrations/051_yjs_op_log_created_at_idx.sql
-- (PLAN-1248 TASK-1309). See the SQLite migration for full context.

CREATE INDEX IF NOT EXISTS idx_yjs_updates_item_id_created_at
    ON item_yjs_updates(item_id, created_at);
