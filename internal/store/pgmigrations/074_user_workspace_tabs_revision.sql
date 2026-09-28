-- Migration 074 (Postgres): a per-user revision on the open set of workspace tabs (BUG-3285, PLAN-3002).
-- Postgres counterpart to migrations/100_user_workspace_tabs_revision.sql; the
-- rationale is there.
ALTER TABLE users ADD COLUMN IF NOT EXISTS workspace_tabs_revision BIGINT NOT NULL DEFAULT 0;
