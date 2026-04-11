-- Add per-member collection visibility.
-- collection_access: 'all' (default) = see everything, 'specific' = see only granted collections.
-- Decision D7: default is "all" — absence of restrictions means full access.

ALTER TABLE workspace_members ADD COLUMN collection_access TEXT NOT NULL DEFAULT 'all';

-- Join table for the "specific" case: which collections a member can see.
CREATE TABLE IF NOT EXISTS member_collection_access (
    workspace_id  TEXT NOT NULL,
    user_id       TEXT NOT NULL,
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (workspace_id, user_id, collection_id),
    FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE
);

-- Add is_system flag to collections for conventions/playbooks exemption.
-- System collections are always visible to members regardless of collection_access.
ALTER TABLE collections ADD COLUMN is_system INTEGER NOT NULL DEFAULT 0;

-- Mark conventions and playbooks as system collections.
UPDATE collections SET is_system = 1 WHERE slug IN ('conventions', 'playbooks');
