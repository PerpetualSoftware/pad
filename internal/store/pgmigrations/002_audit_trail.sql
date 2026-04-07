-- Audit trail: extend activities with IP/UA, nullable workspace, expanded action types.

-- Add new columns
ALTER TABLE activities ADD COLUMN IF NOT EXISTS ip_address TEXT;
ALTER TABLE activities ADD COLUMN IF NOT EXISTS user_agent TEXT;

-- Make workspace_id nullable (auth events have no workspace)
ALTER TABLE activities ALTER COLUMN workspace_id DROP NOT NULL;

-- Drop restrictive CHECK constraints and replace with open ones
ALTER TABLE activities DROP CONSTRAINT IF EXISTS activities_action_check;
ALTER TABLE activities DROP CONSTRAINT IF EXISTS activities_actor_check;
ALTER TABLE activities DROP CONSTRAINT IF EXISTS activities_source_check;

-- Add indexes for audit log queries
CREATE INDEX IF NOT EXISTS idx_activities_action ON activities(action, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_user ON activities(user_id, created_at);
