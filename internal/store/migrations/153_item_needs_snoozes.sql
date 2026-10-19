-- Migration 153: item_needs_snoozes (TASK-3918, IDEA-3906 half A unit A2).
--
-- A person's dismissal or deferral of an item in their needs-you inbox.
-- Per user: one row per (user, item), and nobody else's view changes.
--
-- kind 'dismiss' holds while nothing has happened since the snapshot: no
-- live comment by anyone but the user's own human comments is newer than
-- seen_comment_at, none at that same second has an id outside
-- seen_comment_ids, and the item's MAX(status_transitions.seq) is at most
-- seen_transition_seq. items.seq and updated_at cannot stand in: comments
-- move neither, and timestamps are whole seconds.
--
-- kind 'defer' holds until defer_until, whatever changes before then
-- (IDEA-3906 A2, ruled 10-18).
--
-- Every TEXT value is written by the server: kind is validated, timestamps
-- are re-formatted from parsed times, and seen_comment_ids holds only ids of
-- the item's own live comments (the client's marker is clamped to them).
CREATE TABLE IF NOT EXISTS item_needs_snoozes (
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id             TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    workspace_id        TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind                TEXT NOT NULL CHECK (kind IN ('dismiss', 'defer')),
    defer_until         TEXT,
    seen_comment_at     TEXT NOT NULL DEFAULT '',
    seen_comment_ids    TEXT NOT NULL DEFAULT '[]',
    seen_transition_seq INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL,
    PRIMARY KEY (user_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_item_needs_snoozes_item ON item_needs_snoozes(item_id);
