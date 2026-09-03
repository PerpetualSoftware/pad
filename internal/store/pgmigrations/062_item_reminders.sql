-- Item reminders — Postgres mirror of
-- internal/store/migrations/085_item_reminders.sql. See the SQLite migration
-- for the full rationale (why a table rather than a schema-field annotation;
-- why ON DELETE CASCADE here where event_outbox deliberately has no foreign
-- keys; why remind_at is an RFC3339 UTC instant rather than a `date` value;
-- why ack is its own column and nothing implicit acks).
--
-- One dialect note: timestamps stay TEXT, matching every other table in this
-- schema (items, watches, activities, event_outbox). Not a preference —
-- remind_at is compared against values the Go layer formats, and a TIMESTAMPTZ
-- here would silently change comparison semantics on the one column the
-- scheduler tick's claim predicate depends on. event_outbox's migration made
-- the same call on occurred_at for the same reason.
CREATE TABLE IF NOT EXISTS item_reminders (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    item_id      TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    remind_at    TEXT NOT NULL,
    fired_at     TEXT,
    acked_at     TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_item_reminders_armed
    ON item_reminders(remind_at)
    WHERE fired_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_item_reminders_unacked
    ON item_reminders(workspace_id, fired_at)
    WHERE fired_at IS NOT NULL AND acked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_item_reminders_item
    ON item_reminders(item_id, remind_at);
