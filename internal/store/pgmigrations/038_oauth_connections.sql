-- Migration 038 (Postgres): per-OAuth-connection state (PLAN-1519 / TASK-1520).
--
-- Postgres counterpart to migrations/059_oauth_connections.sql. Same
-- schema, dialect-specific type tweaks:
--
--   - BOOLEAN instead of INTEGER for true/false flags (matches the
--     pattern in pgmigrations/027_oauth.sql for oauth_clients.public
--     and oauth_*_tokens.active).
--   - (NOW() AT TIME ZONE 'UTC')::TEXT for default timestamps, keeping
--     the column TEXT so the cross-dialect ISO8601 contract stays
--     uniform on the Go side (same convention as migration 027).
--
-- See migrations/059_oauth_connections.sql for the full schema-level
-- documentation. Comments here are dialect-only.

-- ============================================================
-- 1. oauth_connections
-- ============================================================
CREATE TABLE IF NOT EXISTS oauth_connections (
    request_id                  TEXT PRIMARY KEY,
    user_id                     TEXT NOT NULL,
    name                        TEXT NOT NULL DEFAULT '',
    may_create_workspaces       BOOLEAN NOT NULL DEFAULT TRUE,
    all_current_workspaces      BOOLEAN NOT NULL DEFAULT TRUE,
    include_future_workspaces   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at                  TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT,
    updated_at                  TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT
);

CREATE INDEX IF NOT EXISTS idx_oauth_connections_user
    ON oauth_connections(user_id);

-- ============================================================
-- 2. oauth_connection_workspaces
-- ============================================================
CREATE TABLE IF NOT EXISTS oauth_connection_workspaces (
    request_id    TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    added_at      TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT,
    added_by      TEXT NOT NULL DEFAULT 'user',
    PRIMARY KEY (request_id, workspace_id),
    FOREIGN KEY (request_id) REFERENCES oauth_connections(request_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_oauth_conn_workspaces_ws
    ON oauth_connection_workspaces(workspace_id);
