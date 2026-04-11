-- Indexes for permission table query performance (TASK-486).

CREATE INDEX IF NOT EXISTS idx_mca_collection ON member_collection_access(collection_id);
CREATE INDEX IF NOT EXISTS idx_collections_system ON collections(workspace_id, is_system) WHERE is_system = TRUE AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_wm_user ON workspace_members(user_id);
