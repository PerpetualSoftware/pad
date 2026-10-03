-- Migration 086: app installs (SPEC-6 U1, TASK-3388).
-- Mirrors SQLite migration 112; see it for the rationale.
CREATE TABLE IF NOT EXISTS app_installs (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    origin       TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'active'
                 CHECK (state IN ('active', 'disabling', 'inactive', 'uninstalling', 'uninstalled')),
    auth_epoch   BIGINT NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_installs_workspace ON app_installs (workspace_id);
