-- Item stars: per-user item bookmarking/favoriting.
-- Users can star any item to mark it as personally important.
-- Stars are user-scoped (my stars ≠ your stars).
CREATE TABLE IF NOT EXISTS item_stars (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id    TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (user_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_item_stars_user ON item_stars(user_id);
CREATE INDEX IF NOT EXISTS idx_item_stars_item ON item_stars(item_id);
