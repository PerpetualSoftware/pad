-- Migration 145 (Postgres): item_needs_not_mine (TASK-4643).
-- Postgres counterpart to migrations/186_item_needs_not_mine.sql; the rationale is there.
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
