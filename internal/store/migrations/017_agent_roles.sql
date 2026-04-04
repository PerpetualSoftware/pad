-- Agent roles: workspace-scoped capability roles for human-agent assignment

CREATE TABLE IF NOT EXISTS agent_roles (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    slug         TEXT NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    icon         TEXT NOT NULL DEFAULT '',
    sort_order   INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE(workspace_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_agent_roles_workspace ON agent_roles(workspace_id);

-- Assignment columns on items: (user, role) pairs
ALTER TABLE items ADD COLUMN assigned_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE items ADD COLUMN agent_role_id TEXT REFERENCES agent_roles(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_items_assigned_user ON items(assigned_user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_agent_role ON items(agent_role_id) WHERE deleted_at IS NULL;

-- Remove assignee text field from existing Tasks collection schemas.
-- This replaces the free-text assignee with the structured (user, role) assignment above.
UPDATE collections
SET schema = REPLACE(schema, '{"key":"assignee","label":"Assignee","type":"text"},', '')
WHERE slug = 'tasks'
  AND schema LIKE '%"assignee"%';

-- Handle alternate JSON formatting (with spaces)
UPDATE collections
SET schema = REPLACE(schema, '{"key": "assignee", "label": "Assignee", "type": "text"},', '')
WHERE slug = 'tasks'
  AND schema LIKE '%"assignee"%';
