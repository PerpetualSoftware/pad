-- Migration 106 (Postgres): the highest op-log id a compaction snapshot covers (TASK-3531).
-- Postgres counterpart to migrations/134_yjs_compacted_through.sql; the rationale is there.
ALTER TABLE items ADD COLUMN IF NOT EXISTS yjs_compacted_through BIGINT;
ALTER TABLE items ADD COLUMN IF NOT EXISTS yjs_snapshot_op_id BIGINT;
