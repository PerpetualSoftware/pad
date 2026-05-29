-- Migration 064: per-user Insights/report layout preferences
-- (PLAN-1628 / TASK-1634).
--
-- Stores each user's personalization of the Insights surface, scoped per
-- workspace: which metric cards are hidden, and the default window + collection
-- filter to restore on load. One row per (user, workspace) — a single config,
-- not named layouts (named layouts are a deliberate future scope). config is a
-- JSON object (models.ReportLayout). Per-workspace SHARED layouts are out of
-- scope for v1; this is strictly per-user.

CREATE TABLE IF NOT EXISTS user_report_layouts (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    config       TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (user_id, workspace_id)
);
