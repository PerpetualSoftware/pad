-- Indexes for permission table query performance (TASK-486).

-- member_collection_access: reverse lookup by collection (for cascade cleanup on collection delete)
CREATE INDEX IF NOT EXISTS idx_mca_collection ON member_collection_access(collection_id);

-- collections: fast lookup of system collections per workspace
CREATE INDEX IF NOT EXISTS idx_collections_system ON collections(workspace_id, is_system) WHERE is_system = 1 AND deleted_at IS NULL;

-- workspace_members: fast lookup by user across workspaces (for user deletion cascade)
CREATE INDEX IF NOT EXISTS idx_wm_user ON workspace_members(user_id);
