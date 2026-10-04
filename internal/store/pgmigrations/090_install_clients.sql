-- Migration 090: confidential install clients and token bindings (SPEC-6 U5a,
-- TASK-3394). Mirrors SQLite migration 116; see it for the rationale.
ALTER TABLE oauth_clients ADD COLUMN IF NOT EXISTS client_secret_hash TEXT;
ALTER TABLE oauth_clients ADD COLUMN IF NOT EXISTS allowed_audiences JSONB;
ALTER TABLE oauth_clients ADD COLUMN IF NOT EXISTS app_install_id TEXT REFERENCES app_installs(id);
ALTER TABLE oauth_clients ADD COLUMN IF NOT EXISTS disabled_at TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_clients_app_install
    ON oauth_clients (app_install_id) WHERE app_install_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS app_token_bindings (
    request_id   TEXT PRIMARY KEY,
    client_id    TEXT NOT NULL REFERENCES oauth_clients(id),
    install_id   TEXT NOT NULL REFERENCES app_installs(id),
    workspace_id TEXT NOT NULL,
    auth_epoch   BIGINT NOT NULL,
    auth_kind    TEXT NOT NULL CHECK (auth_kind IN ('service', 'delegated')),
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_token_bindings_install ON app_token_bindings (install_id);
