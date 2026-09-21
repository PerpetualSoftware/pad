-- Typed-decision storage and its job queue — Postgres mirror of
-- internal/store/migrations/090_item_decisions.sql. See the SQLite migration
-- for the rationale (why a private queue rather than the event outbox; why a
-- state hash rather than items.seq; why generation exists; why answer is TEXT).
--
-- Dialect notes: timestamps stay TEXT, matching every other table here and
-- the lexical comparison the claim predicate depends on. confidence is DOUBLE
-- PRECISION, the Postgres spelling of SQLite's REAL. answer is TEXT, not
-- JSONB: no SQL reads inside it, and jsonb would refuse an escape the Go
-- marshaller legitimately writes.
CREATE TABLE IF NOT EXISTS item_decisions (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    item_id         TEXT NOT NULL,
    question_set    TEXT NOT NULL,
    question_key    TEXT NOT NULL,
    kind            TEXT NOT NULL,
    answer          TEXT NOT NULL,
    confidence      DOUBLE PRECISION,
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL,
    state_hash      TEXT NOT NULL,
    item_seq        BIGINT NOT NULL,
    state_truncated INTEGER NOT NULL DEFAULT 0,
    evaluated_at    TEXT NOT NULL,

    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_item_decisions_state
    ON item_decisions(item_id, question_set, question_key, state_hash);

CREATE INDEX IF NOT EXISTS idx_item_decisions_latest
    ON item_decisions(item_id, question_set, question_key, evaluated_at);

CREATE TABLE IF NOT EXISTS decision_jobs (
    item_id          TEXT NOT NULL,
    workspace_id     TEXT NOT NULL,
    question_set     TEXT NOT NULL,
    generation       BIGINT NOT NULL DEFAULT 1,
    enqueued_at      TEXT NOT NULL,
    claimed_by       TEXT,
    lease_expires_at TEXT,
    attempts         INTEGER NOT NULL DEFAULT 0,
    last_error       TEXT,

    PRIMARY KEY (item_id, question_set),
    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_decision_jobs_enqueued
    ON decision_jobs(enqueued_at);
