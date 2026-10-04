-- Migration 116: confidential install clients and token bindings (SPEC-6 U5a,
-- TASK-3394, DOC-3371 §4 Credentials).
--
-- Every installed app gets one confidential OAuth client. Unlike the public
-- DCR clients (PKCE only, no secret), it authenticates with a secret whose
-- hash is stored here (fosite's configured hasher), may only ever hold the
-- app API audience (allowed_audiences), and is tied to its install. A NULL
-- app_install_id is an ordinary (DCR) client, whose audiences stay the
-- server's MCP canonicals.
ALTER TABLE oauth_clients ADD COLUMN client_secret_hash TEXT;
ALTER TABLE oauth_clients ADD COLUMN allowed_audiences TEXT;
ALTER TABLE oauth_clients ADD COLUMN app_install_id TEXT REFERENCES app_installs(id);
ALTER TABLE oauth_clients ADD COLUMN disabled_at TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_clients_app_install
    ON oauth_clients (app_install_id) WHERE app_install_id IS NOT NULL;

-- One row per token family (fosite request_id) an install client holds,
-- written by the issuance barrier in the same transaction as the token, under
-- the install row's lock. auth_epoch is the install's epoch at issuance;
-- introspection refuses a binding whose epoch is not the install's current
-- one, so a disable or rotate (which bumps the epoch) ends every token issued
-- before it, including any that raced it.
CREATE TABLE IF NOT EXISTS app_token_bindings (
    request_id   TEXT PRIMARY KEY,
    client_id    TEXT NOT NULL REFERENCES oauth_clients(id),
    install_id   TEXT NOT NULL REFERENCES app_installs(id),
    workspace_id TEXT NOT NULL,
    auth_epoch   INTEGER NOT NULL,
    auth_kind    TEXT NOT NULL CHECK (auth_kind IN ('service', 'delegated')),
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_app_token_bindings_install ON app_token_bindings (install_id);
