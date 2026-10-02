-- Migration 080 (Postgres twin of SQLite 106): every live workspace's canonical owner holds an owner
-- membership (BUG-3355). The member doors now refuse to demote or remove the
-- canonical owner (workspaces.owner_id), and that refusal keeps a workspace
-- with an owner only where the canonical owner IS an owner member. This
-- repairs the rows where it is not, so the invariant holds everywhere.
--
-- Only ADDS an owner membership or RAISES an existing one to owner; it never
-- demotes or removes anyone, and never touches collection_access or
-- sort_order. Live workspaces only (deleted_at IS NULL), and only when the
-- owner user still exists. Idempotent: a second run matches no rows.

UPDATE workspace_members
SET role = 'owner'
WHERE role <> 'owner'
  AND EXISTS (
    SELECT 1 FROM workspaces w
    JOIN users u ON u.id = w.owner_id
    WHERE w.id = workspace_members.workspace_id
      AND w.owner_id = workspace_members.user_id
      AND w.deleted_at IS NULL
  );

INSERT INTO workspace_members (workspace_id, user_id, role)
SELECT w.id, w.owner_id, 'owner'
FROM workspaces w
JOIN users u ON u.id = w.owner_id
WHERE w.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM workspace_members m
    WHERE m.workspace_id = w.id AND m.user_id = w.owner_id
  )
ON CONFLICT (workspace_id, user_id) DO NOTHING;
