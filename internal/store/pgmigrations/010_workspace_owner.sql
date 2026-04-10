-- Add owner_id column to workspaces.
-- Backfilled on startup by backfillWorkspaceOwners() with the earliest owner member.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owner_id TEXT DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_workspaces_owner ON workspaces(owner_id);
CREATE INDEX IF NOT EXISTS idx_workspaces_owner_slug ON workspaces(owner_id, slug);
