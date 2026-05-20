-- Migration 039 (Postgres): per-user last-write timestamp (PLAN-1542 / TASK-1543).
--
-- Postgres counterpart to migrations/060_user_last_write_at.sql. Same intent:
-- a dedicated last_write_at signal distinct from last_active_at (which fires
-- on any authenticated request and so includes reads). Hook lives at the
-- handler layer; this migration is the data-layer half.
--
-- Backfill reads from the activities table (canonical record of who-did-what)
-- using the action set that handlers_items.go / handlers_comments.go actually
-- emit through logActivity / logActivityWithMeta.

ALTER TABLE users ADD COLUMN IF NOT EXISTS last_write_at TEXT DEFAULT NULL;
CREATE INDEX IF NOT EXISTS idx_users_last_write_at ON users(last_write_at);

UPDATE users u
SET last_write_at = sub.max_ts
FROM (
    SELECT a.user_id, MAX(a.created_at) AS max_ts
    FROM activities a
    WHERE a.user_id IS NOT NULL
      AND a.action IN ('created', 'updated', 'archived', 'restored',
                       'moved', 'commented')
    GROUP BY a.user_id
) sub
WHERE sub.user_id = u.id
  AND u.last_write_at IS NULL;
