-- Migration 043 (Postgres): per-user Insights/report layout preferences
-- (PLAN-1628 / TASK-1634). Postgres counterpart to
-- migrations/064_user_report_layouts.sql.
--
-- One row per (user, workspace): hidden cards + default window + default
-- collection filter, as a JSON object (models.ReportLayout). Per-user only;
-- per-workspace shared layouts are out of scope for v1.

CREATE TABLE IF NOT EXISTS user_report_layouts (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    config       TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (user_id, workspace_id)
);
