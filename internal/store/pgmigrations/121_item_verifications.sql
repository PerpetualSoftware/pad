-- Verification evidence tied to a revision (TASK-3617) — Postgres mirror of
-- internal/store/migrations/151_item_verifications.sql. See the SQLite
-- migration for the rationale (why a table, why criterion_hash and not
-- spec_seq, why via_item_id is SET NULL).
--
-- Dialect notes: timestamps stay TEXT, matching item_decisions and the
-- lexical ordering the latest-row read depends on. spec_seq is BIGINT, the
-- width of items.seq.
CREATE TABLE IF NOT EXISTS item_verifications (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    item_id         TEXT NOT NULL,
    criterion_id    TEXT NOT NULL,
    criterion_hash  TEXT NOT NULL,
    spec_seq        BIGINT NOT NULL,
    code_revision   TEXT NOT NULL DEFAULT '',
    verdict         TEXT NOT NULL,
    evidence        TEXT NOT NULL DEFAULT '',
    evidence_url    TEXT NOT NULL DEFAULT '',
    via_item_id     TEXT REFERENCES items(id) ON DELETE SET NULL,
    run_id          TEXT NOT NULL,
    created_by      TEXT NOT NULL DEFAULT '',
    user_id         TEXT NOT NULL DEFAULT '',
    agent_name      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,

    FOREIGN KEY (item_id, workspace_id) REFERENCES items(id, workspace_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_item_verifications_latest
    ON item_verifications(item_id, criterion_id, created_at);

CREATE INDEX IF NOT EXISTS idx_item_verifications_workspace
    ON item_verifications(workspace_id);

CREATE INDEX IF NOT EXISTS idx_item_verifications_via
    ON item_verifications(via_item_id);
