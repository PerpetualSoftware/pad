-- Workspace invitations for member onboarding
CREATE TABLE IF NOT EXISTS workspace_invitations (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    email        TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'editor'
                 CHECK (role IN ('owner', 'editor', 'viewer')),
    invited_by   TEXT NOT NULL REFERENCES users(id),
    code         TEXT NOT NULL UNIQUE,
    accepted_at  TEXT,
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_invitations_workspace ON workspace_invitations(workspace_id);
CREATE INDEX IF NOT EXISTS idx_invitations_code ON workspace_invitations(code);
CREATE INDEX IF NOT EXISTS idx_invitations_email ON workspace_invitations(email);
