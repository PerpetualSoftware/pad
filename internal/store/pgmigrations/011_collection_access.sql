-- Add per-member collection visibility.
ALTER TABLE workspace_members ADD COLUMN IF NOT EXISTS collection_access TEXT NOT NULL DEFAULT 'all';

CREATE TABLE IF NOT EXISTS member_collection_access (
    workspace_id  TEXT NOT NULL,
    user_id       TEXT NOT NULL,
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (workspace_id, user_id, collection_id),
    FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE
);

-- Add is_system flag to collections.
ALTER TABLE collections ADD COLUMN IF NOT EXISTS is_system BOOLEAN NOT NULL DEFAULT FALSE;

-- Mark conventions and playbooks as system collections.
UPDATE collections SET is_system = TRUE WHERE slug IN ('conventions', 'playbooks');
