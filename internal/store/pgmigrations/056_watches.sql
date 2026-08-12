-- Migration 056 (Postgres): watches — durable, server-side subscriptions
-- for the padd event-stream / plugin-monitor nudge pipeline (TASK-2533).
-- Postgres counterpart to internal/store/migrations/078_watches.sql — same
-- intent, same columns, same indexes; see that file for the full rationale.
-- No dialect differences: every column here is TEXT, so there is nothing
-- for BoolToInt-style translation to do.
CREATE TABLE IF NOT EXISTS watches (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id      TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    predicate    TEXT,
    created_at   TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_watches_user_item ON watches(user_id, item_id);
CREATE INDEX IF NOT EXISTS idx_watches_user ON watches(user_id);
