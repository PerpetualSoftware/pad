-- Migration 149 (Postgres): pending provider links (TASK-4745).
-- An OAuth sign-in refused as not linked keeps the provider identity here for
-- ten minutes, behind a single-use token held as an HttpOnly cookie. The
-- next full sign-in of that account in that browser links it. Only the SHA-256 of the
-- token is stored.
CREATE TABLE IF NOT EXISTS oauth_pending_links (
    id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_pending_links_user ON oauth_pending_links(user_id);
