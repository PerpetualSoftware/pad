-- Migration 060: per-user last-write timestamp (PLAN-1542 / TASK-1543).
--
-- Adds a dedicated `last_write_at` column on `users` so admin engagement
-- metrics can distinguish active writers from passive readers. The existing
-- `last_active_at` column is bumped on any authenticated request (throttled
-- by TouchUserActivity), so it can't tell us whether the user actually
-- created or modified anything.
--
-- The hook (TouchUserWrite + handler-layer call sites) is wired in the same
-- task; this migration is the data-layer half.
--
-- Backfill draws from the existing `activities` table — the canonical record
-- of who did what — rather than from items.last_modified_by (which holds
-- attribution strings like "user"/"agent", not user IDs). The action values
-- below match what handlers_items.go and handlers_comments.go pass into
-- logActivity / logActivityWithMeta.

ALTER TABLE users ADD COLUMN last_write_at TEXT DEFAULT NULL;
CREATE INDEX IF NOT EXISTS idx_users_last_write_at ON users(last_write_at);

UPDATE users
SET last_write_at = (
    SELECT MAX(a.created_at)
    FROM activities a
    WHERE a.user_id = users.id
      AND a.action IN ('created', 'updated', 'archived', 'restored',
                       'moved', 'commented')
)
WHERE last_write_at IS NULL;
