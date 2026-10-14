-- Migration 113 (Postgres): the lowest op-log id a compaction snapshot covers (BUG-3747).
-- Postgres counterpart to migrations/143_yjs_compacted_from.sql; the rationale is there.
ALTER TABLE items ADD COLUMN IF NOT EXISTS yjs_compacted_from BIGINT;
