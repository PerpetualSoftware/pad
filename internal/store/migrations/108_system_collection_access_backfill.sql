-- Migration 108: list system collections for restricted members (TASK-3376).
--
-- Until TASK-3376 every member with collection_access = 'specific' saw every
-- system collection (conventions, playbooks) implicitly, and an editor among
-- them could write it. They are now ordinary collections: a restricted member
-- reaches one only when it is in member_collection_access or granted.
--
-- So that the upgrade changes nobody's access, this lists every live system
-- collection for every member already restricted, which is exactly what each
-- of them could see before. Owners can then remove them per member. A system
-- collection created after this migration is NOT listed for existing
-- restricted members; that is the new rule, stated in the release notes.
INSERT OR IGNORE INTO member_collection_access (workspace_id, user_id, collection_id, created_at)
SELECT wm.workspace_id, wm.user_id, c.id, strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM workspace_members wm
JOIN collections c ON c.workspace_id = wm.workspace_id
WHERE wm.collection_access = 'specific'
  AND c.is_system = 1
  AND c.deleted_at IS NULL;
