-- Timeline: link comments to activities, threading, and reactions

-- Link comments to the activity that triggered them (e.g. status-change comments)
ALTER TABLE comments ADD COLUMN activity_id TEXT REFERENCES activities(id);

-- Threading: allow comments to be replies to other comments
ALTER TABLE comments ADD COLUMN parent_id TEXT REFERENCES comments(id);

CREATE INDEX IF NOT EXISTS idx_comments_parent ON comments(parent_id);
CREATE INDEX IF NOT EXISTS idx_comments_activity ON comments(activity_id);

-- Emoji reactions on comments
CREATE TABLE IF NOT EXISTS comment_reactions (
    id         TEXT PRIMARY KEY,
    comment_id TEXT NOT NULL REFERENCES comments(id) ON DELETE CASCADE,
    user_id    TEXT REFERENCES users(id),
    actor      TEXT NOT NULL DEFAULT 'user',
    emoji      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(comment_id, user_id, emoji)
);

CREATE INDEX IF NOT EXISTS idx_comment_reactions_comment ON comment_reactions(comment_id);
