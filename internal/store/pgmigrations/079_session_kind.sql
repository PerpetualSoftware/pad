-- Migration 079: record how a session was issued (BUG-3350). Mirrors SQLite
-- migration 105; see it for the rationale and the backfill.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'web';
UPDATE sessions SET kind = 'cli' WHERE device_info = 'cli-browser-auth';
