-- Migration 098: the per-user open set of workspace tabs (PLAN-3002 U1 / TASK-3256).
--
-- One row per (user, workspace) the user has OPEN in the workspace tab bar.
-- The design is on PLAN-3002's trail (§2 storage, §3 seed); Dave's rulings Q1
-- (the same open set on every device, so the whole row is server-side) and Q8
-- (seed 6) are what this migration encodes.
--
--   position     the tab's place in the bar, per user. It is its own order and
--                does NOT drive workspace_members.sort_order, which stays the
--                membership order the CLI, MCP and account export read.
--   ephemeral    a tab opened by a landing (deep link, invitation accept) that
--                has not been kept yet. At most one per user; the store enforces
--                it under the per-user write rather than with a partial index.
--   last_route   the in-workspace path the tab returns to. The server accepts
--                only a path under the workspace's own /{owner}/{ws}/ prefix.
--
-- Keyed on users and workspaces, NOT workspace_members, so a guest (who has no
-- member row) can hold a tab. The invariant "a tab never names a workspace the
-- user cannot see" is enforced on READ; the loss paths also delete rows, as
-- hygiene.

CREATE TABLE IF NOT EXISTS user_workspace_tabs (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    position     INTEGER NOT NULL DEFAULT 0,
    ephemeral    INTEGER NOT NULL DEFAULT 0,
    last_route   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (user_id, workspace_id)
);

CREATE INDEX IF NOT EXISTS idx_user_workspace_tabs_workspace ON user_workspace_tabs(workspace_id);

-- Seed: each user's first six live member workspaces, in the order the bar
-- shows them today (workspace_members.sort_order, then name), as durable tabs.
-- Guests get nothing; their first landing opens an ephemeral tab.
INSERT OR IGNORE INTO user_workspace_tabs (user_id, workspace_id, position, ephemeral, last_route, created_at, updated_at)
SELECT user_id, workspace_id, rn - 1, 0, NULL, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM (
    SELECT wm.user_id AS user_id, wm.workspace_id AS workspace_id,
           ROW_NUMBER() OVER (PARTITION BY wm.user_id ORDER BY wm.sort_order, w.name, w.id) AS rn
    FROM workspace_members wm
    JOIN workspaces w ON w.id = wm.workspace_id
    WHERE w.deleted_at IS NULL
) ranked
WHERE rn <= 6;
