-- Postgres mirror of SQLite migration 149_workspace_read_cursors.sql
-- (TASK-3615): the per-user, per-workspace read cursor. See that file for
-- the rationale.

CREATE TABLE IF NOT EXISTS workspace_read_cursors (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    seen_at      TEXT NOT NULL,
    PRIMARY KEY (user_id, workspace_id)
);

CREATE INDEX IF NOT EXISTS idx_workspace_read_cursors_workspace ON workspace_read_cursors(workspace_id);
