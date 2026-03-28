CREATE TABLE IF NOT EXISTS api_tokens (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    prefix TEXT NOT NULL,
    scopes TEXT NOT NULL DEFAULT '["*"]',
    expires_at TEXT,
    last_used_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id)
);
