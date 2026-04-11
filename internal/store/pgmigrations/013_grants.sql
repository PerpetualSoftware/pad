-- Collection and item grants for guest access and member overrides (TASK-417).

CREATE TABLE IF NOT EXISTS collection_grants (
    id TEXT PRIMARY KEY,
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    permission TEXT NOT NULL DEFAULT 'view',
    granted_by TEXT NOT NULL REFERENCES users(id),
    created_at TEXT NOT NULL,
    UNIQUE(collection_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_collection_grants_user ON collection_grants(user_id, workspace_id);
CREATE INDEX IF NOT EXISTS idx_collection_grants_collection ON collection_grants(collection_id);

CREATE TABLE IF NOT EXISTS item_grants (
    id TEXT PRIMARY KEY,
    item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    permission TEXT NOT NULL DEFAULT 'view',
    granted_by TEXT NOT NULL REFERENCES users(id),
    created_at TEXT NOT NULL,
    UNIQUE(item_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_item_grants_user ON item_grants(user_id, workspace_id);
CREATE INDEX IF NOT EXISTS idx_item_grants_item ON item_grants(item_id);
