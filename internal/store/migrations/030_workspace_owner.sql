-- Add owner_id column to workspaces.
-- Backfilled on startup by backfillWorkspaceOwners() with the earliest owner member.
-- The global UNIQUE(slug) constraint remains until TASK-412 replaces it
-- with UNIQUE(owner_id, slug) via table recreation.
ALTER TABLE workspaces ADD COLUMN owner_id TEXT DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_workspaces_owner ON workspaces(owner_id);
CREATE INDEX IF NOT EXISTS idx_workspaces_owner_slug ON workspaces(owner_id, slug);
