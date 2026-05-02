-- Migration 027 (Postgres): OAuth 2.1 server tables.
--
-- Postgres counterpart to migrations/048_oauth.sql. The two files
-- carry the SAME schema with dialect-specific type tweaks:
--
--   - JSONB instead of TEXT for JSON-typed columns (faster + native
--     containment queries, in case future audit queries want to
--     introspect grant_types / redirect_uris arrays).
--   - BOOLEAN instead of INTEGER for true/false flags (matches the
--     pattern in pgmigrations/001_initial.sql for webhooks.active).
--   - (NOW() AT TIME ZONE 'UTC')::TEXT for default timestamps,
--     keeping the column TEXT so the cross-dialect ISO8601 contract
--     stays uniform on the Go side.
--   - REFERENCES <tab>(<col>) inline FK syntax.
--
-- Numbering note: Postgres migrations are at index 026 prior to
-- this; SQLite is at 047. The two histories are independent (the
-- runners scan their own dir + their own schema_migrations table)
-- so the asymmetric numbering is fine. See store.go:migrate vs
-- store.go:migratePostgres.
--
-- See migrations/048_oauth.sql for the full schema-level
-- documentation. Comments here are dialect-only.

-- ============================================================
-- 1. Registered OAuth clients (RFC 7591 DCR)
-- ============================================================
CREATE TABLE IF NOT EXISTS oauth_clients (
    id                          TEXT PRIMARY KEY,
    name                        TEXT NOT NULL,
    redirect_uris               JSONB NOT NULL DEFAULT '[]'::jsonb,
    grant_types                 JSONB NOT NULL DEFAULT '[]'::jsonb,
    response_types              JSONB NOT NULL DEFAULT '[]'::jsonb,
    token_endpoint_auth_method  TEXT NOT NULL DEFAULT 'none',
    scopes                      JSONB NOT NULL DEFAULT '[]'::jsonb,
    public                      BOOLEAN NOT NULL DEFAULT TRUE,
    logo_url                    TEXT,
    created_at                  TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT
);

CREATE INDEX IF NOT EXISTS oauth_clients_created_at_idx
    ON oauth_clients(created_at);

-- ============================================================
-- 2. Authorization codes
-- ============================================================
CREATE TABLE IF NOT EXISTS oauth_authorization_codes (
    signature         TEXT PRIMARY KEY,
    request_id        TEXT NOT NULL,
    requested_at      TEXT NOT NULL,
    client_id         TEXT NOT NULL REFERENCES oauth_clients(id),
    scopes            TEXT NOT NULL DEFAULT '',
    granted_scopes    TEXT NOT NULL DEFAULT '',
    request_form      TEXT NOT NULL DEFAULT '',
    session_data      TEXT NOT NULL DEFAULT '',
    audience          TEXT NOT NULL DEFAULT '',
    granted_audience  TEXT NOT NULL DEFAULT '',
    active            BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE INDEX IF NOT EXISTS oauth_codes_request_id_idx
    ON oauth_authorization_codes(request_id);
CREATE INDEX IF NOT EXISTS oauth_codes_requested_at_idx
    ON oauth_authorization_codes(requested_at);

-- ============================================================
-- 3. Access tokens
-- ============================================================
CREATE TABLE IF NOT EXISTS oauth_access_tokens (
    signature         TEXT PRIMARY KEY,
    request_id        TEXT NOT NULL,
    requested_at      TEXT NOT NULL,
    client_id         TEXT NOT NULL REFERENCES oauth_clients(id),
    scopes            TEXT NOT NULL DEFAULT '',
    granted_scopes    TEXT NOT NULL DEFAULT '',
    request_form      TEXT NOT NULL DEFAULT '',
    session_data      TEXT NOT NULL DEFAULT '',
    audience          TEXT NOT NULL DEFAULT '',
    granted_audience  TEXT NOT NULL DEFAULT '',
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    subject           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS oauth_access_request_id_idx
    ON oauth_access_tokens(request_id);
CREATE INDEX IF NOT EXISTS oauth_access_subject_idx
    ON oauth_access_tokens(subject);
CREATE INDEX IF NOT EXISTS oauth_access_requested_at_idx
    ON oauth_access_tokens(requested_at);

-- ============================================================
-- 4. Refresh tokens
-- ============================================================
CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
    signature               TEXT PRIMARY KEY,
    request_id              TEXT NOT NULL,
    access_token_signature  TEXT,
    requested_at            TEXT NOT NULL,
    client_id               TEXT NOT NULL REFERENCES oauth_clients(id),
    scopes                  TEXT NOT NULL DEFAULT '',
    granted_scopes          TEXT NOT NULL DEFAULT '',
    request_form            TEXT NOT NULL DEFAULT '',
    session_data            TEXT NOT NULL DEFAULT '',
    audience                TEXT NOT NULL DEFAULT '',
    granted_audience        TEXT NOT NULL DEFAULT '',
    active                  BOOLEAN NOT NULL DEFAULT TRUE,
    subject                 TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS oauth_refresh_request_id_idx
    ON oauth_refresh_tokens(request_id);
CREATE INDEX IF NOT EXISTS oauth_refresh_subject_idx
    ON oauth_refresh_tokens(subject);
CREATE INDEX IF NOT EXISTS oauth_refresh_requested_at_idx
    ON oauth_refresh_tokens(requested_at);

-- ============================================================
-- 5. PKCE request sessions
-- ============================================================
CREATE TABLE IF NOT EXISTS oauth_pkce_requests (
    signature         TEXT PRIMARY KEY,
    request_id        TEXT NOT NULL,
    requested_at      TEXT NOT NULL,
    client_id         TEXT NOT NULL REFERENCES oauth_clients(id),
    scopes            TEXT NOT NULL DEFAULT '',
    granted_scopes    TEXT NOT NULL DEFAULT '',
    request_form      TEXT NOT NULL DEFAULT '',
    session_data      TEXT NOT NULL DEFAULT '',
    audience          TEXT NOT NULL DEFAULT '',
    granted_audience  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS oauth_pkce_request_id_idx
    ON oauth_pkce_requests(request_id);
