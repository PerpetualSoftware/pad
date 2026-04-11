-- Share links for anonymous/external access to items and collections (TASK-421).

CREATE TABLE IF NOT EXISTS share_links (
    id TEXT PRIMARY KEY,
    token_hash TEXT UNIQUE NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    permission TEXT NOT NULL DEFAULT 'view',
    created_by TEXT NOT NULL REFERENCES users(id),
    password_hash TEXT,
    expires_at TEXT,
    max_views INTEGER,
    require_auth BOOLEAN DEFAULT FALSE,
    restrict_to_email TEXT,
    view_count INTEGER DEFAULT 0,
    unique_viewers INTEGER DEFAULT 0,
    last_viewed_at TEXT,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_share_links_target ON share_links(target_type, target_id);
CREATE INDEX IF NOT EXISTS idx_share_links_workspace ON share_links(workspace_id, created_by);

CREATE TABLE IF NOT EXISTS share_link_views (
    id TEXT PRIMARY KEY,
    share_link_id TEXT NOT NULL REFERENCES share_links(id) ON DELETE CASCADE,
    viewer_fingerprint TEXT,
    viewer_user_id TEXT REFERENCES users(id),
    viewed_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_share_link_views_link ON share_link_views(share_link_id);
