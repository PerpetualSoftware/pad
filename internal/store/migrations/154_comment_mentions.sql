-- Migration 154: comment_mentions (TASK-3917, IDEA-3906).
--
-- An index of which users a live comment @-mentions. One row per
-- (comment, user). It is derived from comments.body and resolved at WRITE
-- time (a later rename does not reindex), so the reader never parses bodies.
--
-- Every value is server-minted: ids are UUIDs read from comments/users and
-- created_at is a server timestamp. Rows leave with their comment (CASCADE
-- on hard delete and reap) or when the comment is tombstoned (the helper
-- deletes them).
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
