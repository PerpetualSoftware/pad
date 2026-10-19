-- Migration 122 (Postgres): item_needs_snoozes (TASK-3918).
-- Postgres counterpart to migrations/153_item_needs_snoozes.sql; the rationale is there.
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
