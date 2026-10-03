-- Migration 082: list system collections for restricted members (TASK-3376).
-- Mirrors SQLite migration 108; see it for the rationale.
INSERT INTO member_collection_access (workspace_id, user_id, collection_id, created_at)
SELECT wm.workspace_id, wm.user_id, c.id, to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
FROM workspace_members wm
JOIN collections c ON c.workspace_id = wm.workspace_id
WHERE wm.collection_access = 'specific'
  AND c.is_system = TRUE
  AND c.deleted_at IS NULL
ON CONFLICT DO NOTHING;
