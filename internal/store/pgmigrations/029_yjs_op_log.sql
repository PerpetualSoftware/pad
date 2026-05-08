-- Postgres mirror of migrations/050_yjs_op_log.sql (PLAN-1248 TASK-1252).
-- See the SQLite migration for full architectural context.
--
-- Differences from the SQLite migration:
--   - id is BIGSERIAL (vs. INTEGER PRIMARY KEY AUTOINCREMENT). Same monotonic
--     contract — Postgres sequences never reuse a value.
--   - update_data is BYTEA (vs. BLOB).
--   - created_at remains TEXT (ISO8601 UTC) per pad's cross-dialect convention.

CREATE TABLE IF NOT EXISTS item_yjs_updates (
    id              BIGSERIAL PRIMARY KEY,
    item_id         TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    update_data     BYTEA NOT NULL,
    schema_version  TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_yjs_updates_item_id
    ON item_yjs_updates(item_id, id);
