-- Migration 139: idempotency keys for agent writes (TASK-3585).
--
-- A caller that sends a key (Idempotency-Key header, or op_key) gets the
-- original answer back on a retry instead of a second write. One row per
-- (workspace, user, key). The life of a row:
--   pending  claimed, with claim_token and lease_until. The write's own
--            transaction moves it to done, guarded by the token, so `done`
--            holds exactly when the write committed. A pending row whose
--            lease expired therefore means nothing committed, and a retry
--            may take it over with a new token.
--   done     status + response hold the answer to replay.
--   unknown  an open editor received a patch and never confirmed it
--            (patch_id, conn_id, base_seq, base_body record what was sent);
--            only that same connection may resolve it.
-- request_hash (route + canonical request) refuses a key reused for a
-- different request. Swept 24h after created_at. Not exported.
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
