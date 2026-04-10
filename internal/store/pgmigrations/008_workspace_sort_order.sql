-- Add per-user workspace sort order to workspace_members
ALTER TABLE workspace_members ADD COLUMN IF NOT EXISTS sort_order INTEGER NOT NULL DEFAULT 0;
