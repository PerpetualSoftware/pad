-- Migration 149: the per-user, per-workspace read cursor behind the
-- "Since you were last here" card (TASK-3615, DOC-3616).
--
-- seen_at is the moment the user last pressed Mark caught up in this
-- workspace. It moves ONLY on that action (Dave, 10-17); reading the card
-- never writes. With no row, the card's window is the 14-day ceiling, and
-- the first Mark caught up creates the row.
--
-- seen_at is fixed-width UTC ("2006-01-02T15:04:05.000000000Z", the
-- decisionTime format), so TEXT comparison against other timestamps in
-- that format is time comparison.
--
-- Keyed on users and workspaces, not workspace_members, so a guest can hold
-- one, as with user_workspace_tabs (098).

CREATE TABLE IF NOT EXISTS workspace_read_cursors (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    seen_at      TEXT NOT NULL,
    PRIMARY KEY (user_id, workspace_id)
);

CREATE INDEX IF NOT EXISTS idx_workspace_read_cursors_workspace ON workspace_read_cursors(workspace_id);
