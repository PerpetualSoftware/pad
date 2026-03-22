CREATE TABLE IF NOT EXISTS progress_snapshots (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    total_tasks  INTEGER NOT NULL DEFAULT 0,
    done_tasks   INTEGER NOT NULL DEFAULT 0,
    open_tasks   INTEGER NOT NULL DEFAULT 0,
    in_progress  INTEGER NOT NULL DEFAULT 0,
    percentage   REAL NOT NULL DEFAULT 0.0,
    phase_data   TEXT NOT NULL DEFAULT '[]',
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_snapshots_workspace_time
    ON progress_snapshots(workspace_id, created_at);
