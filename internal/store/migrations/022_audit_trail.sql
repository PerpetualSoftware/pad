-- Audit trail: extend activities with IP/UA, nullable workspace, expanded action types.
-- SQLite requires table recreation to alter CHECK constraints.

PRAGMA foreign_keys = OFF;

DROP TABLE IF EXISTS activities_new;

CREATE TABLE activities_new (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT REFERENCES workspaces(id),
    document_id   TEXT,
    action        TEXT NOT NULL,
    actor         TEXT NOT NULL,
    source        TEXT NOT NULL DEFAULT 'web',
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL,
    user_id       TEXT REFERENCES users(id),
    ip_address    TEXT,
    user_agent    TEXT
);

-- Copy existing data (ip_address and user_agent will be NULL for historical records)
INSERT INTO activities_new (id, workspace_id, document_id, action, actor, source, metadata, created_at, user_id)
SELECT id, workspace_id, document_id, action, actor, source, metadata, created_at, user_id
FROM activities;

-- Update foreign keys pointing to activities
-- comments.activity_id references activities(id) — handled automatically by SQLite
-- since we're using the same primary key values

DROP TABLE activities;
ALTER TABLE activities_new RENAME TO activities;

-- Recreate indexes
CREATE INDEX IF NOT EXISTS idx_activities_workspace ON activities(workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_document ON activities(document_id, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_action ON activities(action, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_user ON activities(user_id, created_at);

PRAGMA foreign_keys = ON;
