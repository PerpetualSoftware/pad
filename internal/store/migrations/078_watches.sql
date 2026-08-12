-- Migration 078: watches — durable, server-side subscriptions for the
-- padd event-stream / plugin-monitor nudge pipeline (TASK-2533, per
-- DOC-2479's subscription-table design).
--
-- A watch is created by `pad watch <ref>` (optionally with a `--until
-- field=value` predicate) and lives independently of any client process:
-- the plugin monitor that consumes it restarts every Claude Code session,
-- so the subscription itself cannot be in-memory state — it must survive
-- both the monitor process and a padd restart.
--
-- Columns match DOC-2479's schema literally: id, workspace_id, user_id,
-- item_id, predicate, created_at. `predicate` is a nullable TEXT holding
-- the raw `--until field=value` string (or empty for an unconditional
-- watch); DOC-2479 specs no boolean-combinator grammar, so this
-- deliberately stores at most one `field=value` pair rather than
-- inventing one.
CREATE TABLE IF NOT EXISTS watches (
    -- NOT NULL is explicit: SQLite doesn't imply it for a TEXT PRIMARY
    -- KEY, unlike Postgres (see item_workspace_moves/077 for the same
    -- note).
    id           TEXT NOT NULL PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id      TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    predicate    TEXT,
    created_at   TEXT NOT NULL
);

-- `pad watch <ref>` re-run on an already-watched item is an upsert (new
-- predicate replaces the old one), not a duplicate subscription — one
-- watch per (user, item). Also the index the event-stream handler's
-- per-notification lookup uses.
CREATE UNIQUE INDEX IF NOT EXISTS uq_watches_user_item ON watches(user_id, item_id);

-- `pad watch list` / the event-stream handler's "load my active watches"
-- query, both keyed by user_id alone.
CREATE INDEX IF NOT EXISTS idx_watches_user ON watches(user_id);
