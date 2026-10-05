-- Migration 099: app item actions and context codes (SPEC-6 U11, TASK-3414).
-- Mirrors SQLite migration 125; see it for the rationale.
CREATE TABLE IF NOT EXISTS app_item_actions (
    id              TEXT PRIMARY KEY,
    install_id      TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    action_key      TEXT NOT NULL,
    label           TEXT NOT NULL,
    path            TEXT NOT NULL,
    collection_ids  TEXT NOT NULL,
    revision        INTEGER NOT NULL DEFAULT 1,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (install_id, action_key)
);
CREATE TABLE IF NOT EXISTS app_context_codes (
    code_sha256     TEXT PRIMARY KEY,
    install_id      TEXT NOT NULL REFERENCES app_installs(id) ON DELETE CASCADE,
    auth_epoch      BIGINT NOT NULL,
    item_id         TEXT NOT NULL,
    action_id       TEXT NOT NULL,
    action_revision INTEGER NOT NULL,
    viewer_user_id  TEXT NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    consumed_at     TEXT
);
CREATE INDEX IF NOT EXISTS idx_app_context_codes_expiry ON app_context_codes (expires_at);
