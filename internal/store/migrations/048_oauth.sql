-- Migration 048: OAuth 2.1 server tables (PLAN-943 TASK-951 sub-PR A).
--
-- Five tables backing the OAuth authorization server defined in
-- PLAN-943 TASK-951: registered DCR clients, authorization codes,
-- access tokens, refresh tokens, PKCE request sessions. Schema
-- mirrors the storage interfaces fosite expects (handler/oauth2/storage.go,
-- handler/pkce/storage.go, client_manager.go in github.com/ory/fosite v0.49.0)
-- without importing fosite — this migration is pure SQL and the
-- storage layer in internal/store/oauth.go uses pad-internal types.
-- Sub-PR B wires fosite types over the top via adapter methods.
--
-- No table for token revocation: fosite's TokenRevocationStorage
-- semantics are satisfied by toggling each token row's `active` flag.
-- RevokeRefreshToken(request_id) walks the chain via the request_id
-- index (rotations preserve the originating Requester's ID, see
-- fosite handler/oauth2/flow_refresh.go:86).

-- ============================================================
-- 1. Registered OAuth clients (RFC 7591 Dynamic Client Registration)
-- ============================================================
--
-- Public clients only for v1 (Claude Desktop / Cursor / etc. — they
-- can't keep a secret). Confidential clients can be added later by
-- introducing a `client_secret` column; we keep `public` as a flag
-- now so the schema doesn't need to change.
CREATE TABLE IF NOT EXISTS oauth_clients (
    id                          TEXT PRIMARY KEY,             -- client_id (random ID, opaque)
    name                        TEXT NOT NULL,                -- human-readable, surfaced on consent screen
    redirect_uris               TEXT NOT NULL,                -- JSON array; OAuth 2.1 requires exact match
    grant_types                 TEXT NOT NULL,                -- JSON array, e.g. ["authorization_code","refresh_token"]
    response_types              TEXT NOT NULL,                -- JSON array, e.g. ["code"]
    token_endpoint_auth_method  TEXT NOT NULL DEFAULT 'none', -- "none" for public clients (PKCE-only)
    scopes                      TEXT NOT NULL DEFAULT '[]',   -- JSON array, the scopes this client is allowed to request
    public                      INTEGER NOT NULL DEFAULT 1,   -- 1=public (no secret), 0=confidential (future)
    logo_url                    TEXT,                         -- optional, surfaced on consent screen
    created_at                  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS oauth_clients_created_at_idx
    ON oauth_clients(created_at);

-- ============================================================
-- 2. Authorization codes (one per /authorize → /token exchange)
-- ============================================================
--
-- The `signature` is the HMAC-derived lookup key for the code;
-- the actual code value is never stored (so a DB read can't replay).
-- `request_form` is the URL-encoded /authorize query string; fosite
-- uses it to verify PKCE on /token exchange (the code_challenge is
-- in here). `session_data` is the marshalled session struct, which
-- pad will define in sub-PR B (subject = pad user ID, etc.).
CREATE TABLE IF NOT EXISTS oauth_authorization_codes (
    signature         TEXT PRIMARY KEY,            -- code's HMAC signature
    request_id        TEXT NOT NULL,               -- fosite Requester.GetID() — chain root for this grant
    requested_at      TEXT NOT NULL,               -- ISO8601
    client_id         TEXT NOT NULL,
    scopes            TEXT NOT NULL DEFAULT '',    -- space-separated, requested
    granted_scopes    TEXT NOT NULL DEFAULT '',    -- space-separated, after consent
    request_form      TEXT NOT NULL DEFAULT '',    -- URL-encoded form data
    session_data      TEXT NOT NULL DEFAULT '',    -- JSON-encoded session struct
    audience          TEXT NOT NULL DEFAULT '',    -- space-separated, requested (RFC 8707 resource= values)
    granted_audience  TEXT NOT NULL DEFAULT '',    -- space-separated, after binding
    active            INTEGER NOT NULL DEFAULT 1,  -- 0 once exchanged or invalidated
    FOREIGN KEY (client_id) REFERENCES oauth_clients(id)
);

CREATE INDEX IF NOT EXISTS oauth_codes_request_id_idx
    ON oauth_authorization_codes(request_id);
CREATE INDEX IF NOT EXISTS oauth_codes_requested_at_idx
    ON oauth_authorization_codes(requested_at);

-- ============================================================
-- 3. Access tokens (opaque HMAC, audience-bound per RFC 8707)
-- ============================================================
--
-- `subject` is denormalized from session_data so user-bound lookups
-- (audit, "list active tokens for user X") don't have to JSON-parse
-- every row. fosite doesn't query by subject itself; the column is
-- here for our admin / connected-apps surfaces (TASK-954).
CREATE TABLE IF NOT EXISTS oauth_access_tokens (
    signature         TEXT PRIMARY KEY,
    request_id        TEXT NOT NULL,               -- preserved across refresh rotations (chain identifier)
    requested_at      TEXT NOT NULL,
    client_id         TEXT NOT NULL,
    scopes            TEXT NOT NULL DEFAULT '',
    granted_scopes    TEXT NOT NULL DEFAULT '',
    request_form      TEXT NOT NULL DEFAULT '',
    session_data      TEXT NOT NULL DEFAULT '',
    audience          TEXT NOT NULL DEFAULT '',
    granted_audience  TEXT NOT NULL DEFAULT '',
    active            INTEGER NOT NULL DEFAULT 1,
    subject           TEXT NOT NULL DEFAULT '',    -- denormalized from session_data for fast subject-bound queries
    FOREIGN KEY (client_id) REFERENCES oauth_clients(id)
);

CREATE INDEX IF NOT EXISTS oauth_access_request_id_idx
    ON oauth_access_tokens(request_id);
CREATE INDEX IF NOT EXISTS oauth_access_subject_idx
    ON oauth_access_tokens(subject);
CREATE INDEX IF NOT EXISTS oauth_access_requested_at_idx
    ON oauth_access_tokens(requested_at);

-- ============================================================
-- 4. Refresh tokens (rotated single-use; theft-detection by family)
-- ============================================================
--
-- `access_token_signature` links the refresh to the access token it
-- was issued alongside. fosite passes both signatures into
-- CreateRefreshTokenSession so the storage can chain them, useful
-- when revoking a refresh token to also invalidate its sibling
-- access token.
--
-- `request_id` is the chain identifier for theft detection: every
-- token in a rotation chain (initial → rotated → rotated-again …)
-- shares the same request_id. RevokeRefreshToken(request_id) walks
-- this column and marks every chain member inactive — the "revoke
-- the whole family on replay" rule from the OAuth 2.1 BCP.
CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
    signature               TEXT PRIMARY KEY,
    request_id              TEXT NOT NULL,           -- chain root; family revocation walks this index
    access_token_signature  TEXT,                    -- nullable; links to oauth_access_tokens.signature
    requested_at            TEXT NOT NULL,
    client_id               TEXT NOT NULL,
    scopes                  TEXT NOT NULL DEFAULT '',
    granted_scopes          TEXT NOT NULL DEFAULT '',
    request_form            TEXT NOT NULL DEFAULT '',
    session_data            TEXT NOT NULL DEFAULT '',
    audience                TEXT NOT NULL DEFAULT '',
    granted_audience        TEXT NOT NULL DEFAULT '',
    active                  INTEGER NOT NULL DEFAULT 1,
    subject                 TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (client_id) REFERENCES oauth_clients(id)
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
--
-- fosite's PKCE handler stores the request alongside the auth code
-- (signature == auth code's signature) so the code_challenge from
-- /authorize can be verified against the code_verifier from /token.
-- Same shape as oauth_authorization_codes; separate table because
-- fosite uses a distinct interface (PKCERequestStorage) and the
-- lifecycle differs slightly — PKCE rows are deleted on /token
-- exchange success rather than flagged inactive.
CREATE TABLE IF NOT EXISTS oauth_pkce_requests (
    signature         TEXT PRIMARY KEY,
    request_id        TEXT NOT NULL,
    requested_at      TEXT NOT NULL,
    client_id         TEXT NOT NULL,
    scopes            TEXT NOT NULL DEFAULT '',
    granted_scopes    TEXT NOT NULL DEFAULT '',
    request_form      TEXT NOT NULL DEFAULT '',    -- contains code_challenge + code_challenge_method
    session_data      TEXT NOT NULL DEFAULT '',
    audience          TEXT NOT NULL DEFAULT '',
    granted_audience  TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (client_id) REFERENCES oauth_clients(id)
);

CREATE INDEX IF NOT EXISTS oauth_pkce_request_id_idx
    ON oauth_pkce_requests(request_id);
