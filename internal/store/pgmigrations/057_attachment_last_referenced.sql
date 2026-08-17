-- Migration 057: attachments.last_referenced_at (BUG-2415).
-- Postgres mirror of SQLite migration 079 — see that file for the full
-- rationale (writer-side reference stamp backing the orphan-GC claim
-- protocol). TEXT ISO-8601 UTC to match the table's other timestamps.
ALTER TABLE attachments ADD COLUMN IF NOT EXISTS last_referenced_at TEXT;
