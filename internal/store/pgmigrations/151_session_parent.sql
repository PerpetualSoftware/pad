-- Migration 151: a WebView session names the web session it was minted from
-- (SQLite migration 194). Deleting the parent deletes every child through the
-- cascade; every existing row keeps a NULL parent.
SET LOCAL lock_timeout = '5s';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS parent_session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);
