-- Share links for anonymous/external access to items and collections (TASK-421).
-- Tokens are hashed at rest (SHA-256). Raw token returned only once on creation.
-- D8: Anonymous users are ALWAYS read-only regardless of permission field.

CREATE TABLE IF NOT EXISTS share_links (
    id TEXT PRIMARY KEY,
    token_hash TEXT UNIQUE NOT NULL,
    target_type TEXT NOT NULL,               -- 'item' or 'collection'
    target_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    permission TEXT NOT NULL DEFAULT 'view', -- 'view' or 'edit' (edit only for require_auth links)
    created_by TEXT NOT NULL REFERENCES users(id),
    password_hash TEXT,
    expires_at TEXT,
    max_views INTEGER,
    require_auth INTEGER DEFAULT 0,
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
