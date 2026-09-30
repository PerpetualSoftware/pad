-- Migration 077 (Postgres): what a History needs from a version row (PLAN-2348 U2).
-- Postgres counterpart to migrations/103_item_version_history_data.sql; the rationale is there.
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS user_id TEXT;
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS lines_added INTEGER;
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS lines_removed INTEGER;
ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS is_create BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE item_versions SET is_create = TRUE
WHERE version_seq = 1
  AND created_at = (SELECT i.created_at FROM items i WHERE i.id = item_versions.item_id);
