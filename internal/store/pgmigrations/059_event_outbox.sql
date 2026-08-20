-- Transactional event outbox — Postgres mirror of
-- internal/store/migrations/081_event_outbox.sql. See the SQLite migration for
-- the full rationale (SPEC-3 §choke point; why the event and the mutation must
-- commit together; why there are DELIBERATELY no foreign keys on
-- workspace_id / subject_id).
--
-- Two dialect differences, both deliberate:
--
--   1. payload is JSONB rather than TEXT, matching what migration 058 did for
--      collections.traits and what this schema already does for
--      collections.schema / settings. Go scans JSONB into a string exactly as
--      it does for those columns, so the store's scan path is dialect-neutral;
--      the gain is that Postgres validates the payload at write time. A
--      payload that fails to parse is an event no consumer can act on, and
--      catching that at the INSERT — inside the mutation's own transaction,
--      where it still rolls the mutation back — is strictly better than
--      discovering it at drain time when the mutation is long committed.
--
--   2. Timestamps stay TEXT, matching every other table in this schema
--      (watches, items, activities). Not a preference — occurred_at is
--      compared against values the Go layer formats, and a TIMESTAMPTZ here
--      would silently change comparison semantics on exactly the column
--      SPEC-3 pins time-relative predicates to.
CREATE TABLE IF NOT EXISTS event_outbox (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    event_type    TEXT NOT NULL,
    subject_kind  TEXT NOT NULL,
    subject_id    TEXT,
    payload       JSONB NOT NULL,
    hop           INTEGER NOT NULL DEFAULT 0,
    occurred_at   TEXT NOT NULL,
    dispatched_at TEXT,
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT
);

CREATE INDEX IF NOT EXISTS idx_event_outbox_pending
    ON event_outbox(occurred_at, id)
    WHERE dispatched_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_event_outbox_dispatched
    ON event_outbox(dispatched_at)
    WHERE dispatched_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_event_outbox_workspace
    ON event_outbox(workspace_id, occurred_at);
