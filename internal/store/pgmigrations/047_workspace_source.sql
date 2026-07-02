-- Postgres counterpart to migrations/069_workspace_source.sql.
-- Add `source` to workspaces: record how a workspace was created (web UI /
-- CLI / MCP). Existing rows default to '' ("unknown / legacy origin"); an
-- empty value is never treated as agent-created. New workspaces set
-- 'web' | 'cli' | 'mcp' explicitly at creation time. See BUG-1557.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';
