-- Migration 104 (Postgres): a durable marker for a partly imported workspace (TASK-896).
-- Postgres counterpart to migrations/131_workspace_import_status.sql; the rationale is there.
CREATE TABLE IF NOT EXISTS workspace_import_status (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    status       TEXT NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);
