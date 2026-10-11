-- Migration 110 (Postgres): idempotency keys for agent writes (TASK-3585). Counterpart to migrations/139_idempotency_keys.sql; the rationale is there.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    workspace_id TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    key          TEXT NOT NULL,
    route        TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    state        TEXT NOT NULL,
    claim_token  TEXT NOT NULL,
    lease_until  TEXT,
    status       INTEGER,
    response     TEXT,
    patch_id     TEXT,
    conn_id      TEXT,
    base_seq     INTEGER,
    base_body    TEXT,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (workspace_id, user_id, key)
);
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created ON idempotency_keys(created_at);
