-- Custom templates created by users
CREATE TABLE IF NOT EXISTS custom_templates (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    doc_type TEXT NOT NULL DEFAULT 'notes',
    icon TEXT NOT NULL DEFAULT '📝',
    content TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(workspace_id, name)
);
