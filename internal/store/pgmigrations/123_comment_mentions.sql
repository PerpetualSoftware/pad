-- Migration 123 (Postgres): comment_mentions (TASK-3917).
-- Postgres counterpart to migrations/154_comment_mentions.sql; the rationale is there.
CREATE TABLE IF NOT EXISTS comment_mentions (
    comment_id   TEXT NOT NULL REFERENCES comments(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id      TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (comment_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_comment_mentions_user ON comment_mentions(user_id, workspace_id);
CREATE INDEX IF NOT EXISTS idx_comment_mentions_item ON comment_mentions(item_id);
