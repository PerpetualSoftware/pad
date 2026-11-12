CREATE TABLE calendar_feeds (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    layer TEXT NOT NULL CHECK (layer IN ('mine', 'agent')),
    workspace_ids TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    last_used_at TEXT
);
CREATE INDEX idx_calendar_feeds_user ON calendar_feeds(user_id);
