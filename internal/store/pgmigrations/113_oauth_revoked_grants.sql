-- Migration 113: BUG-3678, a revoked OAuth grant by request_id. Mirrors SQLite
-- migration 143; see it for why the token rows alone cannot tell a revoked
-- grant from a refreshed one.
CREATE TABLE IF NOT EXISTS oauth_revoked_grants (
    request_id TEXT PRIMARY KEY,
    revoked_at TEXT NOT NULL
);
