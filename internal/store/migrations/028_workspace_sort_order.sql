-- Add per-user workspace sort order to workspace_members
ALTER TABLE workspace_members ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;
