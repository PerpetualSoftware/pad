-- Migration 076 (Postgres): comment tombstones (BUG-3252).
-- Postgres counterpart to migrations/102_comment_tombstones.sql; the rationale is there.
ALTER TABLE comments ADD COLUMN IF NOT EXISTS deleted_at TEXT;
