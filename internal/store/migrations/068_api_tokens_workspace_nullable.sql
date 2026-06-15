-- BUG: creating a user-scoped (workspace-agnostic) API token via the account
-- settings page (POST /api/v1/auth/tokens -> handleCreateUserToken) fails on
-- SQLite with "NOT NULL constraint failed: api_tokens.workspace_id".
--
-- Root cause: 011_api_tokens.sql declared workspace_id NOT NULL, but
--   * the Postgres schema (pgmigrations/001_initial.sql) declares it nullable,
--   * the Go layer treats it as nullable end-to-end: models.APIToken,
--     store.CreateAPIToken inserts NULL for unscoped tokens, and getAPIToken /
--     ValidateToken scan workspace_id into *string.
-- So only SQLite deployments are affected. The account-settings path never
-- sets a workspace, so the INSERT always trips the constraint and the handler
-- returns a 500 ("An internal error occurred").
--
-- Fix: rebuild api_tokens with workspace_id nullable, matching Postgres and
-- the application contract. SQLite has no ALTER COLUMN ... DROP NOT NULL, so
-- the table is rebuilt via the standard recipe (cf. 055_collections_settings_
-- not_null.sql). Nothing references api_tokens(id) via a foreign key, so there
-- are no inbound references to recreate; foreign_keys is toggled OFF for the
-- rebuild per the established pattern (the migration runner applies an
-- foreign_keys=OFF pragma before the transaction and restores it after).

PRAGMA foreign_keys = OFF;

DROP TABLE IF EXISTS api_tokens_new;

CREATE TABLE api_tokens_new (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT,
    user_id      TEXT,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '["*"]',
    expires_at   TEXT,
    last_used_at TEXT,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id),
    FOREIGN KEY (user_id) REFERENCES users(id)
);

INSERT INTO api_tokens_new (
    id, workspace_id, user_id, name, token_hash, prefix, scopes,
    expires_at, last_used_at, created_at
)
SELECT
    id, workspace_id, user_id, name, token_hash, prefix, scopes,
    expires_at, last_used_at, created_at
FROM api_tokens;

DROP TABLE api_tokens;
ALTER TABLE api_tokens_new RENAME TO api_tokens;

PRAGMA foreign_keys = ON;
