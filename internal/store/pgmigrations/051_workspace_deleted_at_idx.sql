-- Migration 051 (Postgres): partial index on workspaces(deleted_at) for
-- the soft-deleted-workspace hard-purge sweeper (TASK-1966). Mirrors
-- SQLite migrations/073.
--
-- The sweeper's eligibility query runs daily:
--   SELECT id, slug FROM workspaces
--   WHERE deleted_at IS NOT NULL AND deleted_at < ?
--   ORDER BY deleted_at
--
-- The partial predicate (WHERE deleted_at IS NOT NULL) keeps the index
-- restricted to the handful of soft-deleted rows rather than every
-- workspace, while still covering the cutoff range scan + ORDER BY.

CREATE INDEX IF NOT EXISTS idx_workspaces_deleted_at
    ON workspaces(deleted_at)
    WHERE deleted_at IS NOT NULL;
