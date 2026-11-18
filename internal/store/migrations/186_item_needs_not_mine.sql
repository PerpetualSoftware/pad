-- Migration 186: item_needs_not_mine (TASK-4643).
--
-- A person marking an item "not mine" in their needs-you inbox. Per user:
-- one row per (user, item), and nobody else's view changes.
--
-- Unlike a dismissal (item_needs_snoozes), the hold survives other people
-- talking and ordinary status changes. It ends only when the item comes back
-- to this person: an open mention of them outside the marker or after
-- seen_at, a transition with a seq above seen_transition_seq into a waiting
-- status on the done field, the item now in an awaits_human collection other
-- than seen_collection_id, the item now assigned to them when
-- seen_assignee_id was not them or by an assignment made after seen_at, or a
-- reminder of theirs whose oldest outstanding occurrence came due after
-- seen_at. Times are compared in whole seconds, strictly later.
--
-- A separate table, not a new snooze kind: the snooze kind CHECK would need a
-- table rebuild, and these watermarks differ. Writing either hide state
-- deletes the other in the same transaction.
--
-- Every TEXT value is written by the server: the marker is clamped to the
-- item (ids read back from comments, the collection and assignee kept only
-- when they match the item now, timestamps re-formatted from parsed times
-- and held at or before now), and source is derived from how the request
-- authenticated.
CREATE TABLE IF NOT EXISTS item_needs_not_mine (
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id             TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    workspace_id        TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    seen_comment_at     TEXT NOT NULL DEFAULT '',
    seen_comment_ids    TEXT NOT NULL DEFAULT '[]',
    seen_transition_seq INTEGER NOT NULL DEFAULT 0,
    seen_collection_id  TEXT NOT NULL DEFAULT '',
    seen_assignee_id    TEXT NOT NULL DEFAULT '',
    seen_at             TEXT NOT NULL DEFAULT '',
    source              TEXT NOT NULL CHECK (source IN ('web', 'cli')),
    created_at          TEXT NOT NULL,
    PRIMARY KEY (user_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_item_needs_not_mine_item ON item_needs_not_mine(item_id);
