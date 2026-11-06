CREATE TABLE user_workspace_groups (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_user_workspace_groups_name ON user_workspace_groups(user_id, lower(name));
CREATE INDEX idx_user_workspace_groups_user ON user_workspace_groups(user_id);

CREATE TABLE user_workspace_group_members (
    group_id TEXT NOT NULL REFERENCES user_workspace_groups(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, workspace_id)
);
CREATE INDEX idx_user_workspace_group_members_workspace ON user_workspace_group_members(workspace_id);
