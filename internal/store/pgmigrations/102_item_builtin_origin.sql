-- Migration 102 (Postgres): which built-in an item was made from (TASK-3462).
-- Postgres counterpart to migrations/128_item_builtin_origin.sql; the rationale is there.
CREATE TABLE IF NOT EXISTS item_builtin_origin (
    item_id     TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
    builtin_key TEXT NOT NULL,
    seed_hash   TEXT,
    seed_content TEXT,
    seed_fields  TEXT,
    created_at  TEXT NOT NULL
);
