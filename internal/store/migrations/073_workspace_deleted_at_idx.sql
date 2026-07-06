-- Migration 073: partial index on workspaces(deleted_at) for the
-- soft-deleted-workspace hard-purge sweeper (TASK-1966).
--
-- The sweeper's eligibility query runs on every tick (daily):
--   SELECT id, slug FROM workspaces
--   WHERE deleted_at IS NOT NULL AND deleted_at < ?
--   ORDER BY deleted_at
--
-- Without an index that is a full scan of the workspaces table. A
-- PARTIAL index (WHERE deleted_at IS NOT NULL) keeps the index tiny —
-- it only holds soft-deleted rows, which are a vanishing fraction of
-- all workspaces — while still covering the cutoff range scan and the
-- ORDER BY. Partial indexes are supported on both SQLite (>= 3.8.0) and
-- PostgreSQL; mirrored in pgmigrations/051.

CREATE INDEX IF NOT EXISTS idx_workspaces_deleted_at
    ON workspaces(deleted_at)
    WHERE deleted_at IS NOT NULL;
